package com.backspinlabs.tickwarden.companion;

import com.sun.net.httpserver.HttpServer;
import net.fabricmc.api.DedicatedServerModInitializer;
import net.fabricmc.fabric.api.event.lifecycle.v1.ServerLifecycleEvents;
import net.fabricmc.fabric.api.event.lifecycle.v1.ServerTickEvents;
import net.minecraft.server.level.ChunkHolder;
import net.minecraft.server.level.ServerLevel;
import net.minecraft.world.level.ChunkPos;
import net.minecraft.world.level.chunk.LevelChunk;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;

/**
 * tickwarden companion — the one thing tickwarden can't read from outside the
 * JVM: the server's real tick health.
 *
 * <p>It times every server tick (END − START gives the work done that tick, the
 * same "MSPT" spark reports) over a rolling window and serves the rolling TPS
 * and MSPT as JSON on a localhost-only HTTP endpoint. tickwarden's watch loop
 * (running in the same container) polls it and correlates the numbers with
 * cgroup pressure to decide whether a dip is the game's fault or the host's.
 *
 * <p>spark can't fill this role here: its commands reply asynchronously and come
 * back empty over RCON, so there's no scriptable way to pull TPS out of it.
 *
 * <p>Port: {@code -Dtickwarden.port=9225} or {@code TICKWARDEN_PORT}; default 9225.
 * Binds 127.0.0.1 only — it is not a public endpoint.
 */
public class TickwardenCompanion implements DedicatedServerModInitializer {

    private static final String TICK_TARGET_MS = "50"; // 20 TPS = 50 ms/tick
    private static final int WINDOW = 100;              // ~5 s of ticks
    private static final int DEFAULT_PORT = 9225;

    // Scanning every loaded chunk each tick would itself be a lag source — the
    // exact thing this endpoint exists to find. Sample on the same cadence as
    // the TPS window and keep only the worst offenders.
    private static final int HOTSPOT_SAMPLE_EVERY = WINDOW; // ~5 s
    private static final int HOTSPOT_TOP_N = 10;

    private final long[] durationsNanos = new long[WINDOW];
    private int idx = 0;
    private int count = 0;
    private long tickStartNanos = 0;

    // Read by the HTTP handler thread; written by the server thread.
    private volatile double mspt = 0.0;
    private volatile double tps = 20.0;
    // Player load — tickwarden uses the peak to size view/simulation distance to
    // the server's ACTUAL load instead of a guess. The peak is PERSISTED across
    // restarts (a fresh restart would otherwise read 0 and make the tuner
    // over-aggressive until players happen to log in again).
    private volatile int players = 0;
    private volatile int playersPeak = 0;
    // Where the peak survives a restart. Relative path resolves to the server's
    // working directory (e.g. /opt/minecraft); a plain integer, easy to inspect.
    private static final Path PEAK_FILE = Path.of("tickwarden-players-peak.txt");

    // Rolling snapshot of the top loaded chunks by block-entity count. Written
    // by the server thread on the sample tick, read by the HTTP handler thread.
    // Immutable list behind a volatile ref — same publish pattern as the gauges
    // above, so the reader never sees a half-built list.
    private volatile List<Hotspot> hotspots = Collections.emptyList();
    private long tickCounter = 0;

    private HttpServer http;

    /** One loaded chunk and the tickable load it carries. Chunk coordinates. */
    private record Hotspot(String dimension, int x, int z, int blockEntities) {
    }

    @Override
    public void onInitializeServer() {
        ServerTickEvents.START_SERVER_TICK.register(server -> tickStartNanos = System.nanoTime());
        ServerTickEvents.END_SERVER_TICK.register(server -> {
            recordTick(System.nanoTime(), server.getPlayerCount());
            if (++tickCounter % HOTSPOT_SAMPLE_EVERY == 0) {
                sampleHotspots(server);
            }
        });
        ServerLifecycleEvents.SERVER_STARTED.register(server -> {
            loadPeak();
            startHttp();
        });
        ServerLifecycleEvents.SERVER_STOPPING.register(server -> stopHttp());
    }

    private synchronized void recordTick(long endNanos, int playerCount) {
        players = playerCount;
        if (playerCount > playersPeak) {
            playersPeak = playerCount;
            savePeak(); // only on a new high-water mark — writes are rare
        }
        if (tickStartNanos == 0) {
            return;
        }
        durationsNanos[idx] = endNanos - tickStartNanos;
        idx = (idx + 1) % WINDOW;
        if (count < WINDOW) {
            count++;
        }

        long sum = 0;
        for (int i = 0; i < count; i++) {
            sum += durationsNanos[i];
        }
        double avgMs = (sum / (double) count) / 1_000_000.0;
        mspt = avgMs;
        // Below the 50 ms budget the server holds 20 TPS; above it, TPS is the
        // rate the work actually allows. Same approximation spark surfaces.
        tps = Math.min(20.0, 1000.0 / Math.max(Double.parseDouble(TICK_TARGET_MS), avgMs));
    }

    /**
     * Walk every loaded ticking chunk in every dimension, count its block
     * entities (hoppers, chests, furnaces — the per-tick cost spark can't tie to
     * a place), and publish the worst {@value #HOTSPOT_TOP_N}. Runs on the server
     * thread inside END_SERVER_TICK, so {@code getChunks()} is safe to touch.
     */
    private void sampleHotspots(net.minecraft.server.MinecraftServer server) {
        List<Hotspot> found = new ArrayList<>();
        for (ServerLevel level : server.getAllLevels()) {
            String dim = level.dimension().location().toString();
            for (ChunkHolder holder : level.getChunkSource().chunkMap.getChunks()) {
                LevelChunk chunk = holder.getTickingChunk();
                if (chunk == null) {
                    continue; // not at ticking level; nothing tickable here
                }
                int be = chunk.getBlockEntities().size();
                if (be == 0) {
                    continue;
                }
                ChunkPos pos = holder.getPos();
                found.add(new Hotspot(dim, pos.x, pos.z, be));
            }
        }
        found.sort((a, b) -> Integer.compare(b.blockEntities(), a.blockEntities()));
        List<Hotspot> top = found.size() > HOTSPOT_TOP_N
                ? new ArrayList<>(found.subList(0, HOTSPOT_TOP_N))
                : found;
        hotspots = Collections.unmodifiableList(top);
    }

    /** Restore the persisted peak so a restart doesn't reset it to 0. */
    private void loadPeak() {
        try {
            if (Files.exists(PEAK_FILE)) {
                int saved = Integer.parseInt(Files.readString(PEAK_FILE).trim());
                if (saved > playersPeak) {
                    playersPeak = saved;
                }
            }
        } catch (IOException | NumberFormatException e) {
            System.err.println("[tickwarden-companion] couldn't read " + PEAK_FILE + ": " + e.getMessage());
        }
    }

    /** Persist the new high-water mark. Called only when the peak rises. */
    private void savePeak() {
        try {
            Files.writeString(PEAK_FILE, Integer.toString(playersPeak));
        } catch (IOException e) {
            System.err.println("[tickwarden-companion] couldn't write " + PEAK_FILE + ": " + e.getMessage());
        }
    }

    private int port() {
        String raw = System.getProperty("tickwarden.port");
        if (raw == null) {
            raw = System.getenv("TICKWARDEN_PORT");
        }
        if (raw != null) {
            try {
                return Integer.parseInt(raw.trim());
            } catch (NumberFormatException ignored) {
                // fall through to default
            }
        }
        return DEFAULT_PORT;
    }

    private void startHttp() {
        int port = port();
        try {
            http = HttpServer.create(new InetSocketAddress("127.0.0.1", port), 0);
            http.createContext("/tps", exchange -> {
                String json = String.format(Locale.ROOT,
                        "{\"tps\":%.2f,\"mspt\":%.2f,\"players\":%d,\"players_peak\":%d}",
                        tps, mspt, players, playersPeak);
                byte[] body = json.getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().set("Content-Type", "application/json");
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream os = exchange.getResponseBody()) {
                    os.write(body);
                }
            });
            http.createContext("/hotspots", exchange -> {
                byte[] body = hotspotsJson().getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().set("Content-Type", "application/json");
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream os = exchange.getResponseBody()) {
                    os.write(body);
                }
            });
            http.setExecutor(null);
            http.start();
            System.out.println("[tickwarden-companion] TPS endpoint up on http://127.0.0.1:" + port + "/tps");
            System.out.println("[tickwarden-companion] hotspots endpoint up on http://127.0.0.1:" + port + "/hotspots");
        } catch (IOException e) {
            System.err.println("[tickwarden-companion] failed to start HTTP endpoint on port " + port + ": " + e.getMessage());
        }
    }

    /**
     * Serialize the current snapshot as a JSON array. Keys are snake_case and
     * MUST match what internal/hotspots decodes ({@code dimension}, {@code x},
     * {@code z}, {@code block_entities}); a mismatch decodes silently to zero.
     */
    private String hotspotsJson() {
        List<Hotspot> snap = hotspots; // single volatile read
        StringBuilder sb = new StringBuilder();
        sb.append('[');
        for (int i = 0; i < snap.size(); i++) {
            Hotspot h = snap.get(i);
            if (i > 0) {
                sb.append(',');
            }
            sb.append(String.format(Locale.ROOT,
                    "{\"dimension\":\"%s\",\"x\":%d,\"z\":%d,\"block_entities\":%d}",
                    escape(h.dimension()), h.x(), h.z(), h.blockEntities()));
        }
        sb.append(']');
        return sb.toString();
    }

    private static String escape(String s) {
        return s.replace("\\", "\\\\").replace("\"", "\\\"");
    }

    private void stopHttp() {
        if (http != null) {
            http.stop(0);
            http = null;
        }
    }
}

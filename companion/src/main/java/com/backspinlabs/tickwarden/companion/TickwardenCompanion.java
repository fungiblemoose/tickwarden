package com.backspinlabs.tickwarden.companion;

import com.sun.net.httpserver.HttpServer;
import net.fabricmc.api.DedicatedServerModInitializer;
import net.fabricmc.fabric.api.event.lifecycle.v1.ServerLifecycleEvents;
import net.fabricmc.fabric.api.event.lifecycle.v1.ServerTickEvents;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
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

    private final long[] durationsNanos = new long[WINDOW];
    private int idx = 0;
    private int count = 0;
    private long tickStartNanos = 0;

    // Read by the HTTP handler thread; written by the server thread.
    private volatile double mspt = 0.0;
    private volatile double tps = 20.0;
    // Player load — tickwarden uses the peak to size view/simulation distance to
    // the server's ACTUAL load instead of a guess. peak is the max simultaneous
    // count seen since startup (resets on restart; persisting it is a TODO).
    private volatile int players = 0;
    private volatile int playersPeak = 0;

    private HttpServer http;

    @Override
    public void onInitializeServer() {
        ServerTickEvents.START_SERVER_TICK.register(server -> tickStartNanos = System.nanoTime());
        ServerTickEvents.END_SERVER_TICK.register(server -> recordTick(System.nanoTime(), server.getPlayerCount()));
        ServerLifecycleEvents.SERVER_STARTED.register(server -> startHttp());
        ServerLifecycleEvents.SERVER_STOPPING.register(server -> stopHttp());
    }

    private synchronized void recordTick(long endNanos, int playerCount) {
        players = playerCount;
        if (playerCount > playersPeak) {
            playersPeak = playerCount;
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
            http.setExecutor(null);
            http.start();
            System.out.println("[tickwarden-companion] TPS endpoint up on http://127.0.0.1:" + port + "/tps");
        } catch (IOException e) {
            System.err.println("[tickwarden-companion] failed to start HTTP endpoint on port " + port + ": " + e.getMessage());
        }
    }

    private void stopHttp() {
        if (http != null) {
            http.stop(0);
            http = null;
        }
    }
}

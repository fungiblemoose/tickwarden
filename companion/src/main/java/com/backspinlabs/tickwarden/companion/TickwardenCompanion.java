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
import java.lang.management.GarbageCollectorMXBean;
import java.lang.management.ManagementFactory;
import java.lang.management.MemoryUsage;
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

    /**
     * The marketable face of the companion: a single self-contained status page.
     * No external CDNs, fonts, or scripts — everything (markup, CSS, JS) lives in
     * this one string so it works on an air-gapped server box. It polls the
     * existing {@code /tps} and {@code /hotspots} JSON from the browser every ~2 s,
     * so rendering it costs the game server nothing per tick. Fetches are relative
     * so the page is port-independent.
     */
    private static final String STATUS_PAGE_HTML = """
            <!DOCTYPE html>
            <html lang="en">
            <head>
            <meta charset="utf-8">
            <meta name="viewport" content="width=device-width, initial-scale=1">
            <title>tickwarden</title>
            <style>
              :root {
                --bg: #0b0e11;
                --panel: #14181d;
                --panel2: #1b2026;
                --line: #232a31;
                --text: #e6edf3;
                --muted: #8b97a3;
                --green: #3fb950;
                --amber: #d9a441;
                --red: #f0566a;
                --mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace;
              }
              * { box-sizing: border-box; }
              html, body { margin: 0; height: 100%; }
              body {
                background: radial-gradient(1200px 600px at 70% -10%, #11161c 0%, var(--bg) 60%);
                color: var(--text);
                font-family: var(--mono);
                -webkit-font-smoothing: antialiased;
                padding: 28px 18px 48px;
              }
              .wrap { max-width: 860px; margin: 0 auto; }
              header {
                display: flex; align-items: baseline; gap: 12px;
                border-bottom: 1px solid var(--line); padding-bottom: 14px; margin-bottom: 22px;
              }
              .brand { font-size: 20px; font-weight: 700; letter-spacing: 0.04em; }
              .brand .dot { color: var(--green); }
              .sub { color: var(--muted); font-size: 12px; }
              .live { margin-left: auto; color: var(--muted); font-size: 12px; display: flex; align-items: center; gap: 7px; }
              .pulse { width: 8px; height: 8px; border-radius: 50%; background: var(--green); box-shadow: 0 0 0 0 rgba(63,185,80,0.6); animation: pulse 2s infinite; }
              .pulse.stale { background: var(--red); animation: none; }
              @keyframes pulse { 0% { box-shadow: 0 0 0 0 rgba(63,185,80,0.5);} 70% { box-shadow: 0 0 0 7px rgba(63,185,80,0);} 100% { box-shadow: 0 0 0 0 rgba(63,185,80,0);} }
              .grid { display: grid; grid-template-columns: 1.3fr 1fr; gap: 16px; }
              @media (max-width: 640px) { .grid { grid-template-columns: 1fr; } }
              .card {
                background: linear-gradient(180deg, var(--panel) 0%, var(--panel2) 100%);
                border: 1px solid var(--line); border-radius: 12px; padding: 18px 20px;
              }
              .label { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: 0.12em; margin-bottom: 6px; }
              .tps-card { display: flex; flex-direction: column; justify-content: center; }
              .tps { font-size: 76px; font-weight: 800; line-height: 1; letter-spacing: -0.02em; }
              .tps.green { color: var(--green); } .tps.amber { color: var(--amber); } .tps.red { color: var(--red); }
              .tps-unit { font-size: 22px; color: var(--muted); font-weight: 600; margin-left: 6px; }
              .mspt-row { margin-top: 18px; }
              .mspt-val { font-size: 15px; }
              .bar { height: 10px; background: #0a0d10; border: 1px solid var(--line); border-radius: 6px; overflow: hidden; margin-top: 8px; position: relative; }
              .bar-fill { height: 100%; width: 0%; background: var(--green); transition: width .4s ease, background .4s ease; }
              .bar-budget { position: absolute; top: -3px; bottom: -3px; left: 100%; width: 2px; background: var(--muted); opacity: .4; }
              .stats { display: grid; grid-template-columns: 1fr 1fr; gap: 14px 18px; }
              .stat .v { font-size: 26px; font-weight: 700; }
              .stat .v small { font-size: 14px; color: var(--muted); font-weight: 500; }
              table { width: 100%; border-collapse: collapse; font-size: 13px; margin-top: 4px; }
              th { text-align: left; color: var(--muted); font-weight: 600; font-size: 11px; text-transform: uppercase; letter-spacing: 0.08em; padding: 6px 8px; border-bottom: 1px solid var(--line); }
              td { padding: 7px 8px; border-bottom: 1px solid #1a2026; }
              td.num { text-align: right; }
              .be { color: var(--amber); font-weight: 700; }
              tr:last-child td { border-bottom: 0; }
              .empty { color: var(--muted); padding: 14px 8px; font-size: 13px; }
              .full { grid-column: 1 / -1; }
              footer { color: var(--muted); font-size: 11px; margin-top: 22px; text-align: center; }
            </style>
            </head>
            <body>
            <div class="wrap">
              <header>
                <div class="brand">tick<span class="dot">warden</span></div>
                <div class="sub">server tick health</div>
                <div class="live"><span class="pulse" id="pulse"></span><span id="livetext">live</span></div>
              </header>

              <div class="grid">
                <div class="card tps-card">
                  <div class="label">ticks per second</div>
                  <div><span class="tps green" id="tps">--</span><span class="tps-unit">TPS</span></div>
                  <div class="mspt-row">
                    <div class="label">ms / tick &mdash; 50ms budget</div>
                    <div class="mspt-val"><span id="mspt">--</span> ms</div>
                    <div class="bar"><div class="bar-fill" id="msptbar"></div><div class="bar-budget"></div></div>
                  </div>
                </div>

                <div class="card">
                  <div class="label">load</div>
                  <div class="stats">
                    <div class="stat"><div class="label">players</div><div class="v"><span id="players">--</span><small> / <span id="peak">--</span> peak</small></div></div>
                    <div class="stat"><div class="label">view dist</div><div class="v"><span id="view">--</span></div></div>
                    <div class="stat"><div class="label">sim dist</div><div class="v"><span id="sim">--</span></div></div>
                    <div class="stat"><div class="label">status</div><div class="v"><span id="health" style="font-size:18px">--</span></div></div>
                  </div>
                </div>

                <div class="card full">
                  <div class="label">hotspots &mdash; top loaded chunks by block entities</div>
                  <table>
                    <thead><tr><th>dimension</th><th>chunk x</th><th>chunk z</th><th class="num">block entities</th></tr></thead>
                    <tbody id="hotspots"><tr><td class="empty" colspan="4">waiting for first sample&hellip;</td></tr></tbody>
                  </table>
                </div>
              </div>

              <footer>tickwarden companion &middot; polling 127.0.0.1 every 2s &middot; localhost only</footer>
            </div>

            <script>
              const POLL_MS = 2000;
              const fmt = (n, d) => (n === null || n === undefined || Number.isNaN(n)) ? '--' : Number(n).toFixed(d);
              const dist = (v) => (v === undefined || v === null || v < 0) ? '--' : v;

              function setLive(ok) {
                const p = document.getElementById('pulse');
                const t = document.getElementById('livetext');
                if (ok) { p.classList.remove('stale'); t.textContent = 'live'; }
                else { p.classList.add('stale'); t.textContent = 'no signal'; }
              }

              async function poll() {
                try {
                  const [tpsRes, hotRes] = await Promise.all([fetch('/tps'), fetch('/hotspots')]);
                  const d = await tpsRes.json();
                  const hot = await hotRes.json();

                  const tpsEl = document.getElementById('tps');
                  const tps = d.tps;
                  tpsEl.textContent = fmt(tps, 1);
                  tpsEl.classList.remove('green', 'amber', 'red');
                  let cls = 'green', health = 'healthy';
                  if (tps < 15) { cls = 'red'; health = 'struggling'; }
                  else if (tps < 19.5) { cls = 'amber'; health = 'strained'; }
                  tpsEl.classList.add(cls);

                  const healthEl = document.getElementById('health');
                  healthEl.textContent = health;
                  healthEl.style.color = cls === 'green' ? 'var(--green)' : cls === 'amber' ? 'var(--amber)' : 'var(--red)';

                  document.getElementById('mspt').textContent = fmt(d.mspt, 1);
                  const pct = Math.min(100, Math.max(0, (d.mspt / 50) * 100));
                  const bar = document.getElementById('msptbar');
                  bar.style.width = pct + '%';
                  bar.style.background = pct < 70 ? 'var(--green)' : pct < 100 ? 'var(--amber)' : 'var(--red)';

                  document.getElementById('players').textContent = (d.players ?? '--');
                  document.getElementById('peak').textContent = (d.players_peak ?? '--');
                  document.getElementById('view').textContent = dist(d.view);
                  document.getElementById('sim').textContent = dist(d.sim);

                  const tbody = document.getElementById('hotspots');
                  if (Array.isArray(hot) && hot.length) {
                    tbody.innerHTML = '';
                    for (const h of hot) {
                      const tr = document.createElement('tr');
                      const dimName = String(h.dimension).replace('minecraft:', '');
                      tr.innerHTML = '<td>' + dimName + '</td><td>' + h.x + '</td><td>' + h.z +
                                     '</td><td class="num be">' + h.block_entities + '</td>';
                      tbody.appendChild(tr);
                    }
                  } else {
                    tbody.innerHTML = '<tr><td class="empty" colspan="4">no notable hotspots</td></tr>';
                  }
                  setLive(true);
                } catch (e) {
                  setLive(false);
                }
              }

              poll();
              setInterval(poll, POLL_MS);
            </script>
            </body>
            </html>
            """;

    // The running server, captured at start so the HTTP handlers can read and
    // (on the server thread) change view/simulation distance at runtime — the
    // hook the adaptive controller drives. Volatile: written by the server
    // thread, read by the HTTP handler thread.
    private volatile net.minecraft.server.MinecraftServer mcServer;

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
            mcServer = server;
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

    /** Parse an int query parameter (e.g. "sim" from "sim=10&view=14"); null if absent/bad. */
    private static Integer paramInt(String query, String key) {
        if (query == null) {
            return null;
        }
        for (String pair : query.split("&")) {
            int eq = pair.indexOf('=');
            if (eq > 0 && pair.substring(0, eq).equals(key)) {
                try {
                    return Integer.parseInt(pair.substring(eq + 1).trim());
                } catch (NumberFormatException e) {
                    return null;
                }
            }
        }
        return null;
    }

    /** Clamp a distance to Minecraft's sane range so a bad request can't wedge the server. */
    private static int clampDist(int d) {
        if (d < 2) {
            return 2;
        }
        if (d > 32) {
            return 32;
        }
        return d;
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
            // Root: a self-contained status page (no external deps). It polls the
            // JSON endpoints below from the browser, so it adds zero per-tick
            // server work. "/" is a catch-all prefix; serve the page only for the
            // exact root and 404 anything else so /favicon.ico etc. stay clean.
            http.createContext("/", exchange -> {
                if (!exchange.getRequestURI().getPath().equals("/")) {
                    exchange.sendResponseHeaders(404, -1);
                    exchange.close();
                    return;
                }
                byte[] body = STATUS_PAGE_HTML.getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().set("Content-Type", "text/html; charset=utf-8");
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream os = exchange.getResponseBody()) {
                    os.write(body);
                }
            });
            http.createContext("/tps", exchange -> {
                int view = -1, sim = -1;
                var srv = mcServer;
                if (srv != null) {
                    view = srv.getPlayerList().getViewDistance();
                    sim = srv.getPlayerList().getSimulationDistance();
                }
                String json = String.format(Locale.ROOT,
                        "{\"tps\":%.2f,\"mspt\":%.2f,\"players\":%d,\"players_peak\":%d,\"view\":%d,\"sim\":%d}",
                        tps, mspt, players, playersPeak, view, sim);
                byte[] body = json.getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().set("Content-Type", "application/json");
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream os = exchange.getResponseBody()) {
                    os.write(body);
                }
            });
            // Runtime distance setter — the engine supports changing view/sim
            // distance live (vanilla just has no command). Must run on the server
            // thread, so we hand it to srv.execute(). Query: /setdistance?sim=10&view=14
            http.createContext("/setdistance", exchange -> {
                String q = exchange.getRequestURI().getRawQuery();
                Integer sim = paramInt(q, "sim");
                Integer view = paramInt(q, "view");
                final var srv = mcServer;
                String resp;
                if (srv == null) {
                    resp = "{\"ok\":false,\"error\":\"server not ready\"}";
                } else {
                    srv.execute(() -> {
                        var pl = srv.getPlayerList();
                        if (view != null) {
                            pl.setViewDistance(clampDist(view));
                        }
                        if (sim != null) {
                            pl.setSimulationDistance(clampDist(sim));
                        }
                    });
                    resp = String.format(Locale.ROOT, "{\"ok\":true,\"sim\":%s,\"view\":%s}",
                            sim == null ? "null" : sim.toString(), view == null ? "null" : view.toString());
                }
                byte[] body = resp.getBytes(StandardCharsets.UTF_8);
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
            // JVM telemetry — heap occupancy and GC counters. A multi-hundred-ms
            // GC pause is a multi-tick freeze that looks identical to world load
            // in the MSPT number; exposing the cumulative GC counters lets the
            // tickwarden daemon diff them between polls and attribute a spike to
            // the collector instead of cutting distance for it. MXBean reads are
            // thread-safe and cost nothing per tick, so this runs entirely on the
            // HTTP handler thread.
            http.createContext("/jvm", exchange -> {
                byte[] body = jvmJson().getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().set("Content-Type", "application/json");
                exchange.sendResponseHeaders(200, body.length);
                try (OutputStream os = exchange.getResponseBody()) {
                    os.write(body);
                }
            });
            http.setExecutor(null);
            http.start();
            System.out.println("[tickwarden-companion] status page up on http://127.0.0.1:" + port + "/");
            System.out.println("[tickwarden-companion] TPS endpoint up on http://127.0.0.1:" + port + "/tps");
            System.out.println("[tickwarden-companion] hotspots endpoint up on http://127.0.0.1:" + port + "/hotspots");
            System.out.println("[tickwarden-companion] JVM endpoint up on http://127.0.0.1:" + port + "/jvm");
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

    /**
     * Serialize heap usage and per-collector GC counters. The counts and times
     * are CUMULATIVE since JVM start (that's what the MXBeans expose); consumers
     * must diff successive reads — {@code gc_count}/{@code gc_time_ms} totals are
     * included so a consumer only needs to track one pair. Keys are snake_case
     * and MUST match what internal/observe decodes.
     */
    private String jvmJson() {
        MemoryUsage heap = ManagementFactory.getMemoryMXBean().getHeapMemoryUsage();
        StringBuilder sb = new StringBuilder();
        sb.append(String.format(Locale.ROOT,
                "{\"heap_used\":%d,\"heap_committed\":%d,\"heap_max\":%d,\"gc\":[",
                heap.getUsed(), heap.getCommitted(), heap.getMax()));
        long totalCount = 0, totalTimeMs = 0;
        boolean first = true;
        for (GarbageCollectorMXBean gc : ManagementFactory.getGarbageCollectorMXBeans()) {
            long count = gc.getCollectionCount();
            long timeMs = gc.getCollectionTime();
            if (count < 0) {
                continue; // collector doesn't report; skip rather than lie with -1
            }
            if (!first) {
                sb.append(',');
            }
            first = false;
            sb.append(String.format(Locale.ROOT,
                    "{\"name\":\"%s\",\"count\":%d,\"time_ms\":%d}",
                    escape(gc.getName()), count, Math.max(0, timeMs)));
            totalCount += count;
            totalTimeMs += Math.max(0, timeMs);
        }
        sb.append(String.format(Locale.ROOT,
                "],\"gc_count\":%d,\"gc_time_ms\":%d}", totalCount, totalTimeMs));
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

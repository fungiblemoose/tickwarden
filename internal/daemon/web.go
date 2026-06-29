package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// This file is the daemon's web control plane: a status dashboard, the
// decision history, and live knob changes — the parts of running the adaptive
// controller that used to require reading journald and editing flags.
//
// It deliberately lives on the DAEMON, not the companion. The companion runs
// inside the game JVM where every feature costs tick time and widens the
// attack surface of the server itself; the daemon is a separate process that
// already owns the knobs and (unlike the companion) can see cgroup pressure.
// The companion's own page stays a read-only status display.
//
// Security posture: off by default (empty UIAddr). Loopback binds need no
// token, matching the companion, but are still defended: the Host header is
// pinned to the loopback bind (DNS-rebinding defense) and every mutating call
// must carry the custom X-Tickwarden-Token header (CSRF defense — a simple
// cross-site request can't set it). A non-loopback bind is REFUSED unless a
// token is set, because /api/config changes a live game server. Settings
// changed here are runtime-only — tickwarden.toml remains the durable source.

// serveUI validates the bind address and starts the control plane in a
// goroutine that shuts down with ctx. Returns an error only for
// misconfiguration (bad address, non-loopback without a token).
func serveUI(ctx context.Context, cfg Config, store *Store, log *slog.Logger) error {
	host, _, err := net.SplitHostPort(cfg.UIAddr)
	if err != nil {
		return fmt.Errorf("ui: invalid -ui address %q: %w", cfg.UIAddr, err)
	}
	if !isLoopbackHost(host) && cfg.UIToken == "" {
		return fmt.Errorf("ui: refusing to bind %q without a token — /api/config changes a live server; set -ui-token or bind 127.0.0.1", cfg.UIAddr)
	}

	srv := &http.Server{
		Addr:              cfg.UIAddr,
		Handler:           requireHost(cfg.UIAddr, uiHandler(store, cfg.UIToken, log)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.UIAddr)
	if err != nil {
		return fmt.Errorf("ui: listen %s: %w", cfg.UIAddr, err)
	}
	log.Info("control plane up", "addr", "http://"+cfg.UIAddr+"/")

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Warn("control plane stopped", "err", err.Error())
		}
	}()
	return nil
}

// isLoopbackHost reports whether the bind host stays on this machine. An
// empty host (":9226") binds every interface — NOT loopback.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireHost pins the Host header to the loopback bind — a DNS-rebinding
// defense. A malicious page can point its own domain at 127.0.0.1, but the
// browser still sends that domain in Host, so requiring a loopback Host with
// the bound port blocks it while still allowing both "localhost" and
// "127.0.0.1". A deliberately public bind is token-guarded and reachable under
// many names, so Host pinning only applies to the loopback default.
func requireHost(addr string, next http.Handler) http.Handler {
	bindHost, bindPort, err := net.SplitHostPort(addr)
	if err != nil || !isLoopbackHost(bindHost) {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqHost, reqPort, err := net.SplitHostPort(r.Host)
		if err != nil || reqPort != bindPort || !isLoopbackHost(reqHost) {
			http.Error(w, "bad Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// uiHandler builds the control plane's routes. Exported-ish seam (used by
// tests via httptest) — everything it needs comes in as arguments.
func uiHandler(store *Store, token string, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(controlPageHTML))
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"status": store.Status(),
			"knobs":  store.Knobs(),
		})
	})

	mux.HandleFunc("/api/decisions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, store.Decisions())
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// CSRF defense, enforced even on a tokenless loopback bind: a custom
		// header that a simple cross-site form/img/script request cannot set
		// must be present, and the body must be JSON (not a form post). When a
		// token is configured, requireToken has already checked this header's
		// value; here we only insist that mutations carry it.
		if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(mt) != "application/json" {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		if r.Header.Get("X-Tickwarden-Token") == "" {
			http.Error(w, "missing X-Tickwarden-Token header", http.StatusForbidden)
			return
		}
		// Partial update: only the fields present in the body change. Pointers
		// distinguish "absent" from "zero" so {"apply":false} works.
		var patch struct {
			TargetMSPT     *float64 `json:"target_mspt"`
			MinSim         *int     `json:"min_sim"`
			MaxSim         *int     `json:"max_sim"`
			MaxView        *int     `json:"max_view"`
			StarvePSI      *float64 `json:"starve_psi"`
			GCStallPct     *float64 `json:"gc_stall_pct"`
			MemPressurePct *float64 `json:"mem_pressure_pct"`
			Apply          *bool    `json:"apply"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		k := store.Knobs()
		if patch.TargetMSPT != nil {
			k.TargetMSPT = *patch.TargetMSPT
		}
		if patch.MinSim != nil {
			k.MinSim = *patch.MinSim
		}
		if patch.MaxSim != nil {
			k.MaxSim = *patch.MaxSim
		}
		if patch.MaxView != nil {
			k.MaxView = *patch.MaxView
		}
		if patch.StarvePSI != nil {
			k.StarvePSI = *patch.StarvePSI
		}
		if patch.GCStallPct != nil {
			k.GCStallPct = *patch.GCStallPct
		}
		if patch.MemPressurePct != nil {
			k.MemPressurePct = *patch.MemPressurePct
		}
		if patch.Apply != nil {
			k.Apply = *patch.Apply
		}
		if err := validateKnobs(k); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		store.SetKnobs(k)
		log.Info("knobs changed via control plane",
			"target_mspt", k.TargetMSPT, "min_sim", k.MinSim, "max_sim", k.MaxSim,
			"max_view", k.MaxView, "starve_psi", k.StarvePSI, "gc_stall_pct", k.GCStallPct,
			"mem_pressure_pct", k.MemPressurePct, "apply", k.Apply)
		writeJSON(w, k)
	})

	return requireToken(token, mux)
}

// validateKnobs rejects values that would wedge the controller or the server.
// Bounds mirror the companion's own clamps (distance 2..32) and the tick
// budget (target must leave the controller something to aim under).
func validateKnobs(k Knobs) error {
	if k.TargetMSPT <= 0 || k.TargetMSPT > 50 {
		return fmt.Errorf("target_mspt must be in (0, 50], got %v", k.TargetMSPT)
	}
	if k.MinSim < 2 || k.MaxSim > 32 || k.MinSim > k.MaxSim {
		return fmt.Errorf("sim bounds must satisfy 2 <= min_sim <= max_sim <= 32, got [%d, %d]", k.MinSim, k.MaxSim)
	}
	if k.MaxView < 2 || k.MaxView > 32 {
		return fmt.Errorf("max_view must be in [2, 32], got %d", k.MaxView)
	}
	if k.StarvePSI < 0 || k.StarvePSI > 100 {
		return fmt.Errorf("starve_psi must be in [0, 100], got %v", k.StarvePSI)
	}
	if k.GCStallPct < 0 || k.GCStallPct > 100 {
		return fmt.Errorf("gc_stall_pct must be in [0, 100], got %v", k.GCStallPct)
	}
	if k.MemPressurePct < 0 || k.MemPressurePct > 100 {
		return fmt.Errorf("mem_pressure_pct must be in [0, 100], got %v", k.MemPressurePct)
	}
	return nil
}

// requireToken wraps a handler with token auth. Empty token = no auth (the
// loopback default). The token rides ONLY in the X-Tickwarden-Token header —
// never the URL query string, which leaks into history, logs, and Referer.
// The comparison is constant-time to avoid leaking the token byte by byte.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("X-Tickwarden-Token"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// controlPageHTML is the control plane's single self-contained page — same
// rules as the companion's status page: no external CDNs, fonts, or scripts,
// so it works on an air-gapped box. It polls /api/status and /api/decisions
// every 2s and POSTs /api/config on changes. The visual language matches the
// companion page so the two read as one product.
const controlPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tickwarden daemon</title>
<style>
  :root {
    --bg: #0b0e11; --panel: #14181d; --panel2: #1b2026; --line: #232a31;
    --text: #e6edf3; --muted: #8b97a3;
    --green: #3fb950; --amber: #d9a441; --red: #f0566a; --blue: #58a6ff;
    --mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace;
  }
  * { box-sizing: border-box; }
  html, body { margin: 0; height: 100%; }
  body {
    background: radial-gradient(1200px 600px at 70% -10%, #11161c 0%, var(--bg) 60%);
    color: var(--text); font-family: var(--mono);
    -webkit-font-smoothing: antialiased; padding: 28px 18px 48px;
  }
  .wrap { max-width: 980px; margin: 0 auto; }
  header { display: flex; align-items: baseline; gap: 12px;
    border-bottom: 1px solid var(--line); padding-bottom: 14px; margin-bottom: 22px; }
  .brand { font-size: 20px; font-weight: 700; letter-spacing: 0.04em; }
  .brand .dot { color: var(--green); }
  .sub { color: var(--muted); font-size: 12px; }
  .live { margin-left: auto; color: var(--muted); font-size: 12px; display: flex; align-items: center; gap: 7px; }
  .pulse { width: 8px; height: 8px; border-radius: 50%; background: var(--green); }
  .pulse.stale { background: var(--red); }
  .grid { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 16px; }
  @media (max-width: 760px) { .grid { grid-template-columns: 1fr; } }
  .card { background: linear-gradient(180deg, var(--panel) 0%, var(--panel2) 100%);
    border: 1px solid var(--line); border-radius: 12px; padding: 18px 20px; }
  .full { grid-column: 1 / -1; }
  .label { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: 0.12em; margin-bottom: 6px; }
  .big { font-size: 44px; font-weight: 800; line-height: 1.05; }
  .big small { font-size: 16px; color: var(--muted); font-weight: 500; }
  .green { color: var(--green); } .amber { color: var(--amber); } .red { color: var(--red); }
  .mode { display: inline-block; padding: 3px 10px; border-radius: 7px; font-weight: 700; font-size: 12px;
    border: 1px solid var(--line); cursor: pointer; user-select: none; }
  .mode.dry { color: var(--blue); } .mode.apply { color: var(--red); border-color: var(--red); }
  .gate { display: flex; gap: 8px; align-items: baseline; margin-top: 8px; font-size: 13px; }
  .gate .ind { font-weight: 700; }
  .gate .why { color: var(--muted); font-size: 11px; }
  .bar { height: 10px; background: #0a0d10; border: 1px solid var(--line); border-radius: 6px; overflow: hidden; margin-top: 8px; }
  .bar-fill { height: 100%; width: 0%; background: var(--green); transition: width .4s ease, background .4s ease; }
  form.knobs { display: grid; grid-template-columns: repeat(7, 1fr) auto; gap: 12px; align-items: end; }
  @media (max-width: 760px) { form.knobs { grid-template-columns: 1fr 1fr; } }
  .knob label { display: block; color: var(--muted); font-size: 10px; text-transform: uppercase; letter-spacing: 0.1em; margin-bottom: 5px; }
  .knob input { width: 100%; background: #0a0d10; color: var(--text); border: 1px solid var(--line);
    border-radius: 7px; padding: 8px 10px; font-family: var(--mono); font-size: 14px; }
  button { background: #182230; color: var(--text); border: 1px solid var(--line); border-radius: 7px;
    padding: 9px 16px; font-family: var(--mono); font-size: 13px; font-weight: 700; cursor: pointer; }
  button:hover { border-color: var(--blue); }
  .msg { font-size: 12px; margin-top: 8px; min-height: 16px; }
  table { width: 100%; border-collapse: collapse; font-size: 12.5px; margin-top: 4px; }
  th { text-align: left; color: var(--muted); font-weight: 600; font-size: 11px; text-transform: uppercase;
    letter-spacing: 0.08em; padding: 6px 8px; border-bottom: 1px solid var(--line); }
  td { padding: 7px 8px; border-bottom: 1px solid #1a2026; vertical-align: top; }
  tr:last-child td { border-bottom: 0; }
  .empty { color: var(--muted); padding: 14px 8px; font-size: 13px; }
  td.act { font-weight: 700; white-space: nowrap; }
  td.reason { color: var(--muted); }
  footer { color: var(--muted); font-size: 11px; margin-top: 22px; text-align: center; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div class="brand">tick<span class="dot">warden</span> daemon</div>
    <div class="sub">adaptive control plane</div>
    <div class="live"><span class="pulse" id="pulse"></span><span id="livetext">live</span></div>
  </header>

  <div class="grid">
    <div class="card">
      <div class="label">tick health</div>
      <div class="big"><span id="tps" class="green">--</span><small> TPS</small></div>
      <div class="label" style="margin-top:10px">ms / tick &mdash; 50ms budget</div>
      <div><span id="mspt">--</span> ms</div>
      <div class="bar"><div class="bar-fill" id="msptbar"></div></div>
    </div>

    <div class="card">
      <div class="label">distance &amp; load</div>
      <div class="big"><span id="sim">--</span><small> sim</small> &nbsp;<span id="view">--</span><small> view</small></div>
      <div style="margin-top:10px; font-size:13px"><span id="players">--</span> players (<span id="peak">--</span> peak)</div>
      <div style="margin-top:10px"><span class="label">mode</span>
        <span class="mode dry" id="mode" title="click to toggle apply">--</span></div>
    </div>

    <div class="card">
      <div class="label">cause gates</div>
      <div class="gate"><span class="ind" id="hostgate">--</span><span>host pressure</span></div>
      <div class="why" id="hostwhy"></div>
      <div class="gate"><span class="ind" id="gcgate">--</span><span>JVM garbage collector</span></div>
      <div class="why" id="gcwhy"></div>
      <div class="gate"><span class="ind" id="memgate">--</span><span>memory pressure</span></div>
      <div class="why" id="memwhy"></div>
      <div class="label" style="margin-top:12px">heap</div>
      <div style="font-size:13px"><span id="heap">--</span></div>
      <div class="bar"><div class="bar-fill" id="heapbar"></div></div>
    </div>

    <div class="card full">
      <div class="label">knobs &mdash; live, not persisted (tickwarden.toml is the durable source)</div>
      <form class="knobs" id="knobs" onsubmit="return saveKnobs(event)">
        <div class="knob"><label>target mspt</label><input id="k_target" type="number" step="0.5" min="1" max="50"></div>
        <div class="knob"><label>min sim</label><input id="k_minsim" type="number" min="2" max="32"></div>
        <div class="knob"><label>max sim</label><input id="k_maxsim" type="number" min="2" max="32"></div>
        <div class="knob"><label>max view</label><input id="k_maxview" type="number" min="2" max="32"></div>
        <div class="knob"><label>starve psi %</label><input id="k_starve" type="number" step="1" min="0" max="100"></div>
        <div class="knob"><label>gc stall %</label><input id="k_gc" type="number" step="1" min="0" max="100"></div>
        <div class="knob"><label>mem psi %</label><input id="k_mempsi" type="number" step="1" min="0" max="100"></div>
        <button type="submit">save</button>
      </form>
      <div class="msg" id="knobmsg"></div>
    </div>

    <div class="card full">
      <div class="label">decisions &mdash; newest first</div>
      <table>
        <thead><tr><th>time</th><th>action</th><th>sim</th><th>view</th><th>applied</th><th>reason</th></tr></thead>
        <tbody id="decisions"><tr><td class="empty" colspan="6">waiting for first decision&hellip;</td></tr></tbody>
      </table>
    </div>
  </div>

  <footer>tickwarden daemon control plane &middot; polling every 2s</footer>
</div>

<script>
  const POLL_MS = 2000;
  // The operator may open /?token=... ONCE; we lift it into sessionStorage and
  // scrub it from the URL so it never lingers in history, logs, or Referer.
  // From then on the token rides in the X-Tickwarden-Token request header.
  (function () {
    const u = new URLSearchParams(location.search).get('token');
    if (u) {
      sessionStorage.setItem('tw_token', u);
      history.replaceState(null, '', location.pathname);
    }
  })();
  const TOKEN = sessionStorage.getItem('tw_token') || '';
  // X-Tickwarden-Token doubles as the CSRF marker: the daemon requires this
  // custom header on every mutation even when no token is set, so always send
  // it (a non-empty sentinel when running tokenless on loopback).
  const api = (path, opts) => {
    opts = opts || {};
    const headers = new Headers(opts.headers || {});
    headers.set('X-Tickwarden-Token', TOKEN || 'loopback');
    return fetch(path, Object.assign({}, opts, { headers }));
  };
  const fmt = (n, d) => (n === null || n === undefined || Number.isNaN(n)) ? '--' : Number(n).toFixed(d);
  const gib = (b) => (b / 1073741824).toFixed(2) + ' GiB';
  let knobsDirty = false;   // don't clobber the form while the operator types
  let lastKnobs = null;

  document.getElementById('knobs').addEventListener('input', () => { knobsDirty = true; });

  function setLive(ok) {
    document.getElementById('pulse').classList.toggle('stale', !ok);
    document.getElementById('livetext').textContent = ok ? 'live' : 'no signal';
  }

  function gate(idInd, idWhy, on, why, activeLabel) {
    const ind = document.getElementById(idInd);
    ind.textContent = on ? (activeLabel || 'HOLDING') : 'clear';
    ind.className = 'ind ' + (on ? 'amber' : 'green');
    document.getElementById(idWhy).textContent = on ? (why || '') : '';
  }

  function fillKnobs(k) {
    lastKnobs = k;
    if (knobsDirty) return;
    document.getElementById('k_target').value = k.target_mspt;
    document.getElementById('k_minsim').value = k.min_sim;
    document.getElementById('k_maxsim').value = k.max_sim;
    document.getElementById('k_maxview').value = k.max_view;
    document.getElementById('k_starve').value = k.starve_psi;
    document.getElementById('k_gc').value = k.gc_stall_pct;
    document.getElementById('k_mempsi').value = k.mem_pressure_pct;
    const m = document.getElementById('mode');
    m.textContent = k.apply ? 'APPLY' : 'DRY-RUN';
    m.className = 'mode ' + (k.apply ? 'apply' : 'dry');
  }

  document.getElementById('mode').addEventListener('click', async () => {
    if (!lastKnobs) return;
    const want = !lastKnobs.apply;
    if (want && !confirm('Switch to APPLY? The daemon will change live distances on the server.')) return;
    await post({ apply: want });
  });

  async function post(body) {
    const msg = document.getElementById('knobmsg');
    try {
      const res = await api('/api/config', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
      if (!res.ok) { msg.textContent = await res.text(); msg.className = 'msg red'; return; }
      fillKnobs(await res.json());
      msg.textContent = 'saved — takes effect on the next decision';
      msg.className = 'msg green';
      knobsDirty = false;
    } catch (e) { msg.textContent = String(e); msg.className = 'msg red'; }
  }

  function saveKnobs(ev) {
    ev.preventDefault();
    post({
      target_mspt: Number(document.getElementById('k_target').value),
      min_sim: Number(document.getElementById('k_minsim').value),
      max_sim: Number(document.getElementById('k_maxsim').value),
      max_view: Number(document.getElementById('k_maxview').value),
      starve_psi: Number(document.getElementById('k_starve').value),
      gc_stall_pct: Number(document.getElementById('k_gc').value),
      mem_pressure_pct: Number(document.getElementById('k_mempsi').value),
    });
    return false;
  }

  async function poll() {
    try {
      const [stRes, decRes] = await Promise.all([api('/api/status'), api('/api/decisions')]);
      const { status: st, knobs: k } = await stRes.json();
      const decs = await decRes.json();
      fillKnobs(k);

      const s = st.snapshot || {};
      const tpsEl = document.getElementById('tps');
      tpsEl.textContent = fmt(s.tps, 1);
      tpsEl.className = s.tps < 15 ? 'red' : s.tps < 19.5 ? 'amber' : 'green';
      document.getElementById('mspt').textContent = fmt(s.mspt, 1);
      const pct = Math.min(100, Math.max(0, (s.mspt / 50) * 100));
      const bar = document.getElementById('msptbar');
      bar.style.width = pct + '%';
      bar.style.background = pct < 70 ? 'var(--green)' : pct < 100 ? 'var(--amber)' : 'var(--red)';

      document.getElementById('sim').textContent = s.sim > 0 ? s.sim : '--';
      document.getElementById('view').textContent = s.view > 0 ? s.view : '--';
      document.getElementById('players').textContent = s.players ?? '--';
      document.getElementById('peak').textContent = s.players_peak ?? '--';

      gate('hostgate', 'hostwhy', st.host_starved, st.starve_detail);
      gate('gcgate', 'gcwhy', st.gc_stalled, st.gc_detail);
      gate('memgate', 'memwhy', st.mem_pressured, st.mem_detail, 'STEPPING VIEW ↓');

      const heapEl = document.getElementById('heap');
      const hb = document.getElementById('heapbar');
      if (st.jvm_available && st.jvm.heap_max > 0) {
        const used = st.jvm.heap_used, max = st.jvm.heap_max;
        heapEl.textContent = gib(used) + ' / ' + gib(max);
        const hpct = Math.min(100, 100 * used / max);
        hb.style.width = hpct + '%';
        hb.style.background = hpct < 70 ? 'var(--green)' : hpct < 90 ? 'var(--amber)' : 'var(--red)';
      } else {
        heapEl.textContent = 'no /jvm (companion < 0.6.0)';
        hb.style.width = '0%';
      }

      const tbody = document.getElementById('decisions');
      if (Array.isArray(decs) && decs.length) {
        tbody.innerHTML = '';
        for (const d of decs) {
          const tr = document.createElement('tr');
          const t = new Date(d.time).toLocaleTimeString();
          const cls = d.action === 'lower' ? 'red' : d.action === 'raise' ? 'green' : '';
          const td = (txt, c) => { const e = document.createElement('td'); e.textContent = txt; if (c) e.className = c; return e; };
          tr.appendChild(td(t));
          tr.appendChild(td(d.action, 'act ' + cls));
          tr.appendChild(td(d.sim_from === d.sim_to ? String(d.sim_from) : d.sim_from + ' → ' + d.sim_to));
          tr.appendChild(td(d.view_from === d.view_to ? String(d.view_from) : d.view_from + ' → ' + d.view_to));
          tr.appendChild(td(d.action === 'hold' ? '—' : (d.applied ? 'yes' : 'dry-run')));
          tr.appendChild(td(d.reason, 'reason'));
          tbody.appendChild(tr);
        }
      }
      setLive(st.ok !== false);
    } catch (e) { setLive(false); }
  }

  poll();
  setInterval(poll, POLL_MS);
</script>
</body>
</html>
`

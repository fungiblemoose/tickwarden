// Package daemon runs tickwarden's adaptive controller as a long-lived service:
// poll the companion, ask internal/adaptive.Decide what to do, log the decision,
// and (optionally) apply it — forever, until the context is cancelled.
//
// The brain is NOT reimplemented here. Decide is a pure function in
// internal/adaptive; this package is only the loop, the debounce, the logging,
// and graceful shutdown around it. Everything that touches the outside world
// (reading the companion, applying a distance) is injected via Deps, so the
// whole loop is unit-testable with fakes and no real server.
package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/adaptive"
	"github.com/fungiblemoose/tickwarden/internal/observe"
)

// Config tunes the service. Zero-value is not meant to be used directly; build
// one from DefaultConfig and override fields. The adaptive bounds live in
// adaptive.Config — this only carries what the *loop* needs plus the few knobs
// the operator sets on the command line.
type Config struct {
	BaseURL       string        // companion base URL (uses /tps and /setdistance)
	Interval      time.Duration // decision interval
	TargetMSPT    float64       // keep MSPT at/under this (ms)
	MinSim        int           // never drop simulation distance below this
	MaxSim        int           // never raise simulation distance above this
	MaxView       int           // cap how high view distance can be raised
	RaiseDebounce int           // consecutive raise decisions required before raising
	Apply         bool          // actually apply changes (false = dry-run, log only)

	// StarvePSI is the cgroup PSI some-avg10 percentage at/over which a tick
	// dip is blamed on the HOST rather than the world, making the controller
	// hold instead of cutting distance (see adaptive.State.HostStarved).
	// <= 0 disables the starvation gate entirely.
	StarvePSI float64

	// GCStallPct is the share of the poll window (percent of wall clock) the
	// JVM's GC must have consumed for a tick dip to be blamed on the collector
	// rather than the world (see adaptive.State.GCStalled). Needs companion
	// >= 0.6.0 for /jvm; degrades silently without it. <= 0 disables.
	GCStallPct float64

	// UIAddr enables the daemon's web control plane when non-empty (e.g.
	// "127.0.0.1:9226"): a status dashboard, the decision history, and live
	// knob changes. Empty (the default) serves nothing. A non-loopback bind
	// is refused unless UIToken is set.
	UIAddr string
	// UIToken, when set, is required on every UI/API request (X-Tickwarden-Token
	// header or ?token= query parameter).
	UIToken string

	// StatusEvery is how often a heartbeat status line is emitted regardless of
	// whether anything changed, so the journal shows liveness during quiet
	// periods. It is rounded to a whole number of intervals (minimum one).
	StatusEvery time.Duration
}

// DefaultConfig is a conservative, ready-to-run service configuration. The
// adaptive bounds mirror adaptive.DefaultConfig so the two stay in step.
func DefaultConfig() Config {
	ac := adaptive.DefaultConfig()
	return Config{
		BaseURL:       "http://127.0.0.1:9225",
		Interval:      10 * time.Second,
		TargetMSPT:    ac.TargetMSPT,
		MinSim:        ac.MinSim,
		MaxSim:        ac.MaxSim,
		MaxView:       ac.MaxView,
		RaiseDebounce: 3,
		Apply:         false,
		StarvePSI:     observe.DefaultThresholds().PSIPressurePct,
		GCStallPct:    15, // a 1.5s+ GC slice of a 10s window is collector trouble, not world load
		StatusEvery:   5 * time.Minute,
	}
}

// knobs projects the service Config onto the live-tunable subset the store
// (and thus the web control plane) manages.
func (c Config) knobs() Knobs {
	return Knobs{
		TargetMSPT: c.TargetMSPT,
		MinSim:     c.MinSim,
		MaxSim:     c.MaxSim,
		MaxView:    c.MaxView,
		StarvePSI:  c.StarvePSI,
		GCStallPct: c.GCStallPct,
		Apply:      c.Apply,
	}
}

// adaptiveConfigFor projects the live knobs onto the controller's Config,
// keeping the ramp/deadband defaults and overriding only the operator knobs.
// Derived per step (not once at startup) so a knob change from the web control
// plane takes effect on the very next decision.
func adaptiveConfigFor(k Knobs) adaptive.Config {
	ac := adaptive.DefaultConfig()
	ac.TargetMSPT = k.TargetMSPT
	ac.MinSim = k.MinSim
	ac.MaxSim = k.MaxSim
	ac.MaxView = k.MaxView
	return ac
}

// Deps are the injected seams: how to read the companion and how to apply a
// decision. Tests supply fakes; production uses HTTPDeps. Logger may be nil
// (a discard logger is substituted), so callers that only want defaults can
// pass a zero Deps.
type Deps struct {
	// Fetch returns the current companion snapshot.
	Fetch func() (observe.Snapshot, error)
	// Apply enforces a new simulation/view distance on the server.
	Apply func(sim, view int) error
	// Pressure reads the current cgroup pressure, feeding the starvation gate.
	// If nil, observe.ReadPressure is used (Run installs it); tests inject a
	// fake. Only consulted when the StarvePSI knob is > 0.
	Pressure func() observe.Pressure
	// JVM reads the companion's heap/GC telemetry, feeding the GC-stall gate.
	// If nil, Run installs an HTTP fetch of /jvm. Errors are tolerated (older
	// companions don't serve /jvm); only consulted when GCStallPct is > 0.
	JVM func() (observe.JVMStats, error)
	// Logger receives structured decision and status lines. If nil, output is
	// discarded.
	Logger *slog.Logger
	// Now returns the current time; defaults to time.Now. Injectable for tests.
	Now func() time.Time
}

// HTTPDeps builds production Deps backed by the companion's HTTP endpoints.
// Fetch reads baseURL+"/tps"; Apply GETs baseURL+"/setdistance". Logs are
// written to w as structured text lines (pass os.Stderr for journald).
func HTTPDeps(baseURL string, w io.Writer) Deps {
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return Deps{
		Fetch:    func() (observe.Snapshot, error) { return observe.FetchSnapshot(baseURL + "/tps") },
		Apply:    func(sim, view int) error { return applyDistance(baseURL, sim, view) },
		Pressure: observe.ReadPressure,
		JVM:      func() (observe.JVMStats, error) { return observe.FetchJVM(baseURL + "/jvm") },
		Logger:   logger,
		Now:      time.Now,
	}
}

// applyDistance enforces a new simulation/view distance via the companion's
// /setdistance endpoint. (Mirrors the helper in the command layer, which this
// package may not import.)
func applyDistance(baseURL string, sim, view int) error {
	url := fmt.Sprintf("%s/setdistance?sim=%d&view=%d", baseURL, sim, view)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("setdistance returned %d", resp.StatusCode)
	}
	return nil
}

// loop carries the mutable state Run threads through each step.
type loop struct {
	cfg    Config
	store  *Store // live knobs in, observations/decisions out
	deps   Deps
	log    *slog.Logger
	now    func() time.Time
	raises int // consecutive raise decisions (debounce counter)
	ticks  int // total steps taken (drives the status heartbeat)
	// statusEveryN is StatusEvery measured in whole intervals (>=1).
	statusEveryN int
	// prevPressure is the last cgroup-pressure reading, kept so the starvation
	// gate can detect NEW throttle events (the counters are cumulative since
	// boot, so only the delta between polls means "throttled just now").
	// prevPressureSet distinguishes "no reading yet" from a zero-value one.
	prevPressure    observe.Pressure
	prevPressureSet bool
	// prevJVM mirrors prevPressure for the GC counters (also cumulative).
	prevJVM    observe.JVMStats
	prevJVMSet bool
	// lastHoldReason suppresses duplicate hold records in the store: a quiet
	// server holding for the same reason for an hour is one entry, not 360.
	lastHoldReason string
}

// Run drives the adaptive loop continuously until ctx is cancelled, at which
// point it returns ctx.Err() (typically context.Canceled). Each decision is
// logged; a periodic heartbeat status line is emitted on StatusEvery. Errors
// reading or applying are logged and the loop continues — a transient
// companion blip must not kill the service.
func Run(ctx context.Context, cfg Config, deps Deps) error {
	if deps.Fetch == nil {
		hd := HTTPDeps(cfg.BaseURL, io.Discard)
		deps.Fetch = hd.Fetch
	}
	if deps.Apply == nil {
		hd := HTTPDeps(cfg.BaseURL, io.Discard)
		deps.Apply = hd.Apply
	}
	if deps.Pressure == nil {
		deps.Pressure = observe.ReadPressure
	}
	if deps.JVM == nil {
		deps.JVM = func() (observe.JVMStats, error) { return observe.FetchJVM(cfg.BaseURL + "/jvm") }
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultConfig().Interval
	}
	if cfg.RaiseDebounce < 1 {
		cfg.RaiseDebounce = 1
	}

	// StatusEvery <= 0 disables the heartbeat (statusEveryN == 0); otherwise it
	// is the heartbeat period measured in whole intervals (at least one).
	statusEveryN := 0
	if cfg.StatusEvery > 0 && cfg.Interval > 0 {
		statusEveryN = int(cfg.StatusEvery / cfg.Interval)
		if statusEveryN < 1 {
			statusEveryN = 1
		}
	}

	l := &loop{
		cfg:          cfg,
		store:        NewStore(cfg.knobs()),
		deps:         deps,
		log:          deps.Logger,
		now:          deps.Now,
		statusEveryN: statusEveryN,
	}

	mode := "dry-run"
	if cfg.Apply {
		mode = "apply"
	}
	l.log.Info("daemon starting",
		"mode", mode,
		"url", cfg.BaseURL,
		"interval", cfg.Interval.String(),
		"target_mspt", cfg.TargetMSPT,
		"sim_min", cfg.MinSim,
		"sim_max", cfg.MaxSim,
		"raise_debounce", cfg.RaiseDebounce,
		"starve_psi", cfg.StarvePSI,
		"gc_stall_pct", cfg.GCStallPct,
	)

	// Web control plane: optional, off unless UIAddr is set. Started here so
	// it shares the loop's store; it dies with the context like the loop does.
	if cfg.UIAddr != "" {
		if err := serveUI(ctx, cfg, l.store, l.log); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.log.Info("daemon stopping", "reason", ctx.Err().Error())
			return ctx.Err()
		case <-ticker.C:
			l.step()
		}
	}
}

// step performs one fetch → Decide → debounce → (maybe) apply → log cycle. It
// never blocks on the ticker and never returns an error: failures are logged
// and the loop keeps going. Factored out of Run so tests can drive the loop
// deterministically, one cycle at a time, with no wall-clock dependence.
func (l *loop) step() {
	l.ticks++
	k := l.store.Knobs() // fresh every step: the control plane may have changed them

	snap, err := l.deps.Fetch()
	if err != nil {
		l.log.Warn("read failed", "err", err.Error())
		l.store.SetStatus(Status{Time: l.now(), OK: false, Err: err.Error()})
		return
	}
	if snap.Sim <= 0 {
		l.log.Warn("companion not reporting sim/view yet (needs companion >= 0.4.0)")
		l.store.SetStatus(Status{Time: l.now(), OK: false, Err: "companion not reporting sim/view (needs >= 0.4.0)", Snapshot: snap})
		return
	}

	st := adaptive.State{
		Players:     snap.Players,
		PlayersPeak: snap.PlayersPeak,
		MSPT:        snap.MSPT,
		CurrentSim:  snap.Sim,
		CurrentView: snap.View,
	}
	// Starvation gate: read cgroup pressure and let the controller hold when
	// the host, not the world, explains a bad MSPT. The first poll compares
	// the throttle counters against themselves (delta zero) so a cumulative
	// count from before the daemon started doesn't read as "starved now".
	if k.StarvePSI > 0 && l.deps.Pressure != nil {
		cur := l.deps.Pressure()
		prev := cur
		if l.prevPressureSet {
			prev = l.prevPressure
		}
		st.HostStarved, st.StarveDetail = observe.StarvedNow(prev, cur, k.StarvePSI)
		l.prevPressure, l.prevPressureSet = cur, true
	}
	// GC gate: same delta pattern over the companion's cumulative GC counters.
	// A fetch error means an older companion without /jvm — degrade silently
	// to "no GC awareness" rather than spam the log every 10 seconds.
	var jvm observe.JVMStats
	if k.GCStallPct > 0 && l.deps.JVM != nil {
		if cur, jerr := l.deps.JVM(); jerr == nil {
			jvm = cur
			prev := cur
			if l.prevJVMSet {
				prev = l.prevJVM
			}
			st.GCStalled, st.GCDetail = observe.GCStalledNow(prev, cur, l.cfg.Interval, k.GCStallPct)
			l.prevJVM, l.prevJVMSet = cur, true
		}
	}

	d := adaptive.Decide(st, adaptiveConfigFor(k))

	l.store.SetStatus(Status{
		Time:         l.now(),
		OK:           true,
		Snapshot:     snap,
		HostStarved:  st.HostStarved,
		StarveDetail: st.StarveDetail,
		GCStalled:    st.GCStalled,
		GCDetail:     st.GCDetail,
		JVM:          jvm,
		JVMAvailable: jvm.Available,
	})

	// Heartbeat: a periodic liveness line even when nothing changes.
	if l.statusEveryN > 0 && l.ticks%l.statusEveryN == 0 {
		l.log.Info("status",
			"tps", snap.TPS,
			"mspt", snap.MSPT,
			"players", snap.Players,
			"players_peak", snap.PlayersPeak,
			"sim", snap.Sim,
			"view", snap.View,
			"host_starved", st.HostStarved,
			"gc_stalled", st.GCStalled,
		)
	}

	// Debounce raises: only act after the controller asks repeatedly.
	// Lowering is immediate (protect TPS); any non-raise resets the streak.
	if d.Action == adaptive.ActionRaise {
		l.raises++
		if l.raises < l.cfg.RaiseDebounce {
			l.log.Info("raise pending",
				"streak", l.raises,
				"need", l.cfg.RaiseDebounce,
				"reason", d.Reason,
			)
			return
		}
	} else {
		l.raises = 0
	}

	l.log.Info("decision",
		"action", string(d.Action),
		"sim_from", snap.Sim, "sim_to", d.Sim,
		"view_from", snap.View, "view_to", d.View,
		"reason", d.Reason,
	)

	// Record for the control plane. Repeated holds for the same reason
	// collapse into the first record (a quiet hour is one entry, not 360);
	// anything actionable is always recorded, with Applied reflecting whether
	// the change actually reached the server.
	rec := DecisionRecord{
		Time:    l.now(),
		Action:  string(d.Action),
		SimFrom: snap.Sim, SimTo: d.Sim,
		ViewFrom: snap.View, ViewTo: d.View,
		Reason: d.Reason,
	}

	if d.Action == adaptive.ActionHold {
		if d.Reason != l.lastHoldReason {
			l.lastHoldReason = d.Reason
			l.store.AddDecision(rec)
		}
		return
	}
	l.lastHoldReason = ""

	if k.Apply {
		if err := l.deps.Apply(d.Sim, d.View); err != nil {
			l.log.Warn("apply failed", "err", err.Error())
		} else {
			rec.Applied = true
			// A successful apply resets the raise streak so the next raise
			// must re-earn its debounce.
			l.raises = 0
			l.log.Info("applied", "sim", d.Sim, "view", d.View)
		}
	}
	l.store.AddDecision(rec)
}

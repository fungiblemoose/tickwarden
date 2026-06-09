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
	RaiseDebounce int           // consecutive raise decisions required before raising
	Apply         bool          // actually apply changes (false = dry-run, log only)

	// StarvePSI is the cgroup PSI some-avg10 percentage at/over which a tick
	// dip is blamed on the HOST rather than the world, making the controller
	// hold instead of cutting distance (see adaptive.State.HostStarved).
	// <= 0 disables the starvation gate entirely.
	StarvePSI float64

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
		RaiseDebounce: 3,
		Apply:         false,
		StarvePSI:     observe.DefaultThresholds().PSIPressurePct,
		StatusEvery:   5 * time.Minute,
	}
}

// adaptiveConfig projects the service Config onto the controller's Config,
// keeping the ramp/deadband defaults and overriding only the operator knobs.
func (c Config) adaptiveConfig() adaptive.Config {
	ac := adaptive.DefaultConfig()
	ac.TargetMSPT = c.TargetMSPT
	ac.MinSim = c.MinSim
	ac.MaxSim = c.MaxSim
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
	// fake. Only consulted when Config.StarvePSI > 0.
	Pressure func() observe.Pressure
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
	ac     adaptive.Config
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
		ac:           cfg.adaptiveConfig(),
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
	)

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

	snap, err := l.deps.Fetch()
	if err != nil {
		l.log.Warn("read failed", "err", err.Error())
		return
	}
	if snap.Sim <= 0 {
		l.log.Warn("companion not reporting sim/view yet (needs companion >= 0.4.0)")
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
	if l.cfg.StarvePSI > 0 && l.deps.Pressure != nil {
		cur := l.deps.Pressure()
		prev := cur
		if l.prevPressureSet {
			prev = l.prevPressure
		}
		st.HostStarved, st.StarveDetail = observe.StarvedNow(prev, cur, l.cfg.StarvePSI)
		l.prevPressure, l.prevPressureSet = cur, true
	}

	d := adaptive.Decide(st, l.ac)

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

	if d.Action == adaptive.ActionHold || !l.cfg.Apply {
		return
	}

	if err := l.deps.Apply(d.Sim, d.View); err != nil {
		l.log.Warn("apply failed", "err", err.Error())
		return
	}
	// A successful apply resets the raise streak so the next raise must
	// re-earn its debounce.
	l.raises = 0
	l.log.Info("applied", "sim", d.Sim, "view", d.View)
}

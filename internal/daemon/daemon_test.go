package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/observe"
)

// fakeApply records what was applied so tests can assert on count and values
// without a real server.
type fakeApply struct {
	mu       sync.Mutex
	calls    int
	lastSim  int
	lastView int
}

func (f *fakeApply) apply(sim, view int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastSim, f.lastView = sim, view
	return nil
}

func (f *fakeApply) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newLoop builds a loop the same way Run does, so step() can be driven
// synchronously in tests — no ticker, no wall clock.
func newLoop(cfg Config, snap observe.Snapshot, snapErr error, apply func(int, int) error) *loop {
	full := DefaultConfig()
	if cfg.TargetMSPT != 0 {
		full.TargetMSPT = cfg.TargetMSPT
	}
	if cfg.MinSim != 0 {
		full.MinSim = cfg.MinSim
	}
	if cfg.MaxSim != 0 {
		full.MaxSim = cfg.MaxSim
	}
	if cfg.RaiseDebounce != 0 {
		full.RaiseDebounce = cfg.RaiseDebounce
	}
	full.Apply = cfg.Apply

	return &loop{
		cfg: full,
		ac:  full.adaptiveConfig(),
		deps: Deps{
			Fetch: func() (observe.Snapshot, error) { return snap, snapErr },
			Apply: apply,
		},
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:          time.Now,
		statusEveryN: 1,
	}
}

// High MSPT (over budget) → a lower simulation distance is applied immediately.
func TestStep_HighMSPTLowers(t *testing.T) {
	fa := &fakeApply{}
	// 4 players, MSPT 48 over the 35 target, sim 14 → adaptive.Decide lowers.
	snap := observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 48, Sim: 14, View: 18, TPS: 19}
	l := newLoop(Config{Apply: true}, snap, nil, fa.apply)

	l.step()

	if fa.count() != 1 {
		t.Fatalf("over-budget snapshot should apply once, got %d applies", fa.count())
	}
	if fa.lastSim >= 14 {
		t.Fatalf("applied sim %d should be lower than current 14", fa.lastSim)
	}
}

// Sustained low MSPT → a raise is applied only after the debounce streak is met.
func TestStep_SustainedLowRaisesAfterDebounce(t *testing.T) {
	fa := &fakeApply{}
	debounce := 3
	// 8 players, MSPT 20 (well under the 35 target) at sim 8 → Decide raises.
	snap := observe.Snapshot{Players: 8, PlayersPeak: 8, MSPT: 20, Sim: 8, View: 12, TPS: 20}
	l := newLoop(Config{Apply: true, RaiseDebounce: debounce}, snap, nil, fa.apply)

	for i := 1; i < debounce; i++ {
		l.step()
		if fa.count() != 0 {
			t.Fatalf("must not raise before debounce met (step %d), got %d applies", i, fa.count())
		}
	}
	// The debounce-th raise decision should apply exactly once.
	l.step()
	if fa.count() != 1 {
		t.Fatalf("raise should apply on the %dth consecutive decision, got %d applies", debounce, fa.count())
	}
	if fa.lastSim <= 8 {
		t.Fatalf("applied sim %d should be higher than current 8", fa.lastSim)
	}
}

// Host starvation → the controller holds, so even an over-budget MSPT must not
// trigger an apply; once the pressure lifts, the same MSPT lowers as usual.
func TestStep_HostStarvedHoldsThenRecovers(t *testing.T) {
	fa := &fakeApply{}
	// Same over-budget snapshot as TestStep_HighMSPTLowers...
	snap := observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 48, Sim: 14, View: 18, TPS: 19}
	l := newLoop(Config{Apply: true}, snap, nil, fa.apply)
	// ...but the cgroup shows heavy I/O pressure: the host is the bottleneck.
	starved := true
	l.deps.Pressure = func() observe.Pressure {
		p := observe.Pressure{Available: true}
		if starved {
			p.IO.SomeAvg10 = 42
		}
		return p
	}

	l.step()
	if fa.count() != 0 {
		t.Fatalf("starved over-budget step must hold, got %d applies", fa.count())
	}

	// Pressure gone → the same MSPT is now the world's fault and lowers.
	starved = false
	l.step()
	if fa.count() != 1 {
		t.Fatalf("after pressure lifts the lower should apply, got %d applies", fa.count())
	}
	if fa.lastSim >= 14 {
		t.Fatalf("applied sim %d should be lower than current 14", fa.lastSim)
	}
}

// A stale cumulative throttle count (from before the daemon started) must not
// read as starvation: only new events between polls count.
func TestStep_StaleThrottleCountIsNotStarvation(t *testing.T) {
	fa := &fakeApply{}
	snap := observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 48, Sim: 14, View: 18, TPS: 19}
	l := newLoop(Config{Apply: true}, snap, nil, fa.apply)
	l.deps.Pressure = func() observe.Pressure {
		return observe.Pressure{Available: true, NrThrottled: 500} // old history, unchanging
	}

	l.step()
	if fa.count() != 1 {
		t.Fatalf("an unchanging throttle counter must not block the lower, got %d applies", fa.count())
	}
}

// Dry-run (Apply=false) → an actionable decision is logged but never applied.
func TestStep_DryRunNeverApplies(t *testing.T) {
	fa := &fakeApply{}
	snap := observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 48, Sim: 14, View: 18, TPS: 19}
	l := newLoop(Config{Apply: false}, snap, nil, fa.apply)

	for i := 0; i < 5; i++ {
		l.step()
	}
	if fa.count() != 0 {
		t.Fatalf("dry-run must never apply, got %d applies", fa.count())
	}
}

// A fetch error is swallowed (logged) and never triggers an apply.
func TestStep_FetchErrorIsSafe(t *testing.T) {
	fa := &fakeApply{}
	l := newLoop(Config{Apply: true}, observe.Snapshot{}, errors.New("connection refused"), fa.apply)

	l.step()
	if fa.count() != 0 {
		t.Fatalf("a fetch error must not apply anything, got %d applies", fa.count())
	}
}

// Run returns promptly when the context is cancelled.
func TestRun_StopsOnContextCancel(t *testing.T) {
	fa := &fakeApply{}
	cfg := DefaultConfig()
	cfg.Interval = 5 * time.Millisecond
	cfg.Apply = false
	deps := Deps{
		Fetch: func() (observe.Snapshot, error) {
			return observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 30, Sim: 10, View: 14, TPS: 20}, nil
		},
		Apply:  fa.apply,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, deps) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run should return context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of context cancel")
	}
}

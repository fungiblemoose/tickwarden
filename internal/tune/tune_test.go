package tune

import (
	"testing"

	"github.com/fungiblemoose/tickwarden/internal/detect"
)

func simFor(t *testing.T, cores float64, players int, perfMods bool) int {
	return simForOpts(t, cores, Options{Players: players, PerfMods: perfMods})
}

func simForOpts(t *testing.T, cores float64, opts Options) int {
	t.Helper()
	p := detect.Profile{EffectiveCores: cores, MemoryBudgetBytes: 8 << 30, Virt: detect.VirtLXC}
	plan := Recommend(p, opts)
	for _, r := range plan.Recs {
		if r.Key == "simulation-distance" {
			// values are small ints rendered as strings
			var v int
			for _, c := range r.Value {
				v = v*10 + int(c-'0')
			}
			return v
		}
	}
	t.Fatal("no simulation-distance rec")
	return 0
}

// The calibration anchor: 3 effective cores, 2 peak players, perf mods → sim≈10,
// the spark-validated point on the reference server. If this breaks, the
// budgetPerCore constant (and docs/DECISION_TREE.md) must be re-derived together.
func TestCalibrationAnchor(t *testing.T) {
	if got := simFor(t, 3, 2, true); got != 10 {
		t.Fatalf("calibration drift: 3 cores / 2 players / perf-mods gave sim=%d, expected 10", got)
	}
}

func TestMorePlayersLowersSim(t *testing.T) {
	few := simFor(t, 3, 2, true)
	many := simFor(t, 3, 16, true)
	if many >= few {
		t.Fatalf("sim should fall as players rise: 2p=%d, 16p=%d", few, many)
	}
}

func TestPerfModsRaiseSim(t *testing.T) {
	with := simFor(t, 8, 4, true)
	without := simFor(t, 8, 4, false)
	if without >= with {
		t.Fatalf("perf mods should allow a higher sim: with=%d, without=%d", with, without)
	}
}

// Clustered play overlaps player-loaded areas, so it should never lower the
// recommended sim distance and should raise it when the halved effective count
// crosses a bucket. 3 cores / 2 players / perf-mods: spread gives sim=10
// (sqrt(64*3/2)≈9.8), clustered halves to 1 effective player giving
// sqrt(64*3/1)≈13.9 → 14 — strictly greater, and both inside the [5,16] clamp.
func TestClusteredRaisesSim(t *testing.T) {
	spread := simForOpts(t, 3, Options{Players: 2, PerfMods: true, Clustered: false})
	clustered := simForOpts(t, 3, Options{Players: 2, PerfMods: true, Clustered: true})
	if clustered <= spread {
		t.Fatalf("clustered should raise sim: spread=%d, clustered=%d", spread, clustered)
	}
	if spread != 10 || clustered != 14 {
		t.Fatalf("expected spread=10, clustered=14; got spread=%d clustered=%d", spread, clustered)
	}
}

// Across a range of inputs, clustered must never produce a LOWER sim than
// spread (it reduces or holds the effective player count, never raises it).
func TestClusteredNeverLowersSim(t *testing.T) {
	cases := []struct {
		cores   float64
		players int
	}{{1, 1}, {3, 2}, {8, 4}, {8, 16}, {2, 50}, {64, 1}}
	for _, c := range cases {
		spread := simForOpts(t, c.cores, Options{Players: c.players, PerfMods: true})
		clustered := simForOpts(t, c.cores, Options{Players: c.players, PerfMods: true, Clustered: true})
		if clustered < spread {
			t.Fatalf("clustered lowered sim at %v cores/%d players: spread=%d clustered=%d", c.cores, c.players, spread, clustered)
		}
	}
}

func recVal(t *testing.T, p detect.Profile, opts Options, key string) (string, bool) {
	t.Helper()
	for _, r := range Recommend(p, opts).Recs {
		if r.Key == key {
			return r.Value, true
		}
	}
	return "", false
}

// Heap should track expected load within the budget, not grab all-RAM-minus-a-sliver:
// 8 GiB / 2 players → 3 GiB (not 6), while 8 GiB / 8 players → 6 GiB.
func TestHeapSizedToLoad(t *testing.T) {
	p := detect.Profile{EffectiveCores: 3, MemoryBudgetBytes: 8 << 30}
	if v, _ := recVal(t, p, Options{Players: 2, PerfMods: true}, "Xmx"); v != "3G" {
		t.Fatalf("8GiB/2players Xmx: want 3G, got %q", v)
	}
	if v, _ := recVal(t, p, Options{Players: 8, PerfMods: true}, "Xmx"); v != "6G" {
		t.Fatalf("8GiB/8players Xmx: want 6G, got %q", v)
	}
}

// C2ME must be kept (not disabled) even on a 3-core box, sized to 2 workers
// (leave the main thread a core) — the measured-helpful config on the ref server.
func TestC2MEKeptOnFewCores(t *testing.T) {
	p := detect.Profile{EffectiveCores: 3, MemoryBudgetBytes: 8 << 30}
	v, ok := recVal(t, p, Options{Players: 2, PerfMods: true}, "c2me.threads")
	if !ok || v != "2" {
		t.Fatalf("3 cores should keep C2ME at 2 workers, got %q (present=%v)", v, ok)
	}
	if _, disabled := recVal(t, p, Options{Players: 2, PerfMods: true}, "c2me"); disabled {
		t.Fatal("must not emit a 'consider disabling' c2me rec on few cores")
	}
}

// Lighting must recommend the maintained ScalableLux regardless of core count —
// never bare Starlight, which often has no build for the current MC version.
func TestLightingAlwaysScalableLux(t *testing.T) {
	for _, cores := range []float64{3, 8, 16} {
		p := detect.Profile{EffectiveCores: cores, MemoryBudgetBytes: 8 << 30}
		if v, _ := recVal(t, p, Options{Players: 2, PerfMods: true}, "lighting-engine"); v != "ScalableLux" {
			t.Fatalf("%.0f cores: lighting want ScalableLux, got %q", cores, v)
		}
	}
}

func TestSimClamped(t *testing.T) {
	// Huge cores, one player must not produce an absurd sim distance.
	if got := simFor(t, 64, 1, true); got > 16 {
		t.Fatalf("sim should clamp to 16, got %d", got)
	}
	// Tiny box, a crowd must not drop below the practical floor of 5.
	if got := simFor(t, 1, 50, true); got < 5 {
		t.Fatalf("sim should clamp to 5, got %d", got)
	}
}

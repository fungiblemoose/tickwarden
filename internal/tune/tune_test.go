package tune

import (
	"testing"

	"github.com/fungiblemoose/tickwarden/internal/detect"
)

func simFor(t *testing.T, cores float64, players int, perfMods bool) int {
	t.Helper()
	p := detect.Profile{EffectiveCores: cores, MemoryBudgetBytes: 8 << 30, Virt: detect.VirtLXC}
	plan := Recommend(p, Options{Players: players, PerfMods: perfMods})
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

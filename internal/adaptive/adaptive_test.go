package adaptive

import (
	"strings"
	"testing"
)

func cfg() Config { return DefaultConfig() }

// Headroom smaller than a full step must hold (not raise + overshoot), and the
// reason must say "overshoot", not "ceiling" (the mislabel the live run found).
func TestDecide_HoldsWhenStepWouldOvershoot(t *testing.T) {
	// sim 6, MSPT 27 (< 28 band) → idealSim ≈ 6·√(35/27) ≈ 6.8, so +1 (7) overshoots.
	d := Decide(State{Players: 8, MSPT: 27, CurrentSim: 6, CurrentView: 10}, cfg())
	if d.Action != ActionHold || d.Sim != 6 {
		t.Fatalf("a step that overshoots should hold at 6, got %s sim=%d", d.Action, d.Sim)
	}
	if !strings.Contains(d.Reason, "overshoot") {
		t.Fatalf("reason should mention overshoot, not ceiling: %q", d.Reason)
	}
}

func TestDecide_OverBudgetLowers(t *testing.T) {
	// 4 players, MSPT 48 (over the 35 target), sim 14 → must lower, bounded by MaxStepDown.
	d := Decide(State{Players: 4, MSPT: 48, CurrentSim: 14, CurrentView: 18}, cfg())
	if d.Action != ActionLower {
		t.Fatalf("over budget should lower, got %s (%s)", d.Action, d.Reason)
	}
	if d.Sim < 14-cfg().MaxStepDown {
		t.Fatalf("must not drop more than MaxStepDown: %d", d.Sim)
	}
	if d.Sim >= 14 {
		t.Fatalf("lower means smaller sim, got %d", d.Sim)
	}
}

func TestDecide_HeadroomRaisesOneStep(t *testing.T) {
	// Comfortably under target → raise, but only one cautious step.
	d := Decide(State{Players: 1, MSPT: 7, CurrentSim: 10, CurrentView: 14}, cfg())
	if d.Action != ActionRaise {
		t.Fatalf("headroom should raise, got %s (%s)", d.Action, d.Reason)
	}
	if d.Sim != 11 {
		t.Fatalf("raise should be one step (10→11), got %d", d.Sim)
	}
}

func TestDecide_WithinBandHolds(t *testing.T) {
	// MSPT 32 vs target 35, deadband 0.2 → band is [28,35]; 32 is inside → hold.
	d := Decide(State{Players: 3, MSPT: 32, CurrentSim: 12, CurrentView: 16}, cfg())
	if d.Action != ActionHold {
		t.Fatalf("within band should hold, got %s (%s)", d.Action, d.Reason)
	}
	if d.Sim != 12 {
		t.Fatalf("hold keeps sim, got %d", d.Sim)
	}
}

func TestDecide_RespectsFloorWhenOver(t *testing.T) {
	// Over budget but already at the floor → can't lower, hold.
	c := cfg()
	d := Decide(State{Players: 8, MSPT: 90, CurrentSim: c.MinSim, CurrentView: 10}, c)
	if d.Action != ActionHold || d.Sim != c.MinSim {
		t.Fatalf("at floor + over budget should hold at floor, got %s sim=%d", d.Action, d.Sim)
	}
}

func TestDecide_NoPlayersHolds(t *testing.T) {
	d := Decide(State{Players: 0, MSPT: 5, CurrentSim: 12}, cfg())
	if d.Action != ActionHold {
		t.Fatalf("empty server should hold, got %s", d.Action)
	}
}

func TestDecide_RaiseCappedAtCeiling(t *testing.T) {
	c := cfg()
	d := Decide(State{Players: 1, MSPT: 2, CurrentSim: c.MaxSim, CurrentView: 24}, c)
	if d.Action != ActionHold || d.Sim != c.MaxSim {
		t.Fatalf("at ceiling should hold, got %s sim=%d", d.Action, d.Sim)
	}
}

func TestDecide_ViewFollowsSimWithinBounds(t *testing.T) {
	d := Decide(State{Players: 1, MSPT: 7, CurrentSim: 10, CurrentView: 14}, cfg())
	// raised to sim 11 → view = 11 + buffer(4) = 15, within [8,24].
	if d.View != 15 {
		t.Fatalf("view should follow sim (11+4=15), got %d", d.View)
	}
}

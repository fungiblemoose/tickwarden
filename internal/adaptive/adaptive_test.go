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

// On-join pre-sizing: with one player and low MSPT but an expected peak of 4,
// the controller must NOT raise to a distance that 4 players would break — it
// sizes for the peak so a join doesn't trigger a cut.
func TestDecide_PreSizesForPeak(t *testing.T) {
	sized := Decide(State{Players: 1, PlayersPeak: 4, MSPT: 7, CurrentSim: 12, CurrentView: 16}, cfg())
	naive := Decide(State{Players: 1, PlayersPeak: 0, MSPT: 7, CurrentSim: 12, CurrentView: 16}, cfg())
	if naive.Action != ActionRaise {
		t.Fatalf("without peak awareness, low 1-player MSPT should raise; got %s", naive.Action)
	}
	if sized.Sim > naive.Sim {
		t.Fatalf("peak-aware sizing must never raise higher than naive: sized=%d naive=%d", sized.Sim, naive.Sim)
	}
	if sized.Action != ActionHold || sized.Sim != 12 {
		t.Fatalf("peak-4 projection (7×4=28, band edge) should hold at the peak-safe 12, got %s sim=%d", sized.Action, sized.Sim)
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
	// raised to sim 11 → view ratchets up to 11 + buffer(4) = 15, within [8,24].
	if d.View != 15 {
		t.Fatalf("view should follow sim up (11+4=15), got %d", d.View)
	}
}

// View costs bandwidth/RAM, not tick CPU — lowering sim must NOT drag view
// down with it. Cutting view recovers zero MSPT; it would only be a visible
// render cut for nothing.
func TestDecide_LowerNeverCutsView(t *testing.T) {
	// 4 players, MSPT 48 → lower sim 14→11; view 18 must stay 18, not 11+4=15.
	d := Decide(State{Players: 4, MSPT: 48, CurrentSim: 14, CurrentView: 18}, cfg())
	if d.Action != ActionLower {
		t.Fatalf("over budget should lower, got %s (%s)", d.Action, d.Reason)
	}
	if d.View != 18 {
		t.Fatalf("lowering sim must not cut view: want 18, got %d", d.View)
	}
}

// An operator-set view above MaxView is left alone, never "corrected" down.
func TestDecide_ViewAboveCeilingNotCorrected(t *testing.T) {
	d := Decide(State{Players: 3, MSPT: 32, CurrentSim: 12, CurrentView: 32}, cfg())
	if d.View != 32 {
		t.Fatalf("view above MaxView must be left alone, got %d", d.View)
	}
}

// At/over the panic ceiling the server is dropping ticks outright: snap sim
// straight to the floor, bypassing MaxStepDown — and still don't touch view.
func TestDecide_PanicSnapsSimToFloor(t *testing.T) {
	c := cfg()
	d := Decide(State{Players: 4, MSPT: 55, CurrentSim: 14, CurrentView: 18}, c)
	if d.Action != ActionLower {
		t.Fatalf("panic should lower, got %s (%s)", d.Action, d.Reason)
	}
	if d.Sim != c.MinSim {
		t.Fatalf("panic should snap to the floor (%d), not step: got %d", c.MinSim, d.Sim)
	}
	if d.View != 18 {
		t.Fatalf("panic must not cut view either: want 18, got %d", d.View)
	}
	if !strings.Contains(d.Reason, "panic") {
		t.Fatalf("reason should name the panic ceiling: %q", d.Reason)
	}
}

// When the host is starving the container, a bad MSPT says nothing about world
// cost — the controller must hold, not cut, and must say why.
func TestDecide_HostStarvedHoldsInsteadOfLowering(t *testing.T) {
	d := Decide(State{Players: 4, MSPT: 48, CurrentSim: 14, CurrentView: 18,
		HostStarved: true, StarveDetail: "I/O pressure some-avg10=42.0%"}, cfg())
	if d.Action != ActionHold || d.Sim != 14 {
		t.Fatalf("starved should hold (not lower), got %s sim=%d", d.Action, d.Sim)
	}
	if !strings.Contains(d.Reason, "HOST") || !strings.Contains(d.Reason, "I/O pressure") {
		t.Fatalf("reason should blame the host and carry the detail: %q", d.Reason)
	}
}

// Starvation outranks the panic valve: 60ms of starved MSPT is still not the
// world's fault, so don't snap to the floor.
func TestDecide_HostStarvedSuppressesPanic(t *testing.T) {
	d := Decide(State{Players: 4, MSPT: 60, CurrentSim: 14, CurrentView: 18, HostStarved: true}, cfg())
	if d.Action != ActionHold || d.Sim != 14 {
		t.Fatalf("starved must suppress the panic snap, got %s sim=%d", d.Action, d.Sim)
	}
}

// Starvation also suppresses raises: a reading taken under pressure is
// unreliable in both directions.
func TestDecide_HostStarvedSuppressesRaise(t *testing.T) {
	d := Decide(State{Players: 1, MSPT: 7, CurrentSim: 10, CurrentView: 14, HostStarved: true}, cfg())
	if d.Action != ActionHold || d.Sim != 10 {
		t.Fatalf("starved must not raise either, got %s sim=%d", d.Action, d.Sim)
	}
}

// When the JVM's GC ate the poll window, the MSPT spike measures pause time,
// not world cost — hold, and point at the real fix.
func TestDecide_GCStalledHoldsInsteadOfLowering(t *testing.T) {
	d := Decide(State{Players: 4, MSPT: 48, CurrentSim: 14, CurrentView: 18,
		GCStalled: true, GCDetail: "GC ran 3000ms in the last 10s"}, cfg())
	if d.Action != ActionHold || d.Sim != 14 {
		t.Fatalf("GC-stalled should hold (not lower), got %s sim=%d", d.Action, d.Sim)
	}
	if !strings.Contains(d.Reason, "GC") || !strings.Contains(d.Reason, "tune") {
		t.Fatalf("reason should blame the collector and point at tune: %q", d.Reason)
	}
}

// PanicMSPT <= 0 disables the valve: a 55ms reading takes the normal bounded
// step down instead of snapping to the floor.
func TestDecide_PanicDisabledStepsNormally(t *testing.T) {
	c := cfg()
	c.PanicMSPT = 0
	d := Decide(State{Players: 4, MSPT: 55, CurrentSim: 14, CurrentView: 18}, c)
	if d.Action != ActionLower {
		t.Fatalf("over budget should lower, got %s (%s)", d.Action, d.Reason)
	}
	if d.Sim < 14-c.MaxStepDown {
		t.Fatalf("with panic disabled the drop must respect MaxStepDown, got %d", d.Sim)
	}
}

// MEMORY GATE TESTS

// When memory is pressured and view is above the sim+buffer floor, view steps
// down by one and the action becomes lower (even from a hold).
func TestDecide_MemPressureStepsViewDown(t *testing.T) {
	// MSPT 32 within band → sim hold. View 20 > floor (12+4=16) → view steps down.
	d := Decide(State{Players: 3, MSPT: 32, CurrentSim: 12, CurrentView: 20,
		MemPressured: true, MemDetail: "memory PSI some-avg10=25.0%"}, cfg())
	if d.Action != ActionLower {
		t.Fatalf("mem pressure on a hold should become lower, got %s", d.Action)
	}
	if d.Sim != 12 {
		t.Fatalf("mem pressure must not touch sim, got %d", d.Sim)
	}
	if d.View != 19 {
		t.Fatalf("view should step down 20→19, got %d", d.View)
	}
	if !strings.Contains(d.Reason, "memory PSI") {
		t.Fatalf("reason should carry the memory detail: %q", d.Reason)
	}
}

// When view is already at the sim+buffer floor, memory pressure does nothing.
func TestDecide_MemPressureAtFloorNoOp(t *testing.T) {
	// sim 12, view 16 = 12+4 = floor. Nothing to shed.
	d := Decide(State{Players: 3, MSPT: 32, CurrentSim: 12, CurrentView: 16,
		MemPressured: true}, cfg())
	if d.View != 16 {
		t.Fatalf("view at floor must not go lower, got %d", d.View)
	}
}

// Memory gate fires even during a HostStarved hold — orthogonal signals.
func TestDecide_MemPressureFiresDuringStarve(t *testing.T) {
	d := Decide(State{Players: 4, MSPT: 48, CurrentSim: 14, CurrentView: 20,
		HostStarved: true, StarveDetail: "I/O pressure",
		MemPressured: true, MemDetail: "memory PSI some-avg10=22.0%"}, cfg())
	if d.Sim != 14 {
		t.Fatalf("starvation must still hold sim, got %d", d.Sim)
	}
	if d.View != 19 {
		t.Fatalf("memory gate should step view 20→19 even during starvation hold, got %d", d.View)
	}
}

// With MemPressurePct == 0 (disabled), memory pressure is ignored.
func TestDecide_MemPressureDisabled(t *testing.T) {
	c := cfg()
	c.MemPressurePct = 0
	d := Decide(State{Players: 3, MSPT: 32, CurrentSim: 12, CurrentView: 20,
		MemPressured: true}, c)
	if d.View != 20 {
		t.Fatalf("disabled mem gate must not change view, got %d", d.View)
	}
	if d.Action != ActionHold {
		t.Fatalf("disabled mem gate must not change action, got %s", d.Action)
	}
}

// Memory gate appends to the reason when sim is also moving.
func TestDecide_MemPressureAppendsReasonOnLower(t *testing.T) {
	// MSPT 48 → lower sim; memory pressured → also step view down.
	d := Decide(State{Players: 4, MSPT: 48, CurrentSim: 14, CurrentView: 22,
		MemPressured: true, MemDetail: "memory PSI some-avg10=30.0%"}, cfg())
	if d.Action != ActionLower {
		t.Fatalf("over-budget + mem pressure should lower, got %s", d.Action)
	}
	if d.Sim >= 14 {
		t.Fatalf("sim should have lowered, got %d", d.Sim)
	}
	if d.View != 21 {
		t.Fatalf("view should step 22→21, got %d", d.View)
	}
	if !strings.Contains(d.Reason, "memory PSI") {
		t.Fatalf("reason should include mem detail: %q", d.Reason)
	}
}

// Package adaptive is the brain for Tier 3: scaling simulation distance live
// with the actual load, instead of picking one static value for the worst case.
// It decides; something else applies and measures.
//
// Why this is now safe to attempt: the measured model (docs/DECISION_TREE.md)
// tells us tick cost follows players × sim², so we don't have to react blindly.
// The controller's core guarantees:
//
//  1. SIM FLOORS. Sim never drops below the configured minimum, no matter the load.
//  2. ASYMMETRIC ramps. Drop fast when over budget (protect TPS), raise slowly
//     (one step) and only when comfortably under — so it never yo-yos.
//  3. DEADBAND. Small fluctuations around the target do nothing.
//  4. VIEW FOLLOWS SIM UP, never down — except when memory pressure is detected
//     (see MEMORY GATE below), in which case view steps down by one to shed chunk
//     load. It never goes below sim+ViewBuffer.
//
// Plus a safety valve: at/over PanicMSPT (the full 50ms tick budget) the server
// is actually dropping ticks, so sim snaps straight to the floor instead of
// stepping down — and the slow-raise ramp earns the distance back afterwards.
//
// And three cause-aware gates — the cross-layer awareness no in-game-only scaler
// has:
//   - HOST STARVATION: when cgroup PSI / throttling shows the host is the
//     bottleneck (State.HostStarved, fed by observe.StarvedNow), the MSPT reading
//     is unreliable — hold sim and do not act on it.
//   - GC STALL: when the JVM's garbage collector dominated the poll window
//     (State.GCStalled, fed by observe.GCStalledNow), the MSPT spike measures
//     pause time, not world cost — hold.
//   - MEMORY PRESSURE: when cgroup memory PSI is high (State.MemPressured, fed by
//     observe.MemPressuredNow), chunk load is pressing against the RAM budget —
//     step view down by one regardless of the sim/MSPT decision. This fires even
//     during HostStarved/GCStalled holds because memory pressure is orthogonal:
//     those gates say "sim reasoning is unreliable"; this gate says "load fewer
//     chunks because you're running out of RAM", which is always actionable.
//
// Decide is a pure function (no I/O), so it's fully unit-testable; the live loop
// that polls the companion and applies the result lives in the command layer.
package adaptive

import (
	"fmt"
	"math"
)

// State is a snapshot the controller reasons over (from the companion, plus
// the caller's cgroup-pressure reading).
type State struct {
	Players     int     // current online players
	PlayersPeak int     // expected peak (persisted) — size for this, not just current
	MSPT        float64 // current mean tick time, ms
	CurrentSim  int     // simulation-distance in effect
	CurrentView int     // view-distance in effect

	// HostStarved reports that cgroup PSI / CPU-quota throttling shows the
	// HOST, not the world, is the bottleneck right now (see observe.StarvedNow).
	// While true the controller holds: the sim² model assumes the tick thread
	// gets the CPU it asks for, so an MSPT reading taken under starvation says
	// nothing about world cost — cutting distance won't recover the ticks, and
	// raising would over-extend once the pressure lifts. StarveDetail is the
	// human explanation, carried into the decision's Reason.
	HostStarved  bool
	StarveDetail string

	// GCStalled reports that the JVM's garbage collector dominated the last
	// poll window (see observe.GCStalledNow). A long GC pause is a multi-tick
	// freeze that reads exactly like world load in the MSPT number, but a
	// smaller world won't stop the pauses — the fix is heap size / GC flags.
	// While true the controller holds, same as HostStarved.
	GCStalled bool
	GCDetail  string

	// MemPressured reports that cgroup memory PSI some-avg10 has crossed the
	// configured threshold, meaning tasks are actively stalling waiting for
	// memory. Unlike HostStarved/GCStalled (which say the MSPT reading is
	// unreliable), this is an actionable signal: view distance directly controls
	// how many chunks are loaded, so stepping it down reduces memory use without
	// touching sim or tick budget reasoning. MemDetail carries the human reason.
	MemPressured bool
	MemDetail    string
}

// Config bounds and tunes the controller. Floors are the render-drop protection.
type Config struct {
	TargetMSPT     float64 // aim to keep MSPT at/under this (headroom below the 50ms TPS budget)
	MinSim         int     // never go below this simulation distance
	MaxSim         int     // never exceed this
	ViewBuffer     int     // minimum lead of view over sim — raises view to sim+this, never cuts it
	MinView        int
	MaxView        int
	MaxStepDown    int     // largest sim decrease per decision (drop fast, but bounded)
	MaxStepUp      int     // largest sim increase per decision (raise cautiously, e.g. 1)
	Deadband       float64 // fraction below target that still counts as "fine" (no raise)
	PanicMSPT      float64 // at/over this, snap sim straight to MinSim (<=0 disables)
	MemPressurePct float64 // memory PSI some-avg10 % at/over which view steps down (0 disables)
}

// DefaultConfig is a conservative starting point: target 35ms (15ms of headroom
// under the 50ms budget), a sane render floor, and a fast-down / slow-up ramp.
func DefaultConfig() Config {
	return Config{
		TargetMSPT:     35,
		MinSim:         6,
		MaxSim:         16,
		ViewBuffer:     4,
		MinView:        8,
		MaxView:        24,
		MaxStepDown:    3,
		MaxStepUp:      1,
		Deadband:       0.2,
		PanicMSPT:      50,
		MemPressurePct: 20,
	}
}

// Action is what the decision asks for.
type Action string

const (
	ActionLower Action = "lower" // shed load: reduce sim distance
	ActionRaise Action = "raise" // spend headroom: increase sim distance
	ActionHold  Action = "hold"  // no change
)

// Decision is the controller's output: the target distances and why.
type Decision struct {
	Sim    int
	View   int
	Action Action
	Reason string
}

// Decide picks the next simulation/view distance from the current state.
//
// It uses the measured sim² relationship to self-calibrate: since MSPT ∝ sim²,
// the simulation distance that would land MSPT exactly on target is
// CurrentSim·√(target/MSPT). We move toward that — bounded by the step limits,
// the floors/ceilings, and the deadband — rather than guessing a fixed delta.
// This needs no hardcoded per-world constant; it reads the live cost.
//
// The function uses a single-return pattern so the MEMORY GATE post-processing
// (which may step view down even during a hold) applies to every decision path.
func Decide(s State, cfg Config) Decision {
	// view returns the target view for a given sim, never below the current view.
	// An operator-set view above MaxView is left alone rather than "corrected".
	view := func(sim int) int {
		v := clampInt(sim+cfg.ViewBuffer, cfg.MinView, cfg.MaxView)
		return max(v, s.CurrentView)
	}
	hold := func(reason string) Decision {
		return Decision{Sim: clampInt(s.CurrentSim, cfg.MinSim, cfg.MaxSim), View: view(s.CurrentSim), Action: ActionHold, Reason: reason}
	}

	var d Decision

	switch {
	case s.Players <= 0:
		d = hold("no players online — leaving settings as-is")

	case s.MSPT <= 0:
		d = hold("no MSPT reading — holding")

	case s.HostStarved:
		// HOST-STARVATION GATE. The MSPT reading reflects stolen CPU, not world
		// cost. Cutting distance punishes players without recovering ticks (this
		// includes the panic valve: 60ms of starved MSPT is still not the world's
		// fault), and the slow-raise ramp would then take many minutes to undo the
		// damage after the neighbour leaves. Hold everything and name the real fix.
		detail := s.StarveDetail
		if detail == "" {
			detail = "cgroup pressure/throttling"
		}
		d = hold(fmt.Sprintf("MSPT %.1f but the HOST is the bottleneck (%s) — distance won't fix that, holding; see `tickwarden host` / `iostorm`", s.MSPT, detail))

	case s.GCStalled:
		// GC-STALL GATE. When the collector ate the window, the MSPT spike
		// measures pause time, not world cost. The immediate, correct fix is heap
		// size / GC flags — a sim cut here would be slow-ramped back for nothing.
		detail := s.GCDetail
		if detail == "" {
			detail = "GC dominated the poll window"
		}
		d = hold(fmt.Sprintf("MSPT %.1f but the JVM's GC, not the world, ate the window (%s) — holding; fix heap/GC flags, see `tickwarden tune`", s.MSPT, detail))

	case cfg.PanicMSPT > 0 && s.MSPT >= cfg.PanicMSPT:
		// SAFETY VALVE. At/over PanicMSPT the server is dropping ticks outright
		// (50ms is the entire tick budget), and MSPT is a ~5s rolling mean, so
		// this is sustained overload, not a blip. Snap straight to the floor and
		// let the slow-raise ramp earn the distance back once the emergency passes.
		if s.CurrentSim <= cfg.MinSim {
			d = hold(fmt.Sprintf("MSPT %.1f at/over the %.0fms panic ceiling but already at the sim floor (%d)", s.MSPT, cfg.PanicMSPT, cfg.MinSim))
		} else {
			d = Decision{Sim: cfg.MinSim, View: view(cfg.MinSim), Action: ActionLower,
				Reason: fmt.Sprintf("MSPT %.1f at/over the %.0fms panic ceiling — ticks are being dropped: snap sim %d→%d (floor)", s.MSPT, cfg.PanicMSPT, s.CurrentSim, cfg.MinSim)}
		}

	default:
		// ON-JOIN PRE-SIZING. Size for the expected PEAK, not just who's on right
		// now. Player cost is linear (measured), so the MSPT the peak would produce
		// at the current distance is ~ currentMSPT × peak/current. We tune against
		// THAT, so the distance is already peak-safe and a join doesn't trigger a
		// cut. The ratio scaling slightly over-counts the fixed base cost, which
		// only makes it more conservative — safe. With no recorded peak it falls
		// back to current load.
		peak := s.PlayersPeak
		if peak < s.Players {
			peak = s.Players
		}
		if peak < 1 {
			peak = 1
		}
		projectedMSPT := s.MSPT * (float64(peak) / float64(s.Players))
		idealSim := float64(s.CurrentSim) * math.Sqrt(cfg.TargetMSPT/projectedMSPT)

		switch {
		case projectedMSPT > cfg.TargetMSPT:
			// Over budget — shed load fast (down to MaxStepDown), but never below floor.
			newSim := int(math.Floor(idealSim))
			if newSim < s.CurrentSim-cfg.MaxStepDown {
				newSim = s.CurrentSim - cfg.MaxStepDown
			}
			newSim = clampInt(newSim, cfg.MinSim, cfg.MaxSim)
			if newSim >= s.CurrentSim {
				d = hold(fmt.Sprintf("MSPT %.1f over target %.1f but already at the sim floor (%d)", s.MSPT, cfg.TargetMSPT, cfg.MinSim))
			} else {
				d = Decision{Sim: newSim, View: view(newSim), Action: ActionLower,
					Reason: fmt.Sprintf("MSPT %.1f (→%.1f projected for peak %d) > %.1f target: lower sim %d→%d", s.MSPT, projectedMSPT, peak, cfg.TargetMSPT, s.CurrentSim, newSim)}
			}

		case projectedMSPT < cfg.TargetMSPT*(1-cfg.Deadband):
			// Comfortably under — spend a little headroom, one cautious step, and
			// never past the distance that would reach target.
			newSim := s.CurrentSim + cfg.MaxStepUp
			if float64(newSim) > idealSim {
				newSim = int(math.Floor(idealSim))
			}
			newSim = clampInt(newSim, cfg.MinSim, cfg.MaxSim)
			if newSim <= s.CurrentSim {
				if s.CurrentSim >= cfg.MaxSim {
					d = hold(fmt.Sprintf("headroom (MSPT %.1f) but already at the sim ceiling (%d)", s.MSPT, cfg.MaxSim))
				} else {
					// There's headroom but not a full step's worth — a +1 would push
					// MSPT past target. Hold rather than overshoot.
					d = hold(fmt.Sprintf("MSPT %.1f under target but a +1 step would overshoot — holding sim %d", s.MSPT, s.CurrentSim))
				}
			} else {
				d = Decision{Sim: newSim, View: view(newSim), Action: ActionRaise,
					Reason: fmt.Sprintf("MSPT %.1f (→%.1f projected for peak %d) under %.1f target: raise sim %d→%d", s.MSPT, projectedMSPT, peak, cfg.TargetMSPT, s.CurrentSim, newSim)}
			}

		default:
			d = hold(fmt.Sprintf("MSPT %.1f within target band — holding sim %d", s.MSPT, s.CurrentSim))
		}
	}

	// MEMORY GATE (post-process). After the sim decision, independently step view
	// down when RAM pressure is detected. View distance controls how many chunks
	// are loaded, so reducing it directly lowers memory use without touching sim
	// or the tick budget calculation. This fires even during HostStarved/GCStalled
	// holds because memory pressure is orthogonal to MSPT reliability.
	// View never goes below sim+ViewBuffer (the floor that keeps it above sim).
	if s.MemPressured && cfg.MemPressurePct > 0 {
		floor := clampInt(d.Sim+cfg.ViewBuffer, cfg.MinView, cfg.MaxView)
		if d.View > floor {
			prev := d.View
			d.View = max(d.View-1, floor)
			detail := s.MemDetail
			if detail == "" {
				detail = "memory pressure"
			}
			if d.Action == ActionHold {
				d.Action = ActionLower
				d.Reason = fmt.Sprintf("view %d→%d: %s — shedding chunk load; sim held: %s", prev, d.View, detail, d.Reason)
			} else {
				d.Reason += fmt.Sprintf(" | view %d→%d: %s", prev, d.View, detail)
			}
		}
	}

	return d
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

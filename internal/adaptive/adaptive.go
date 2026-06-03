// Package adaptive is the (scaffold) brain for Tier 3: scaling simulation
// distance live with the actual load, instead of picking one static value for
// the worst case. It decides; something else applies and measures.
//
// Why this is now safe to attempt: the measured model (docs/DECISION_TREE.md)
// tells us tick cost follows players × sim², so we don't have to react blindly.
// And the controller never lets render distance crater — that was the whole
// fear with adaptive tuning ("people join, my view distance tanks"). Three
// guarantees prevent it:
//
//  1. FLOORS. Sim/view never drop below configured minimums, no matter the load.
//  2. ASYMMETRIC ramps. Drop fast when over budget (protect TPS), raise slowly
//     (one step) and only when comfortably under — so it never yo-yos.
//  3. DEADBAND. Small fluctuations around the target do nothing.
//
// Decide is a pure function (no I/O), so it's fully unit-testable; the live loop
// that polls the companion and applies the result lives in the command layer.
//
// NOTE (scaffold): applying a new simulation distance at runtime needs the
// companion mod to expose net.minecraft.server.PlayerManager#setSimulationDistance
// (and setViewDistance) — the engine supports changing them live, vanilla just
// has no command for it. That endpoint is the next build step; see docs/ADAPTIVE.md.
package adaptive

import (
	"fmt"
	"math"
)

// State is a snapshot the controller reasons over (from the companion).
type State struct {
	Players     int     // current online players
	MSPT        float64 // current mean tick time, ms
	CurrentSim  int     // simulation-distance in effect
	CurrentView int     // view-distance in effect
}

// Config bounds and tunes the controller. Floors are the render-drop protection.
type Config struct {
	TargetMSPT  float64 // aim to keep MSPT at/under this (headroom below the 50ms TPS budget)
	MinSim      int     // never go below this simulation distance
	MaxSim      int     // never exceed this
	ViewBuffer  int     // view distance = sim + this
	MinView     int
	MaxView     int
	MaxStepDown int     // largest sim decrease per decision (drop fast, but bounded)
	MaxStepUp   int     // largest sim increase per decision (raise cautiously, e.g. 1)
	Deadband    float64 // fraction below target that still counts as "fine" (no raise)
}

// DefaultConfig is a conservative starting point: target 35ms (15ms of headroom
// under the 50ms budget), a sane render floor, and a fast-down / slow-up ramp.
func DefaultConfig() Config {
	return Config{
		TargetMSPT:  35,
		MinSim:      6,
		MaxSim:      16,
		ViewBuffer:  4,
		MinView:     8,
		MaxView:     24,
		MaxStepDown: 3,
		MaxStepUp:   1,
		Deadband:    0.2,
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
func Decide(s State, cfg Config) Decision {
	view := func(sim int) int { return clampInt(sim+cfg.ViewBuffer, cfg.MinView, cfg.MaxView) }
	hold := func(reason string) Decision {
		return Decision{Sim: clampInt(s.CurrentSim, cfg.MinSim, cfg.MaxSim), View: view(s.CurrentSim), Action: ActionHold, Reason: reason}
	}

	// Don't tune what isn't loaded, and don't act on a missing reading.
	if s.Players <= 0 {
		return hold("no players online — leaving settings as-is")
	}
	if s.MSPT <= 0 {
		return hold("no MSPT reading — holding")
	}

	idealSim := float64(s.CurrentSim) * math.Sqrt(cfg.TargetMSPT/s.MSPT)

	switch {
	case s.MSPT > cfg.TargetMSPT:
		// Over budget — shed load fast (down to MaxStepDown), but never below floor.
		newSim := int(math.Floor(idealSim))
		if newSim < s.CurrentSim-cfg.MaxStepDown {
			newSim = s.CurrentSim - cfg.MaxStepDown
		}
		newSim = clampInt(newSim, cfg.MinSim, cfg.MaxSim)
		if newSim >= s.CurrentSim {
			return hold(fmt.Sprintf("MSPT %.1f over target %.1f but already at the sim floor (%d)", s.MSPT, cfg.TargetMSPT, cfg.MinSim))
		}
		return Decision{Sim: newSim, View: view(newSim), Action: ActionLower,
			Reason: fmt.Sprintf("MSPT %.1f > %.1f target (%d players): lower sim %d→%d", s.MSPT, cfg.TargetMSPT, s.Players, s.CurrentSim, newSim)}

	case s.MSPT < cfg.TargetMSPT*(1-cfg.Deadband):
		// Comfortably under — spend a little headroom, one cautious step, and
		// never past the distance that would reach target.
		newSim := s.CurrentSim + cfg.MaxStepUp
		if float64(newSim) > idealSim {
			newSim = int(math.Floor(idealSim))
		}
		newSim = clampInt(newSim, cfg.MinSim, cfg.MaxSim)
		if newSim <= s.CurrentSim {
			return hold(fmt.Sprintf("headroom (MSPT %.1f) but already at the sim ceiling (%d)", s.MSPT, cfg.MaxSim))
		}
		return Decision{Sim: newSim, View: view(newSim), Action: ActionRaise,
			Reason: fmt.Sprintf("MSPT %.1f well under %.1f target (%d players): raise sim %d→%d", s.MSPT, cfg.TargetMSPT, s.Players, s.CurrentSim, newSim)}

	default:
		return hold(fmt.Sprintf("MSPT %.1f within target band — holding sim %d", s.MSPT, s.CurrentSim))
	}
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

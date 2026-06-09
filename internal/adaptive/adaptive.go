// Package adaptive is the (scaffold) brain for Tier 3: scaling simulation
// distance live with the actual load, instead of picking one static value for
// the worst case. It decides; something else applies and measures.
//
// Why this is now safe to attempt: the measured model (docs/DECISION_TREE.md)
// tells us tick cost follows players × sim², so we don't have to react blindly.
// And the controller never lets render distance crater — that was the whole
// fear with adaptive tuning ("people join, my view distance tanks"). Four
// guarantees prevent it:
//
//  1. VIEW IS NEVER LOWERED. View distance costs bandwidth and RAM, not tick
//     CPU (see README "What it's tuning for"), so cutting it recovers zero
//     MSPT — it would be a player-visible render cut for nothing. Only sim
//     sheds load; view only ever ratchets up to keep its lead over sim.
//  2. FLOORS. Sim never drops below the configured minimum, no matter the load.
//  3. ASYMMETRIC ramps. Drop fast when over budget (protect TPS), raise slowly
//     (one step) and only when comfortably under — so it never yo-yos.
//  4. DEADBAND. Small fluctuations around the target do nothing.
//
// Plus a safety valve: at/over PanicMSPT (the full 50ms tick budget) the server
// is actually dropping ticks, so sim snaps straight to the floor instead of
// stepping down — and the slow-raise ramp earns the distance back afterwards.
//
// And a host-starvation gate: when cgroup PSI / throttling shows the HOST is
// the bottleneck (State.HostStarved, fed by observe.StarvedNow), the controller
// holds instead of acting — an MSPT reading taken under starvation measures
// stolen CPU, not world cost, and cutting distance can't give the ticks back.
// This is the cross-layer awareness no in-game-only scaler has.
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
}

// Config bounds and tunes the controller. Floors are the render-drop protection.
type Config struct {
	TargetMSPT  float64 // aim to keep MSPT at/under this (headroom below the 50ms TPS budget)
	MinSim      int     // never go below this simulation distance
	MaxSim      int     // never exceed this
	ViewBuffer  int     // minimum lead of view over sim — raises view to sim+this, never cuts it
	MinView     int
	MaxView     int
	MaxStepDown int     // largest sim decrease per decision (drop fast, but bounded)
	MaxStepUp   int     // largest sim increase per decision (raise cautiously, e.g. 1)
	Deadband    float64 // fraction below target that still counts as "fine" (no raise)
	PanicMSPT   float64 // at/over this, snap sim straight to MinSim (<=0 disables)
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
		PanicMSPT:   50,
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
	// View costs bandwidth and RAM, not tick CPU, so lowering it would not
	// recover any MSPT — it would only be a visible render cut. So view never
	// goes below where it already is; it only ratchets up when a sim raise
	// would otherwise outgrow its lead. (An operator-set view above MaxView is
	// likewise left alone rather than "corrected" downward.)
	view := func(sim int) int {
		v := clampInt(sim+cfg.ViewBuffer, cfg.MinView, cfg.MaxView)
		return max(v, s.CurrentView)
	}
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

	// HOST-STARVATION GATE. When the host — not the world — is the bottleneck,
	// every branch below would act on a lie: the MSPT reading reflects stolen
	// CPU, not world cost. Cutting distance punishes players without recovering
	// ticks (this includes the panic valve: 60ms of starved MSPT is still not
	// the world's fault), and the slow-raise ramp would then take many minutes
	// to undo the damage after the neighbour leaves. So hold everything and
	// name the real fix. Trade-off: if the world is ALSO genuinely overloaded
	// we hold over budget until the pressure lifts — accepted, because under
	// starvation we cannot tell, and a wrong cut is the harder error to undo.
	if s.HostStarved {
		detail := s.StarveDetail
		if detail == "" {
			detail = "cgroup pressure/throttling"
		}
		return hold(fmt.Sprintf("MSPT %.1f but the HOST is the bottleneck (%s) — distance won't fix that, holding; see `tickwarden host` / `iostorm`", s.MSPT, detail))
	}

	// SAFETY VALVE. At/over PanicMSPT the server is dropping ticks outright
	// (50ms is the entire tick budget), and MSPT is a ~5s rolling mean, so
	// this is sustained overload, not a blip. The sim² model may also be wrong
	// about the CAUSE here (an entity storm doesn't care about distance), so
	// don't trust a computed step: snap straight to the floor and let the
	// slow-raise ramp earn the distance back once the emergency passes.
	if cfg.PanicMSPT > 0 && s.MSPT >= cfg.PanicMSPT {
		if s.CurrentSim <= cfg.MinSim {
			return hold(fmt.Sprintf("MSPT %.1f at/over the %.0fms panic ceiling but already at the sim floor (%d)", s.MSPT, cfg.PanicMSPT, cfg.MinSim))
		}
		return Decision{Sim: cfg.MinSim, View: view(cfg.MinSim), Action: ActionLower,
			Reason: fmt.Sprintf("MSPT %.1f at/over the %.0fms panic ceiling — ticks are being dropped: snap sim %d→%d (floor)", s.MSPT, cfg.PanicMSPT, s.CurrentSim, cfg.MinSim)}
	}

	// ON-JOIN PRE-SIZING. Size for the expected PEAK, not just who's on right now.
	// Player cost is linear (measured), so the MSPT the peak would produce at the
	// current distance is ~ currentMSPT × peak/current. We tune against THAT, so
	// the distance is already peak-safe and a join doesn't trigger a cut — the
	// render-drop-on-join the operator was wary of. (Quiet times hold at the
	// peak-safe value rather than over-raising for a lone player.) The ratio
	// scaling slightly over-counts the fixed base cost, which only makes it more
	// conservative — safe. With no recorded peak it falls back to current load.
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
			return hold(fmt.Sprintf("MSPT %.1f over target %.1f but already at the sim floor (%d)", s.MSPT, cfg.TargetMSPT, cfg.MinSim))
		}
		return Decision{Sim: newSim, View: view(newSim), Action: ActionLower,
			Reason: fmt.Sprintf("MSPT %.1f (→%.1f projected for peak %d) > %.1f target: lower sim %d→%d", s.MSPT, projectedMSPT, peak, cfg.TargetMSPT, s.CurrentSim, newSim)}

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
				return hold(fmt.Sprintf("headroom (MSPT %.1f) but already at the sim ceiling (%d)", s.MSPT, cfg.MaxSim))
			}
			// There's headroom, but not a full step's worth — a +1 would push MSPT
			// past target. Hold rather than overshoot.
			return hold(fmt.Sprintf("MSPT %.1f under target but a +1 step would overshoot — holding sim %d", s.MSPT, s.CurrentSim))
		}
		return Decision{Sim: newSim, View: view(newSim), Action: ActionRaise,
			Reason: fmt.Sprintf("MSPT %.1f (→%.1f projected for peak %d) under %.1f target: raise sim %d→%d", s.MSPT, projectedMSPT, peak, cfg.TargetMSPT, s.CurrentSim, newSim)}

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

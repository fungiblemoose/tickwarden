package detect

import "fmt"

// This file turns a raw ModSet (see mods.go) into actionable advice. ScanMods
// answers "what is installed?"; the tuner consumes that to size distances. But
// an operator staring at a mods/ folder also wants to know "is this setup
// sane, and what am I missing?" — which is a different question, and a curated
// one. We answer it with a small, hand-maintained rules table rather than a
// live registry: the Minecraft perf-mod ecosystem moves, but the *shape* of a
// good server-side perf stack (one light engine, parallel chunks, tick + memory
// + network optimization, plus a profiler to measure with) is stable enough to
// bake in. Every rule below documents WHY it fires, because advice the operator
// can't reason about is advice they shouldn't trust.

// ModAdvice is the structured result of Advise. Each field is a slice of
// human-readable lines (already phrased for display); an empty slice means
// "nothing to report" for that category. Callers can print the non-empty
// categories under headers, or join everything for a flat report.
type ModAdvice struct {
	// Conflicts are mods that actively fight each other — installing both is
	// not just wasteful but can cause crashes, mixin failures, or undefined
	// behavior. These should be resolved before anything else.
	Conflicts []string
	// Redundant are mods that overlap in function without strictly conflicting:
	// keeping both wastes memory/startup time and muddies profiling, but won't
	// necessarily break. Softer than a conflict.
	Redundant []string
	// Suggestions are missing performance mods worth adding, each with a short
	// "why" so the operator can decide whether the trade-off fits their server.
	Suggestions []string
	// Notes are caveats and context about what IS installed — neither a problem
	// nor a recommendation, just things worth knowing when tuning.
	Notes []string
}

// Empty reports whether the advice has nothing at all to say. Useful for
// callers that want to print a "looks good" line instead of four empty headers.
func (a ModAdvice) Empty() bool {
	return len(a.Conflicts) == 0 &&
		len(a.Redundant) == 0 &&
		len(a.Suggestions) == 0 &&
		len(a.Notes) == 0
}

// Advise inspects the detected mod set and returns curated advice: conflicts to
// resolve, redundancy to trim, and missing perf mods to consider. It is a pure
// function of the ModSet — no I/O, no version awareness (see AdviseFor for
// that) — so it is trivially testable and safe to call anywhere.
//
// The rules deliberately stay honest about uncertainty: a "suggestion" is a
// default-good recommendation, not a mandate, and each carries its rationale so
// the operator can override it for their workload.
func Advise(m ModSet) ModAdvice {
	var a ModAdvice

	// --- Lighting engine ---
	// Starlight and ScalableLux both REPLACE Minecraft's light engine — they
	// can't coexist: whichever loads second either no-ops or, worse, collides
	// with the first's mixins. Running both is the canonical "two light engines"
	// mistake, so this is a hard conflict, not mere redundancy.
	hasStarlight := m.Has("starlight")
	hasScalableLux := m.Has("scalablelux")
	switch {
	case hasStarlight && hasScalableLux:
		a.Conflicts = append(a.Conflicts,
			"two lighting engines (starlight + scalablelux) both replace the light engine — keep exactly one")
	case !hasStarlight && !hasScalableLux:
		// With no light-engine mod, lighting updates run vanilla on the main
		// thread and can stall ticks during heavy chunk churn. Either option is
		// a strict improvement; we name both rather than pick for the operator.
		a.Suggestions = append(a.Suggestions,
			"no lighting mod: add starlight (single-thread rewrite) or scalablelux (parallel) to keep lighting off the tick path")
	}

	// --- Chunk performance ---
	// C2ME parallelizes chunk generation and I/O across cores. On any host with
	// spare cores (i.e. almost all of them) it is the single biggest lever for
	// world-gen and load stalls, which is also why the tuner keys its per-core
	// budget on chunk-perf mods being present.
	if !m.Has("c2me") {
		a.Suggestions = append(a.Suggestions,
			"no c2me: add it to parallelize chunk generation and I/O across cores (biggest win on multi-core hosts)")
	}

	// --- Tick loop ---
	// Lithium optimizes the tick loop while preserving vanilla behavior — it is
	// the safe baseline every server should run, with effectively no downside,
	// so its absence is always worth flagging.
	if !m.Has("lithium") {
		a.Suggestions = append(a.Suggestions,
			"no lithium: add it for vanilla-parity tick optimization — the safe baseline with no behavior change")
	}

	// --- Memory ---
	// FerriteCore shrinks in-memory data structures with no behavior change,
	// reclaiming RAM that headroom-starved servers badly need. Pure upside.
	if !m.Has("ferritecore") {
		a.Suggestions = append(a.Suggestions,
			"no ferritecore: add it to cut RAM use with no behavior change")
	}

	// --- Network ---
	// Krypton optimizes the networking stack, reducing per-packet overhead.
	// Matters most as connection count and player movement climb.
	if !m.Has("krypton") {
		a.Suggestions = append(a.Suggestions,
			"no krypton: add it to optimize the networking stack")
	}

	// --- Profiling ---
	// Spark is the profiler. You can't tune what you can't measure, and
	// `tickwarden bench` leans on it — so a server without spark is one you're
	// optimizing blind. Flag it even when everything else is present.
	if !m.Has("spark") {
		a.Suggestions = append(a.Suggestions,
			"no spark: add the profiler — you can't tune what you can't measure (also used by `tickwarden bench`)")
	}

	// --- ServerCore context ---
	// ServerCore scales simulation distance dynamically on its own — the same
	// actuator `tickwarden adaptive`/`daemon -apply` drives. Two controllers
	// writing the same setting will fight (one raises, the other cuts, repeat),
	// and neither's decisions will make sense in isolation. The jar itself is
	// fine — the conflict is with OUR apply mode — so this is a note, not a
	// Conflicts entry: it only bites if the operator also enables -apply.
	if m.Has("servercore") {
		a.Notes = append(a.Notes,
			"servercore installed: it scales simulation distance dynamically itself — don't also run `tickwarden adaptive -apply` or `daemon -apply`, or two controllers will fight over the same setting; pick one")
	}

	// --- VMP context ---
	// Very Many Players targets high concurrent-player scaling. It is not wrong
	// on a small server, but its wins don't show up until player counts are
	// high — so when it's installed we note that rather than crediting it as a
	// general perf mod.
	if m.Has("vmp") {
		a.Notes = append(a.Notes,
			"vmp installed: mainly helps at higher player counts — limited benefit on small servers")
	}

	return a
}

// AdviseFor is a version-aware wrapper around Advise. It returns the same base
// advice and layers on a small set of curated, version-specific notes. It
// stays deliberately simple — a hand-maintained rules set, not a live
// compatibility database — so it can lie only by going stale, never by
// pretending to know more than it does. An empty mcVersion yields plain Advise.
func AdviseFor(m ModSet, mcVersion string) ModAdvice {
	a := Advise(m)
	if mcVersion == "" {
		return a
	}
	// Starlight was folded into / superseded by other engines on modern
	// versions in some loaders; rather than assert exact cutoffs (which would
	// rot), we just remind the operator to confirm each mod publishes a build
	// for their target version — the one check filename detection can't do.
	a.Notes = append(a.Notes,
		fmt.Sprintf("targeting MC %s: confirm every installed/suggested mod publishes a build for this version before deploying", mcVersion))
	return a
}

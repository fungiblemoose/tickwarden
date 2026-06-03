// Package tune turns a detected host Profile into concrete server-tuning
// recommendations. Every recommendation carries a Reason: the whole point of
// tickwarden is that the *why* is legible, not just the value. The rules here
// are the encoded decision tree documented in docs/DECISION_TREE.md.
//
// These heuristics are opinionated and some are contested (the per-core
// distance budget especially). Where a rule is folklore rather than measured,
// the Reason says so. Validate with a repeatable load before trusting them.
package tune

import (
	"fmt"
	"math"

	"github.com/fungiblemoose/tickwarden/internal/confidence"
	"github.com/fungiblemoose/tickwarden/internal/detect"
)

// Confidence flags how much to trust a recommendation. Aliased from the shared
// confidence package so the const names below (Solid/Heuristic/Contested) and
// existing callers keep working unchanged.
type Confidence = confidence.Confidence

const (
	Solid     = confidence.Solid     // well-established, low risk
	Heuristic = confidence.Heuristic // reasonable default, tune to taste
	Contested = confidence.Contested // folklore; validate against your load
)

// Target says where a recommendation is applied — which is what decides
// whether `tune --apply` can write it automatically or can only advise.
type Target string

const (
	TargetProperties Target = "server.properties" // a key in server.properties — auto-appliable
	TargetJVM        Target = "jvm-flags"         // a JVM flag in the launch command/unit
	TargetModConfig  Target = "mod-config"        // a mod's config file (e.g. c2me.toml)
	TargetManual     Target = "manual"            // requires a human (e.g. swapping a mod jar)
	TargetInfo       Target = "info"              // an observation/warning, nothing to apply
)

// Rec is a single tuning recommendation.
type Rec struct {
	Key        string     `json:"key"`
	Value      string     `json:"value"`
	Reason     string     `json:"reason"`
	Confidence Confidence `json:"confidence"`
	Target     Target     `json:"target"`
}

// Plan is the full set of recommendations for a host.
type Plan struct {
	Profile detect.Profile `json:"profile"`
	Recs    []Rec          `json:"recommendations"`
}

// Options carries the workload assumptions the rules can't read off the host:
// how many players to size for, and whether chunk-system perf mods are present.
type Options struct {
	// Players is the expected PEAK concurrent count. Sizing for spread-out
	// players is the conservative case; clustered players cost far less.
	Players int
	// PerfMods reports whether C2ME / ScalableLux et al. are installed — they
	// raise the per-core budget by moving chunk gen off the main tick thread.
	PerfMods bool
	// PlayersAuto records that Players came from a live measurement (the
	// companion's observed peak) rather than a guess, for the reason string.
	PlayersAuto bool
	// Clustered says the playerbase tends to congregate (a shared base, a hub,
	// an event) rather than scatter to explore. Spread-out players each tick
	// their own disjoint area, so cost scales with the full player count; a
	// cluster overlaps — their loaded/simulated chunks coincide — so the server
	// effectively ticks fewer independent areas and can afford a higher sim
	// distance. Default false is the CONSERVATIVE assumption (size for spread);
	// set it only when you know the social shape of your server.
	Clustered bool
}

// DefaultOptions assumes a small friends-server with perf mods — the common
// case for anyone reaching for this tool.
func DefaultOptions() Options { return Options{Players: 4, PerfMods: true} }

const gib = 1 << 30

// Recommend runs the rules engine over a profile and workload assumptions.
func Recommend(p detect.Profile, opts Options) Plan {
	if opts.Players < 1 {
		opts.Players = 1
	}
	plan := Plan{Profile: p}
	add := func(k, v, reason string, c Confidence, t Target) {
		plan.Recs = append(plan.Recs, Rec{Key: k, Value: v, Reason: reason, Confidence: c, Target: t})
	}

	// --- Heap sizing ---------------------------------------------------------
	// Plan against the memory budget (cgroup limit if set, else host RAM) and
	// leave headroom for off-heap: Netty direct buffers, mmap'd region files,
	// metaspace, and the OS page cache that makes chunk I/O fast. Taking 80% of
	// RAM is a classic mistake — the page cache is not free real estate.
	budget := p.MemoryBudgetBytes
	heap := heapFor(budget, opts.Players)
	if budget == 0 {
		add("Xmx/Xms", "unknown", "couldn't read a memory budget; deploy on the Linux target to size the heap", Contested, TargetInfo)
	} else {
		heapGiB := float64(heap) / gib
		add("Xmx", fmt.Sprintf("%.0fG", heapGiB),
			fmt.Sprintf("sized to load (~%d peak players) within the %.1f GiB budget, not 'all RAM minus a sliver' — an over-large heap you don't fill just starves the page cache that keeps chunk reads warm. If `watch`/spark shows the heap running hot, raise it.", opts.Players, float64(budget)/gib),
			Heuristic, TargetJVM)
		add("Xms", fmt.Sprintf("%.0fG", heapGiB),
			"pin Xms == Xmx (Aikar): pre-committing the heap avoids GC churn from growing it under load", Solid, TargetJVM)
	}

	// --- Garbage collector ---------------------------------------------------
	// G1 with Aikar's flags is the proven default for small/medium heaps.
	// Above ~12 GiB, Generational ZGC's sub-millisecond pauses win — large G1
	// heaps produce the multi-100ms stop-the-worlds that show up as TPS dips.
	if heap > 0 {
		if heap >= 12*gib {
			add("gc", "Generational ZGC (-XX:+UseZGC -XX:+ZGenerational)",
				"heap >= 12 GiB: ZGC's pause times stay sub-ms where G1 starts producing TPS-visible stop-the-world pauses", Heuristic, TargetJVM)
		} else {
			add("gc", "G1 + Aikar flags",
				"heap < 12 GiB: G1 with Aikar's tuned flags is the proven low-risk default at this size", Solid, TargetJVM)
		}
	}

	// --- Distances -----------------------------------------------------------
	// Simulation distance is the expensive one (entity ticking, redstone, mob
	// AI scale with it); view distance is comparatively cheap (just sending
	// chunks). Keep sim < view.
	cores := p.EffectiveCores
	ssd := p.StorageRotational != nil && !*p.StorageRotational
	view, sim := distancesFor(cores, opts.Players, opts.PerfMods, opts.Clustered, ssd)
	add("view-distance", fmt.Sprintf("%d", view),
		"maxed for render range — view distance costs bandwidth + RAM, not tick CPU (cheap disk loads over pregenerated chunks on SSD); pushed well above sim", Heuristic, TargetProperties)
	add("simulation-distance", fmt.Sprintf("%d", sim),
		fmt.Sprintf("sized for %s%d peak players on %.1f cores%s%s: per-tick cost scales with players × sim², so safe sim ∝ sqrt(budget/players); validate with `bench`",
			autoTag(opts.PlayersAuto), opts.Players, cores, perfModsTag(opts.PerfMods), clusteredTag(opts.Clustered)),
		Solid, TargetProperties)

	// --- Chunk system parallelism (C2ME) -------------------------------------
	// C2ME moves chunk generation and I/O off the single main tick thread, so it
	// helps even on few cores (measured ~4x better tail latency on a 3-core box).
	// Keep it on; just size the worker pool to leave the main thread + GC
	// headroom rather than disabling it. Reserve ~1 core on small boxes, ~2 on
	// bigger ones.
	var workers int
	if cores >= 4 {
		workers = int(math.Floor(cores)) - 2
	} else {
		workers = int(math.Floor(cores)) - 1
	}
	if workers < 1 {
		workers = 1
	}
	add("c2me.threads", fmt.Sprintf("%d", workers),
		fmt.Sprintf("%.1f effective cores: keep C2ME — its gen/I/O runs off the tick thread, so it helps even here; size the pool to leave the main thread + GC a core", cores),
		Heuristic, TargetModConfig)

	// --- Lighting engine -----------------------------------------------------
	// ScalableLux is the actively-maintained light-engine rewrite (a Starlight
	// fork). On many cores it parallelizes lighting; on few it performs like
	// single-threaded Starlight with no downside. Recommend it across the board:
	// bare Starlight frequently has no build for current Minecraft versions, so
	// recommending it by core count (as before) could point at a mod you can't
	// install.
	add("lighting-engine", "ScalableLux",
		"the maintained light-engine rewrite: parallelizes lighting on many cores, behaves like Starlight on few, and unlike bare Starlight ships builds for current MC versions",
		Heuristic, TargetManual)

	// --- Storage -------------------------------------------------------------
	if p.StorageRotational != nil {
		if *p.StorageRotational {
			add("storage-warning", "rotational disk detected",
				"region-file I/O on spinning rust will stall the tick thread on chunk save/load; lower view+sim distance and watch io.pressure", Solid, TargetInfo)
		} else {
			add("storage", "SSD/NVMe",
				"low-latency storage: chunk I/O is unlikely to be your tick bottleneck", Solid, TargetInfo)
		}
	}

	// --- Container caveat ----------------------------------------------------
	if p.Virt == detect.VirtLXC || p.Virt == detect.VirtDocker {
		add("note:contention", string(p.Virt),
			"containerized: a quiet modpack can still stutter if a neighbor saturates the shared CPU/IO pool — run `tickwarden watch` to catch host-side starvation", Solid, TargetInfo)
	}

	return plan
}

// heapFor sizes the JVM heap to expected LOAD within the memory budget, rather
// than grabbing all RAM minus a sliver. Heap need scales with how much is loaded
// and ticking, which scales with players; a heap far bigger than you fill is not
// free — it steals the OS page cache that keeps chunk reads off disk. So:
//
//	heap = clamp( 2 GiB + 0.5 GiB/player , 1 GiB , budget − reserve )
//	reserve = max(1.5 GiB, 25% of budget)   // off-heap (Netty/mmap/metaspace) + page cache
//
// This is an upper-ish estimate, not a mandate: if `watch`/spark shows the heap
// running hot, raise it. The point is to stop recommending (say) 6 GiB on an
// 8 GiB box that only ever uses 1 GiB.
func heapFor(budget uint64, players int) uint64 {
	if budget == 0 {
		return 0
	}
	if players < 1 {
		players = 1
	}
	reserve := uint64(float64(budget) * 0.25)
	if min := uint64(1.5 * gib); reserve < min {
		reserve = min
	}
	if reserve >= budget {
		return (budget / 2 / gib) * gib
	}
	cap := budget - reserve

	need := uint64(2*gib) + uint64(players)*(gib/2)
	heap := need
	if heap > cap {
		heap = cap
	}
	if heap < gib {
		heap = gib
	}
	// Round down to whole GiB for a clean flag value.
	return (heap / gib) * gib
}

// distancesFor sizes simulation and view distance to BOTH the host's
// parallelism and the expected peak player load.
//
// Per-tick simulation cost grows with players × sim_distance² — each player
// independently ticks the entities/redstone/mobs in their own area, and that
// area scales with the square of the distance. So the safe simulation distance
// falls as sqrt(core_budget / players): one player can run a high sim distance
// that melts a server full of spread-out players. The per-core budget is higher
// when chunk-system perf mods (C2ME et al.) move generation off the main tick
// thread.
//
// PLAYER SPREAD: the players term assumes every player ticks a DISJOINT area
// (the spread-out, exploring case — the worst case for the tick loop). When the
// caller knows players congregate, their simulated areas overlap and the server
// ticks fewer independent regions, so we size against an EFFECTIVE player count
// of ceil(players/2) — a deliberately conservative "half overlap" factor (not
// zero: a cluster still has edges and stragglers). A lower effective count
// raises sqrt(budget/players) and therefore the recommended sim distance.
//
// CALIBRATION ANCHOR: budgetPerCore=64 is set so 3 effective cores / 2 peak
// players / perf-mods yields sim≈10 — a real, spark-validated data point on the
// reference server that holds 20 TPS (median tick ~7ms). The clustered factor
// is gated so that the default (spread) path is byte-identical to that anchor.
// Re-derive this constant if `bench` ever contradicts it; see
// docs/DECISION_TREE.md.
func distancesFor(cores float64, players int, perfMods, clustered, ssd bool) (view, sim int) {
	if players < 1 {
		players = 1
	}
	budgetPerCore := 64.0
	if !perfMods {
		budgetPerCore = 32.0 // vanilla gen hits the main thread; ~halve the budget
	}
	effPlayers := players
	if clustered {
		// Half-overlap: a clustered group costs about half its head count in
		// independent ticking areas. max(1,...) keeps the divisor sane.
		effPlayers = (players + 1) / 2
		if effPlayers < 1 {
			effPlayers = 1
		}
	}
	sim = clampInt(int(math.Round(math.Sqrt(budgetPerCore*cores/float64(effPlayers)))), 5, 16)
	// View distance is cheap on CPU (chunk sends, not ticking) — and cheaper
	// still over pregenerated chunks on SSD, where it's just disk loads. The
	// philosophy is MAX render: push view well above sim. The real ceiling is
	// RAM (each extra ring of chunks costs memory), so we keep a sane cap rather
	// than going to vanilla's 32; SSD earns a higher cap because loads are cheap.
	viewBonus, viewCap := 6, 16
	if ssd {
		viewBonus, viewCap = 10, 24
	}
	view = clampInt(sim+viewBonus, 8, viewCap)
	return view, sim
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

func autoTag(auto bool) string {
	if auto {
		return "measured "
	}
	return "~"
}

func perfModsTag(perfMods bool) string {
	if perfMods {
		return " (with perf mods)"
	}
	return " (no perf mods)"
}

func clusteredTag(clustered bool) string {
	if clustered {
		return ", assuming clustered play (sized for ~half as many independent areas)"
	}
	return ""
}

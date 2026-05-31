// Package tune turns a detected host Profile into concrete server-tuning
// recommendations. Every recommendation carries a Reason: the whole point of
// tickwarden is that the *why* is legible, not just the value. The rules here
// are the encoded decision tree documented in docs/DECISION_TREE.md.
//
// These heuristics are opinionated and some are contested (lighting engine
// choice especially). Where a rule is folklore rather than measured, the
// Reason says so. Validate with a repeatable load before trusting them.
package tune

import (
	"fmt"
	"math"

	"github.com/fungiblemoose/tickwarden/internal/detect"
)

// Confidence flags how much to trust a recommendation.
type Confidence string

const (
	Solid     Confidence = "solid"     // well-established, low risk
	Heuristic Confidence = "heuristic" // reasonable default, tune to taste
	Contested Confidence = "contested" // folklore; validate against your load
)

// Rec is a single tuning recommendation.
type Rec struct {
	Key        string     `json:"key"`
	Value      string     `json:"value"`
	Reason     string     `json:"reason"`
	Confidence Confidence `json:"confidence"`
}

// Plan is the full set of recommendations for a host.
type Plan struct {
	Profile detect.Profile `json:"profile"`
	Recs    []Rec          `json:"recommendations"`
}

const gib = 1 << 30

// Recommend runs the rules engine over a profile.
func Recommend(p detect.Profile) Plan {
	plan := Plan{Profile: p}
	add := func(k, v, reason string, c Confidence) {
		plan.Recs = append(plan.Recs, Rec{Key: k, Value: v, Reason: reason, Confidence: c})
	}

	// --- Heap sizing ---------------------------------------------------------
	// Plan against the memory budget (cgroup limit if set, else host RAM) and
	// leave headroom for off-heap: Netty direct buffers, mmap'd region files,
	// metaspace, and the OS page cache that makes chunk I/O fast. Taking 80% of
	// RAM is a classic mistake — the page cache is not free real estate.
	budget := p.MemoryBudgetBytes
	heap := heapForBudget(budget)
	if budget == 0 {
		add("Xmx/Xms", "unknown", "couldn't read a memory budget; deploy on the Linux target to size the heap", Contested)
	} else {
		heapGiB := float64(heap) / gib
		add("Xmx", fmt.Sprintf("%.0fG", heapGiB),
			fmt.Sprintf("budget %.1f GiB; reserve the rest for off-heap (Netty/mmap/metaspace) and the OS page cache that keeps chunk reads warm", float64(budget)/gib),
			Solid)
		add("Xms", fmt.Sprintf("%.0fG", heapGiB),
			"pin Xms == Xmx (Aikar): pre-committing the heap avoids GC churn from growing it under load", Solid)
	}

	// --- Garbage collector ---------------------------------------------------
	// G1 with Aikar's flags is the proven default for small/medium heaps.
	// Above ~12 GiB, Generational ZGC's sub-millisecond pauses win — large G1
	// heaps produce the multi-100ms stop-the-worlds that show up as TPS dips.
	if heap > 0 {
		if heap >= 12*gib {
			add("gc", "Generational ZGC (-XX:+UseZGC -XX:+ZGenerational)",
				"heap >= 12 GiB: ZGC's pause times stay sub-ms where G1 starts producing TPS-visible stop-the-world pauses", Heuristic)
		} else {
			add("gc", "G1 + Aikar flags",
				"heap < 12 GiB: G1 with Aikar's tuned flags is the proven low-risk default at this size", Solid)
		}
	}

	// --- Distances -----------------------------------------------------------
	// Simulation distance is the expensive one (entity ticking, redstone, mob
	// AI scale with it); view distance is comparatively cheap (just sending
	// chunks). Keep sim < view.
	cores := p.EffectiveCores
	view, sim := distancesForCores(cores)
	add("view-distance", fmt.Sprintf("%d", view),
		"view distance mostly costs bandwidth + chunk sends, not CPU; can run higher than sim", Heuristic)
	add("simulation-distance", fmt.Sprintf("%d", sim),
		"sim distance drives entity/redstone/AI ticking — the real per-tick CPU cost; keep it below view distance", Solid)

	// --- Chunk system parallelism (C2ME) -------------------------------------
	// Size worker pools to effective cores but always leave the main server
	// thread headroom — starving the tick loop to feed chunk gen is a net loss.
	if cores >= 4 {
		workers := int(math.Max(2, math.Floor(cores)-2))
		add("c2me.threads", fmt.Sprintf("%d", workers),
			fmt.Sprintf("%.1f effective cores: leave ~2 for the main tick thread + GC so chunk gen doesn't starve the loop it feeds", cores),
			Heuristic)
	} else {
		add("c2me", "consider disabling",
			fmt.Sprintf("only %.1f effective cores: parallel chunk I/O contends with the main thread more than it helps", cores),
			Heuristic)
	}

	// --- Lighting engine (the contested one) ---------------------------------
	// Starlight is the faster single-threaded rewrite; ScalableLux parallelizes
	// lighting across cores. The crossover point is genuinely not well
	// measured — flag it as contested and validate.
	if cores >= 8 {
		add("lighting-engine", "ScalableLux",
			fmt.Sprintf("%.0f cores: parallel lighting *should* beat single-threaded Starlight here — but this crossover is folklore, benchmark it", cores),
			Contested)
	} else {
		add("lighting-engine", "Starlight",
			fmt.Sprintf("%.0f effective cores: not enough parallelism to amortize ScalableLux's coordination; Starlight's faster single-thread path wins", cores),
			Contested)
	}

	// --- Storage -------------------------------------------------------------
	if p.StorageRotational != nil {
		if *p.StorageRotational {
			add("storage-warning", "rotational disk detected",
				"region-file I/O on spinning rust will stall the tick thread on chunk save/load; lower view+sim distance and watch io.pressure", Solid)
		} else {
			add("storage", "SSD/NVMe",
				"low-latency storage: chunk I/O is unlikely to be your tick bottleneck", Solid)
		}
	}

	// --- Container caveat ----------------------------------------------------
	if p.Virt == detect.VirtLXC || p.Virt == detect.VirtDocker {
		add("note:contention", string(p.Virt),
			"containerized: a quiet modpack can still stutter if a neighbor saturates the shared CPU/IO pool — run `tickwarden watch` to catch host-side starvation", Solid)
	}

	return plan
}

// heapForBudget reserves headroom for off-heap memory and the page cache,
// scaling the reservation with total budget.
func heapForBudget(budget uint64) uint64 {
	if budget == 0 {
		return 0
	}
	// Reserve max(1.5 GiB, 25% of budget) for off-heap + page cache.
	reserve := uint64(float64(budget) * 0.25)
	if min := uint64(1.5 * gib); reserve < min {
		reserve = min
	}
	if reserve >= budget {
		return budget / 2
	}
	heap := budget - reserve
	// Round down to whole GiB for a clean flag value.
	return (heap / gib) * gib
}

func distancesForCores(cores float64) (view, sim int) {
	switch {
	case cores >= 8:
		return 12, 8
	case cores >= 4:
		return 10, 6
	default:
		return 8, 5
	}
}

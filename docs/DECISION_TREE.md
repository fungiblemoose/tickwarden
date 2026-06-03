# The tuning decision tree

This is the asset. The Go code in `internal/tune` is just this document, executed.
Writing it down first is the test of whether the "intelligence" is a couple dozen
rules (productizable) or a thousand judgment calls (not). So far it looks like the
former.

Each rule lists its **inputs**, the **decision**, the **why**, and a **confidence**:

- **solid** — well established, low risk, apply by default.
- **heuristic** — a sane default; reasonable people tune it.
- **contested** — folklore we haven't measured; *validate before trusting* (Tier 1.5).

Every rule plans against the **cgroup-bounded** numbers, not the host's physical
specs. Inside an LXC/Docker container your real ceiling is the quota, not the box.

---

## 1. Heap size (`-Xmx` / `-Xms`) — *heuristic*

**Inputs:** memory budget (cgroup `memory.max` if set, else `MemTotal`), expected
peak players.

**Decision:**
```
Xmx = clamp( 2 GiB + 0.5 GiB/player , 1 GiB , budget − max(1.5 GiB, 25% of budget) )
Xms = Xmx
```
rounded down to whole GiB. Size to *load*, not to "all RAM minus a sliver."

**Why:** the JVM heap is not the only consumer. You must leave headroom for:
- **Netty direct buffers** (off-heap, scale with player count/network),
- **mmap'd region files** (chunk storage is memory-mapped),
- **Metaspace / thread stacks / GC structures**,
- and most importantly the **OS page cache** — the thing that keeps chunk reads
  off disk. Handing 80% of RAM to the heap is the classic mistake: it starves the
  page cache and you trade GC headroom for disk I/O stalls.

A heap far larger than you actually fill makes this *worse*, not safer — the
unused commitment is page cache you gave up for nothing. So size to load (which
tracks player count) and treat the result as a starting point: if `watch`/spark
shows the heap running hot, raise it. (On the reference 8 GiB / 2-player server
this recommends 3 GiB, matching real usage of ~1.1 GiB, rather than 6 GiB.)

`Xms == Xmx` (Aikar) pre-commits the heap so the JVM never pays to grow it mid-tick.

---

## 2. Garbage collector — *heuristic*

**Inputs:** resolved heap size.

**Decision:**
- heap **< 12 GiB** → **G1 with Aikar's flags**.
- heap **≥ 12 GiB** → **Generational ZGC** (`-XX:+UseZGC -XX:+ZGenerational`).

**Why:** G1 with Aikar's tuned flags is the proven, low-risk default and is great
up through medium heaps. But large G1 heaps produce multi-hundred-millisecond
stop-the-world pauses, and a 300 ms pause *is* a 6-tick freeze — it shows up
directly as a TPS dip. ZGC keeps pauses sub-millisecond regardless of heap size,
so once the heap is big enough that G1 pauses get tick-visible, ZGC wins.

The 12 GiB crossover is a reasonable rule of thumb, not a measured constant.

---

## 3. View distance vs. simulation distance — *sim is solid, view is heuristic*

**Inputs:** effective cores, **expected peak concurrent players**, perf-mods present.

**Decision:**

```
sim = clamp( round( sqrt( budgetPerCore × cores / players ) ), 5, 16 )
view = clamp( sim + 4, 8, 20 )
budgetPerCore = 64 with perf mods, 32 without
```

| cores | players | perf mods | → sim | view |
|---|---|---|---|---|
| 3 | 2  | yes | 10 | 14 |
| 3 | 4  | yes | 7  | 11 |
| 3 | 16 | yes | 5  | 9  |
| 8 | 4  | yes | 11 | 15 |
| 8 | 4  | no  | 8  | 12 |

**Why:** these two knobs have very different costs, and the cost depends on
**how many players are loaded at once**, not just the hardware. **Simulation
distance** drives entity ticking, redstone, mob AI, block updates, fluid flow —
the real per-tick CPU work — and each player independently ticks the area around
them, which scales with the *square* of the distance. So total per-tick cost ≈
`players × sim²`, and the safe sim distance falls as `sqrt(budget / players)`:
one player can run a sim distance that would melt a server full of spread-out
players. (Clustered players overlap and cost far less; the model sizes for the
conservative spread-out case.) **View distance** is mostly bandwidth and chunk
sends — comparatively cheap on CPU, cheaper still over pregenerated chunks — so
it sits a few above sim for visual range.

The previous flat per-core table ignored player count and so badly
under-recommended for low-population servers (it suggested view-8 where a
hand-tuned view-20 holds 20 TPS at 1–2 players). Player count was the missing
variable.

> **Auto-detect:** the companion mod reports `players_peak`, so
> `tune -players-url <endpoint>` sizes to the server's *measured* peak instead
> of a guess — configure for the load you actually get, not an assumed one.

> **Play style (`-settled`):** the default budget is the FLYING worst case —
> limited by chunk-*generation* spikes when players cross chunks fast, not by
> simulation cost. Survival play in pregenerated terrain (walking, not flying)
> avoids those spikes (the A/B's stationary bot held sim-10 at a flat 6 ms,
> sim-14 at 9 ms, no spikes), so `-settled` roughly doubles the budget. Marked
> `contested`: the 2× is reasoned from the gen-spike data, not cleanly A/B'd
> (Carpet bots walk too slowly to reproduce flying gen load directly). Only set
> it if players stay in pregenerated/known terrain at survival speeds.

**Still folklore:** `budgetPerCore` and the perf-mod multiplier are anchored to
ONE validated data point (see the calibration log) and otherwise extrapolated.
The exponent (sim²) is sound; the constant needs more `bench` points.

---

## 4. Chunk-system threads (C2ME) — *heuristic*

**Inputs:** effective cores.

**Decision:** keep C2ME; size workers to leave the main thread + GC headroom:
- cores **≥ 4** → workers = `floor(cores) − 2`.
- cores **< 4** → workers = `floor(cores) − 1` (min 1).

**Why:** C2ME moves chunk generation and I/O *off* the single main tick thread,
so it helps even on few cores — on the reference 3-core box it cut max tick time
from 145–288 ms to 61–67 ms (~4×). An earlier version of this rule recommended
*disabling* C2ME below 4 cores; that was wrong, and the measured ~4× is why. The
real concern isn't whether to run it but not letting its pool consume every core:
reserve ~1 core for the main thread + GC on small boxes, ~2 on bigger ones. (3
cores → 2 workers, which is what auto-sizing picks and what tested best.)

---

## 5. Lighting engine — *heuristic*

**Inputs:** none (recommendation is unconditional).

**Decision:** **ScalableLux**, regardless of core count.

**Why:** ScalableLux is the actively-maintained light-engine rewrite — a Starlight
fork. On many cores it parallelizes lighting; on few it performs like
single-threaded Starlight with no downside. An earlier version recommended bare
**Starlight** below 8 cores, but Starlight frequently has *no build for current
Minecraft versions* (e.g. none for 1.21.5), so that rule could point at a mod you
can't install. Recommending the maintained fork avoids that. The parallel-vs-
single-thread crossover by core count is still unmeasured, but it no longer
changes the recommendation — only, eventually, how hard we'd argue for it.

---

## 6. Storage class — *solid*

**Inputs:** `/sys/block/<dev>/queue/rotational` (best-effort; often invisible
inside a container).

**Decision:**
- **rotational (HDD)** → emit a warning; pull view + sim distance down and watch
  `io.pressure`.
- **SSD/NVMe** → no storage action.

**Why:** region-file reads/writes happen on (or block) the tick thread. On spinning
rust, a chunk save/load can stall the loop for tens of milliseconds; the larger
your loaded area (view/sim distance), the more I/O you generate. On low-latency
storage, chunk I/O is rarely the tick bottleneck.

---

## 7. Containerized contention note — *solid*

**Inputs:** detected virt = LXC or Docker.

**Decision:** emit a note pointing at `tickwarden watch`.

**Why:** a perfectly tuned, quiet modpack can *still* stutter if a neighbor
saturates the shared CPU or I/O pool. This is the failure mode no in-JVM profiler
can diagnose, and the reason the `watch` subcommand exists.

---

## What's deliberately not here yet

- **Network/compression tuning** (Krypton, network-compression-threshold).
- **GC flag-level detail** (the actual Aikar flag string, ZGC tuning).
- **Pregeneration guidance** (Chunky rate vs. live load).
- **Player *spread* awareness** (clustered vs. exploring) — the companion reports
  count + peak today; distinguishing spread-out from grouped players is a TODO.
- **Mod auto-detection** — `-perf-mods` is a flag; scanning the mods dir to set
  it automatically is a TODO.

These are intentionally staged behind the benchmark harness — adding rules we
can't validate just grows the folklore surface.

---

## Calibration log — how these rules stay honest

This file and `internal/tune/tune.go` are the SAME rules in two forms: the
knowledge and the code. **Every learning updates both, in the same commit.** A
rule's `Confidence` (`solid`/`heuristic`/`contested`) is its maturity, and the
intended lifecycle is: `contested` → gather a `bench`/`bench-diff` data point →
re-derive the constant → promote. Numbers in the code carry a `CALIBRATION
ANCHOR` comment pointing back here so nobody "cleans up" a magic number that's
actually load-bearing.

Validated data points so far:

| Date | Rule | Anchor measurement | Source |
|---|---|---|---|
| 2026-05-31 | distances `budgetPerCore=64` (the MAX sustainable, not a safe default) | 3 eff. cores, 2 players, perf mods → sim-10 holds 20 TPS (median tick ~7ms, max spike 61–67ms); tune now reproduces the operator's hand-validated view-20/sim-10 | spark on the reference Proxmox LXC |
| 2026-05-31 | view distance maxed for render (sim+10, cap 24 on SSD; sim+6, cap 16 otherwise) | operator runs view-20 on SSD at 1–2 players with no tick cost (view = bandwidth/RAM, not tick CPU) | reference server live config |
| 2026-05-31 | perf-mod multiplier (~2×) | same box: max tick 145–288ms *before* C2ME/ScalableLux → 61–67ms *after* (~4× on the tail; budget bumped ~2× conservatively) | spark before/after |
| 2026-05-31 | GC = G1+Aikar < 12 GiB | reference server runs Aikar G1 flags at 4 GiB heap, no TPS-visible pauses | live config |
| 2026-05-31 | C2ME kept (not disabled) below 4 cores; 3 cores → 2 workers | same box: max tick 145–288ms → 61–67ms with C2ME at 2 workers; disabling it was the wrong call | spark before/after |
| 2026-05-31 | heap sized to load, not all-RAM | 8 GiB / 2 players: real heap usage ~1.1 GiB of a 4 GiB heap → recommend 3 GiB, leaving page cache | spark heap gauge |

| 2026-06-03 | **sim-distance cost validates the `players × sim²` model** | one Carpet fake-player fixed at spawn, sim 6/10/14 → mean tick 4.74 / 6.07 / 8.80 ms (CPU pressure flat ~7%, so trustworthy). The three points track base + k·sim² (predicts sim-10 ≈ 6.4 ms vs 6.07 measured). sim-6→14 (~5.4× area) = +86% tick. All held 20 TPS — a single player has headroom well past sim-10; the sim-10 ceiling was for the heavier 2-flying-players case | `loadtest -load player` + `bench-diff` on the reference box |
| 2026-06-03 | generation is *not* a tick bottleneck on this box | a 40-chunk-radius Chunky burst on fresh terrain held 20 TPS, 0.22 ms mean tick, CPU pressure ~10% — C2ME runs gen off-thread and 3 cores absorb it (confirms the storage/C2ME work fixed the old 145–288 ms gen spikes) | `tickwarden loadtest` on the reference box |

> **Why the sim-distance ceiling still isn't freshly A/B'd:** `loadtest` drives
> chunk *generation*, which this box no longer chokes on. Simulation distance
> costs come from *ticking entities* in the loaded area, so settling sim-10 vs.
> sim-12 needs an entity/player load that Chunky can't fake.
>
> The `loadtest -load entity` mode (force-load + summon AI mobs) confirmed
> **simulation cost is main-thread bound: it shows up as MSPT, not CPU pressure**
> (500 crowded villagers ≈ 28 ms mean tick at <2% CPU pressure). But summoned mobs
> are a poor *dial* (collision-dominated AI, Y-sensitive placement), so the real
> A/B uses `loadtest -load player` — **Carpet fake-players**, which create a true
> simulation-distance bubble. That A/B is now done (see the table below): it both
> measured the sim-distance cost AND validated the `players × sim²` model the
> distance rule is built on.

**Open calibrations needing data** (run `bench-diff` to settle):
- Lighting parallel-vs-single-thread benefit by core count (rule 5) — no longer
  changes the recommendation (always ScalableLux, the maintained fork), but the
  size of the win on many cores is still unmeasured.
- `budgetPerCore` at higher player counts — only the 2-player point is real.
- GC 12 GiB G1→ZGC crossover (rule 2) — a rule of thumb, unmeasured.
- heap `0.5 GiB/player` slope — anchored to one low-population point; untested at scale.

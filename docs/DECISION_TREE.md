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

## 1. Heap size (`-Xmx` / `-Xms`) — *solid*

**Inputs:** memory budget = cgroup `memory.max` if set, else `MemTotal`.

**Decision:** `Xmx = budget − max(1.5 GiB, 25% of budget)`, rounded down to whole
GiB. `Xms = Xmx`.

**Why:** the JVM heap is not the only consumer. You must leave headroom for:
- **Netty direct buffers** (off-heap, scale with player count/network),
- **mmap'd region files** (chunk storage is memory-mapped),
- **Metaspace / thread stacks / GC structures**,
- and most importantly the **OS page cache** — the thing that keeps chunk reads
  off disk. Handing 80% of RAM to the heap is the classic mistake: it starves the
  page cache and you trade GC headroom for disk I/O stalls.

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

**Inputs:** effective cores.

**Decision:** keep **sim < view**.

| effective cores | view | sim |
|---|---|---|
| ≥ 8 | 12 | 8 |
| ≥ 4 | 10 | 6 |
| < 4 | 8  | 5 |

**Why:** these two knobs have very different costs. **Simulation distance** drives
entity ticking, redstone, mob AI, block updates, fluid flow — the real per-tick
CPU work, and it scales roughly with the *area* simulated. **View distance** mostly
costs bandwidth and chunk sends; it's comparatively cheap on CPU. People crank both
together and blame "lag"; the fix is usually to pull sim distance down and leave
view distance high so the world still *looks* big.

---

## 4. Chunk-system threads (C2ME) — *heuristic*

**Inputs:** effective cores.

**Decision:**
- cores **≥ 4** → C2ME worker threads = `max(2, floor(cores) − 2)`.
- cores **< 4** → consider *not* running C2ME.

**Why:** C2ME parallelizes chunk generation and I/O across worker threads. But the
main server **tick thread** is single-threaded and is the thing you're protecting —
if chunk workers consume every core, they starve the very loop they're feeding,
and GC needs a core too. So reserve ~2 cores for the main thread + GC. On a box
with fewer than 4 cores, parallel chunk I/O contends with the main thread more than
it helps; vanilla/single-threaded chunk handling can be the better call.

---

## 5. Lighting engine: Starlight vs. ScalableLux — *contested* ⚠️

**Inputs:** effective cores.

**Decision:**
- cores **≥ 8** → **ScalableLux**.
- cores **< 8** → **Starlight**.

**Why (and the caveat):** Starlight is a faster *single-threaded* rewrite of the
light engine. ScalableLux *parallelizes* lighting across cores. The intuition is
that with enough cores, parallel lighting beats a fast single thread, and below
that the coordination overhead isn't worth it — so there's a crossover somewhere.
**But where that crossover actually sits is folklore.** We picked 8 cores because
it's plausible, not because we measured it. This is the #1 rule to validate once
the Tier 1.5 benchmark harness exists.

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

- **Player-count-aware rules** (VMP only earns its keep at higher player counts).
- **Network/compression tuning** (Krypton, network-compression-threshold).
- **GC flag-level detail** (the actual Aikar flag string, ZGC tuning).
- **Pregeneration guidance** (Chunky rate vs. live load).

These are intentionally staged behind the benchmark harness — adding rules we
can't validate just grows the folklore surface.

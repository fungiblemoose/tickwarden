# tickwarden

**Hardware-aware, host-aware tuning and observability for self-hosted Minecraft servers.**

tickwarden is a single static Go binary you drop *inside* your server's container.
It does two things nothing else does well:

1. **Tunes to your actual hardware.** It reads your cores, RAM, storage, and —
   crucially — your *cgroup limits* (not the host's specs), then recommends heap,
   GC, view/sim distance, chunk-system threads, and lighting engine, **with the
   reason for every choice.**
2. **Tells you when your TPS dip isn't your fault.** It correlates in-game TPS
   with your container's Pressure Stall Information (PSI) and CPU throttling, so
   it can say *"your modpack is fine — a neighbor saturated the shared I/O pool"*
   — a diagnosis spark literally cannot make, because spark only sees inside the JVM.

> Status: `detect`, `tune`, `watch`, and `bench` all work. `watch`/`bench` read
> real cgroup PSI and real TPS via a tiny Fabric companion mod (in `companion/`).
> Validated live on a Proxmox LXC running Minecraft 1.21.5 + Fabric. See the
> roadmap for what's next.

## Why this exists

Two problems that no existing tool solves, and that are the entire point of this one:

- **Hardware-aware tuning.** The settings that actually matter — heap sizing
  against your *real* memory budget, C2ME threads vs. core count, view-vs-sim
  asymmetry, ScalableLux-vs-Starlight by core count — depend on the hardware
  you're running on. Today you tune them by hand from forum folklore. tickwarden
  detects the hardware (and your cgroup limits) and derives the settings, showing
  the reasoning for each.

- **Cross-layer bottleneck detection.** spark and every other profiler see *inside
  the JVM*. They have no idea your TPS froze because something below the game —
  a neighbor on the same host, your own throttled CPU quota, a saturated I/O pool —
  starved the tick thread. tickwarden correlates in-game TPS with your container's
  pressure and throttling, so it can tell you *where* the bottleneck actually lives.

Optimization *mods* (Lithium, C2ME, ScalableLux/Starlight, spark) and bundles that
ship them already exist; that part is solved. tickwarden is the missing layer above
them: the intelligence that decides what to set and the observability that explains
what went wrong.

## Install

```sh
go install github.com/fungiblemoose/tickwarden/cmd/tickwarden@latest
# or build a static binary to drop into a container:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o tickwarden ./cmd/tickwarden
```

## Usage

```sh
tickwarden detect          # what hardware/limits am I actually running under?
tickwarden detect -json    # machine-readable

tickwarden tune            # recommended settings, each with its reasoning
tickwarden tune -players 8          # size view/sim distance for 8 peak players
tickwarden tune -players-url http://127.0.0.1:9225/tps   # ...or for the MEASURED peak
tickwarden tune -json

tickwarden watch           # correlate TPS dips with host starvation (live)
tickwarden watch -tps-url http://127.0.0.1:9225/tps -interval 5s

tickwarden bench -tps-url http://127.0.0.1:9225/tps -duration 60s -label before -out before.json
# ...change a setting, restart, then re-run under the SAME load...
tickwarden bench -tps-url http://127.0.0.1:9225/tps -duration 60s -label after  -out after.json
tickwarden bench-diff before.json after.json   # did it actually help?
```

`bench-diff` won't be fooled: if host pressure differed between the two runs it
marks the comparison **INCONCLUSIVE**, because then a TPS change might just
reflect what else the box was doing — the exact trap a naive before/after misses.

## TPS source: the Fabric companion (`companion/`)

`watch` and `bench` need the server's real tick health, which lives inside the
JVM. spark can't provide it over RCON (its commands reply asynchronously and come
back empty), so `companion/` is a ~90-line Fabric mod that times every server
tick and serves rolling TPS/MSPT as JSON on a localhost-only endpoint:

```sh
cd companion && ./gradlew build         # needs JDK 21; pins fabric-api to 1.21.5
# drop build/libs/tickwarden-companion-*.jar in your server's mods/ and restart
curl http://127.0.0.1:9225/tps          # {"tps":20.00,"mspt":6.33}
```

Port is `9225` by default (`-Dtickwarden.port=` or `TICKWARDEN_PORT` to change),
bound to `127.0.0.1` only. Versions in `companion/build.gradle` are pinned to
Minecraft 1.21.5; bump them to match other server versions.

`tune` labels every recommendation by confidence:
`solid` (trust it) · `heuristic` (sane default) · `contested` (validate against
your load — the lighting-engine crossover especially).

## How the host-aware part works (no host access required)

The clever bit: you don't need an agent on the Proxmox/Docker host to know you're
being starved. cgroup v2 exposes *your own* pressure and throttling from inside
the container:

```
/sys/fs/cgroup/<self>/cpu.pressure
/sys/fs/cgroup/<self>/io.pressure
/sys/fs/cgroup/<self>/memory.pressure
/sys/fs/cgroup/<self>/cpu.stat   → nr_throttled, throttled_usec
```

When a noisy neighbor saturates a shared pool, the *symptom* — your io.pressure
spiking or your CPU quota getting throttled — is visible to you even though the
*cause* (which process, in which other container) is not. Detecting "I was
starved" is ~90% of the value and needs zero host access. Naming the culprit is
a later tier that does.

## Roadmap

| Tier | What | Status |
|------|------|--------|
| **0** | Static hardware/cgroup-aware tuner → annotated config | ✅ functional (`detect`, `tune`) |
| **0.5** | **Apply** recommendations to server.properties, with per-setting override | ✅ functional (`tune -apply`/`-write`/`-revert` + `tickwarden.toml` locks) |
| **1** | In-container observability: TPS ⨯ PSI correlation (`watch`) | ✅ functional — real PSI (chain-walked) + real TPS via the companion mod |
| **1.5** | Benchmark harness to *validate* tuning rules instead of trusting folklore | ✅ `bench` + `bench-diff` (with host-load confound detection); ⬜ still want a built-in controlled-load driver |
| **2** | Host-side agent mapping cgroup → process, to *name* the noisy neighbor and read host-applied limits | ⬜ planned (fragments across LXC/Docker/bare-metal) |
| **3** | Closed loop: auto back off view-distance / Chunky rate *while* starved | ⬜ aspirational (and genuinely risky) |

The honest hard part is **Tier 1.5**: without a repeatable load you can't know if
a tuning change helped, so the rules stay folklore. With it, tickwarden has a
real validation story and the dataset to make the rules engine learn.

### Applying changes (Tier 0.5)

`tune -apply` writes the server.properties recommendations into the real file,
with guardrails:

```sh
tickwarden tune -apply                       # dry run: shows the diff, writes nothing
tickwarden tune -apply -write                # applies (after a .bak backup)
tickwarden tune -revert                      # restores from the .bak
tickwarden tune -apply -config tickwarden.toml   # honour your locked settings
```

- **Dry-run by default** — nothing changes without `-write`.
- **Per-setting override.** A `tickwarden.toml` pins values you want kept; the
  tuner never overwrites them, only flags where it disagrees:
  ```toml
  [lock]
  view-distance = true        # I run view-20 on purpose; don't touch it
  simulation-distance = true
  ```
- **Automatic `.bak`** before any edit, undone by `tune -revert`.
- **server.properties only, for now.** JVM flags and mod configs are *reported*
  (`apply by hand: …`) rather than auto-written — editing a systemd unit or a
  mod's TOML is format-specific and riskier than it's worth. It writes config;
  you restart on your own schedule.

> Note: the rules are deliberately conservative — e.g. on a 3-core box they
> suggest `view-distance=8`, even though with C2ME + ScalableLux a hand-tuned
> view-20 holds 20 TPS. That's what the lock file is for, and a sign the rules
> should eventually factor in which performance mods are installed.

### Field-validated findings

These came out of running tickwarden on a real Proxmox LXC, and shaped the design:

- **PSI must be read up the cgroup chain, not just the leaf.** Inside a Proxmox
  LXC the workload sits in `/.lxc`, whose own pressure reads ~0; the real CPU
  pressure shows up at the namespace root one level up. `watch` now scans
  leaf→root and keeps the worst.
- **Host-applied CPU caps are invisible from inside the container.** Proxmox
  `pct cpulimit` enforces on a cgroup *above* the container's namespace ceiling,
  so the throttle counter reads 0 even when the container is throttled 90%+ of
  the time. PSI still catches the *symptom*; *naming* the cap needs the Tier 2
  host agent. This is the concrete reason that tier exists.

## License

MIT © Backspin Labs. See [LICENSE](LICENSE).

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

> Status: early scaffold. `detect` and `tune` are real; `watch` reads real cgroup
> PSI but ships with a stub TPS source until the spark companion is wired up. See
> the roadmap below.

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
tickwarden tune -json

tickwarden watch           # correlate TPS dips with host starvation (live)
tickwarden watch -tps-url http://127.0.0.1:9225/tps -interval 5s
```

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
| **0.5** | **Apply** recommendations to the real config files, with per-setting override | ⬜ planned (see below) |
| **1** | In-container observability: TPS ⨯ PSI correlation | 🚧 PSI real + chain-walking; TPS source is a stub + spark-HTTP seam |
| **1.5** | Benchmark harness (Chunky pregen / fake-player load) to *validate* tuning rules instead of trusting folklore | ⬜ planned — this is what turns the rules from blog-post to measured |
| **2** | Host-side agent mapping cgroup → process, to *name* the noisy neighbor and read host-applied limits | ⬜ planned (fragments across LXC/Docker/bare-metal) |
| **3** | Closed loop: auto back off view-distance / Chunky rate *while* starved | ⬜ aspirational (and genuinely risky) |

The honest hard part is **Tier 1.5**: without a repeatable load you can't know if
a tuning change helped, so the rules stay folklore. With it, tickwarden has a
real validation story and the dataset to make the rules engine learn.

### Applying changes (Tier 0.5)

Today `tune` only *prints* advice — you edit configs yourself, which is safe but
tedious. The plan is `tickwarden tune --apply` that writes the recommendations
into the real files (`server.properties`, the JVM flags in the systemd unit,
mod configs like `c2me.toml`), with strong guardrails:

- **Dry-run by default.** `--apply` shows a diff and asks; nothing changes
  without an explicit confirm (or `--yes`).
- **Manual override per setting.** A `tickwarden.toml` pins values you want to
  keep — e.g. `simulation-distance = 10` — and the tuner treats those as locked,
  never overwriting them, just flagging when its recommendation differs.
- **Automatic `.bak` backups** before any edit, and a `tune --revert` to undo.
- **Never live-edits a running server's behavior** — it writes config; you
  restart on your schedule. (Reacting *while* running is the separate, riskier
  Tier 3.)

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

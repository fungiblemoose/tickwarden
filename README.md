# tickwarden

Tuning and lag diagnostics for self-hosted Minecraft servers.

tickwarden tunes a server to the hardware it's actually running on. It also
looks underneath Java itself to explain lag that in-game profilers can't see:
CPU throttling, slow disk access caused by other containers on the same box, OS
and kernel settings, and overheating. A profiler like spark tells you what's
slow inside the game; tickwarden covers the layers below that.

It's a single Go binary with nothing to install alongside it, plus an optional
Fabric mod that reports live TPS and serves a small web dashboard. You can run it
in two places:

- **Inside the server's container**, to tune the server, line up TPS drops with
  the container's own resource pressure, and (optionally) auto-scale distance.
- **On the Proxmox or Docker host**, to find which container is starving the
  others — and throttle it.

What sets it apart: its tuning model isn't folklore. Every constant
(`players × sim²`, the flying-vs-survival budget, the clustered factor, the
capacity cliff) was *measured* on real hardware — see `docs/DECISION_TREE.md`.

Prebuilt binaries (Linux amd64/arm64, macOS arm64) and the companion jar are on
the [releases page](https://github.com/fungiblemoose/tickwarden/releases).

## Quickstart

```sh
# 1. tune to your hardware (reads cores, RAM, cgroup limits, installed mods)
tickwarden optimize -mods-dir /path/to/mods

# 2. drop the companion jar in the server's mods/ and restart, then:
#    - live status dashboard:  http://127.0.0.1:9225/
#    - let the server tune itself, watching only (safe):
tickwarden daemon
#    - ...or actually scale simulation/view distance with load:
tickwarden daemon -apply

# on the Proxmox/Docker host: find and throttle a noisy neighbour
tickwarden host
tickwarden iostorm -apply
```

Settings live in `tickwarden.toml` (`defaults < config < flags`); install the
daemon as a service with `scripts/install.sh`.

## Install

```sh
go install github.com/fungiblemoose/tickwarden/cmd/tickwarden@latest
```

Or grab a binary from releases, or build one to drop into a container:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o tickwarden ./cmd/tickwarden
```

## Commands

**Tuning** (run anywhere, including inside the container):

```sh
tickwarden optimize               # one-shot: detect + mods + players → the whole plan, one screen
tickwarden optimize -apply        # ...and write the server.properties part (dry-run unless -write)
tickwarden detect                 # hardware + cgroup limits the server runs under
tickwarden tune                   # recommended settings, each with a reason and confidence
tickwarden tune -mods-dir mods/   # read installed mods; flag conflicts, suggest missing ones
tickwarden tune -players 8        # size view/sim distance for the expected peak players
tickwarden tune -apply            # preview writing those settings to server.properties
tickwarden tune -apply -write     # apply (after a .bak backup); -revert undoes it
```

`tune` labels every recommendation `solid` (trust it), `heuristic` (a sane
default), or `contested` (test it against your own load). Java memory and
startup flags are reported but never written for you. Only `server.properties`
keys get applied, and a `[lock]` section in `tickwarden.toml` pins any value you
want left alone.

**In-container observability** (TPS needs the companion mod, below):

```sh
tickwarden watch -tps-url http://127.0.0.1:9225/tps   # is a TPS dip the world's fault or the host's?
tickwarden bench -tps-url ... -out before.json        # measure a window: TPS/MSPT + pressure
tickwarden bench-diff before.json after.json          # compare two runs
tickwarden hotspots                                   # loaded chunks ranked by block-entity count
tickwarden pregen                                     # Chunky pregen that yields to players/load
tickwarden loadtest                                   # drive a fixed Chunky load while benchmarking
```

`bench-diff` marks a comparison inconclusive when host pressure was different
between the two runs, because then a TPS change might be the host's fault rather
than your change's.

**Host-level** (run on the Proxmox/Docker host):

```sh
tickwarden host        # rank containers by CPU/IO load; name the noisy neighbour
tickwarden iostorm     # detect a write-storm starving the server; suggest a cgroup io.max throttle
tickwarden ostune      # CPU governor, swappiness, THP, I/O scheduler — with the exact fix
tickwarden thermal     # CPU temperature / frequency throttling
```

## The companion mod (`companion/`)

`watch`, `bench`, `hotspots`, and `pregen` need the server's real tick health,
and that number only exists inside the running game. spark can't hand it over
through RCON (its commands reply asynchronously and come back empty), so
`companion/` is a small Fabric mod that times each server tick and serves the
result as JSON on a localhost-only endpoint:

```sh
cd companion && ./gradlew build    # needs JDK 21
# drop build/libs/tickwarden-companion-*.jar into the server's mods/, then restart
curl http://127.0.0.1:9225/tps        # {"tps":20.00,"mspt":6.3,"players":0,"players_peak":1}
curl http://127.0.0.1:9225/hotspots   # [{"dimension":"...","x":-2,"z":0,"block_entities":2}]
```

It binds `127.0.0.1` only (port `9225`, override with `-Dtickwarden.port=` or
`TICKWARDEN_PORT`). Versions in `companion/build.gradle` are pinned to Minecraft
1.21.5; bump them for other versions.

## How the cross-layer part works

Inside a container you can read your own cgroup-v2 pressure and throttling
stats, so you can tell you were starved without needing any host access:

```
/sys/fs/cgroup/<self>/{cpu,io,memory}.pressure
/sys/fs/cgroup/<self>/cpu.stat        # nr_throttled, throttled_usec
```

Detecting *that* you were starved covers most of the value and needs no
privileges. Naming the *cause*, meaning which other container saturated the
disk, does need a view of the host. That's what `tickwarden host` provides.

Two things I learned running this on a real Proxmox LXC, both now handled in
code:

- **Read PSI up the cgroup chain, not just the leaf.** The workload sits in a
  `/.lxc` leaf whose own pressure reads close to zero. The real pressure shows up
  one level up, at the namespace root. So `watch` scans from leaf to root and
  keeps the worst reading.
- **Host-applied CPU caps are invisible from inside.** A Proxmox `cpulimit` is
  enforced on a cgroup above the container's namespace ceiling, so the
  in-container throttle counter reads 0 even at 90%+ throttling. PSI still shows
  the symptom, and `tickwarden host` reads the real number from the host side.

## What it's tuning for

The goal is to get the most render and simulation distance your system can hold
at a steady 20 TPS, instead of playing it safe with conservative defaults. The
distance rules are calibrated to a ceiling that's actually been measured. View
distance gets pushed hard for render range, since that costs bandwidth and RAM
rather than tick CPU, and it's cheap once chunks are pregenerated on an SSD. The
catch is that when you share a host, tickwarden sizes things to your cgroup's
real budget and uses the `host` and `iostorm` views so that maxing out your
server doesn't starve everything else on the box.

The rules are upfront about how solid they are. Some are well established, some
are educated guesses, and the contested ones (like the per-core distance budget)
say so outright. Each carries its confidence level and the reasoning behind it, and
`docs/DECISION_TREE.md` records which numbers come from a real measurement and
which are extrapolated. `bench` and `bench-diff` exist so a contested rule can
be settled with data instead of an argument.

## Roadmap

**Status: v1.0 — the roadmap is complete.** Every tier below is built and, where
it counts, validated on real hardware. v1.0 also adds the things that make it a
product rather than a toolkit: a web **dashboard**, a **daemon** that runs the
adaptive controller as a service, a **config file**, and `iostorm -apply` to
auto-throttle a noisy neighbour. See [CHANGELOG.md](CHANGELOG.md).

| Tier | What | Status |
|------|------|--------|
| 0 | Hardware/cgroup-aware tuner with reasons (`detect`, `tune`) | done |
| 0.5 | Apply to `server.properties` with locks + revert | done |
| 1 | TPS × cgroup-PSI correlation (`watch`) | done |
| 1.5 | Benchmark + diff with host-load confound detection (`bench`, `bench-diff`) | done; a built-in load driver beyond `loadtest` is still open |
| 2 | Host agent: name the noisy neighbour, read host-applied limits (`host`, `iostorm`) | done |
| below-Java | OS/kernel (`ostune`), thermal, mod intelligence, lag-by-location (`hotspots`), host-aware pregen | done |
| 3 | Adaptive tuning: scale sim/view distance live with load (`adaptive`) | done — companion changes distance at runtime (no restart); controller sheds fast / raises slow, holds a floor, and pre-sizes for the expected peak so a join never triggers a cut. Opt-in: dry-run by default, `-apply` to act |

### Adaptive (`tickwarden adaptive`)

Scales simulation/view distance to the live load instead of fixing one value for
the worst case. It reads the companion's TPS/MSPT, decides with the measured
`players × sim²` model, and (with `-apply`) changes distance at runtime — no
restart, no player kick. Safety: **view (render) distance is never lowered** —
view costs bandwidth and RAM, not tick CPU, so cutting it recovers nothing; only
sim sheds load. Plus hard floors, fast-down/slow-up ramps, a deadband, a **50ms
panic valve** (when ticks are actually being dropped, sim snaps straight to the
floor instead of stepping), and **on-join pre-sizing** — it sizes for your
expected peak (`players_peak`), so a join finds a peak-safe distance already set
rather than forcing a drop.

It's also **host-aware**, which no in-game distance scaler is: each decision
reads the container's cgroup PSI and CPU-throttling (the same signals as
`watch`), and when the *host* — a noisy neighbour, a quota — explains a bad
MSPT, it holds instead of cutting. An MSPT reading taken under starvation
measures stolen CPU, not world cost; shrinking the world wouldn't give the
ticks back, so it points you at `tickwarden host`/`iostorm` instead. Threshold:
`-starve-psi` / `starve_psi` (PSI some-avg10 %, default 10; 0 disables).

Dry-run by default:

```sh
tickwarden adaptive                 # log decisions only (safe to watch)
tickwarden adaptive -apply          # actually scale distance live
```

Needs the companion mod (≥0.4.0) for the `/setdistance` runtime hook.

## License

MIT © Backspin Labs. See [LICENSE](LICENSE).

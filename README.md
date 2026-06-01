# tickwarden

Tuning and cross-layer diagnostics for self-hosted Minecraft servers.

tickwarden tunes a server to the hardware it actually runs on, and looks *below*
the JVM to explain tick problems that in-game profilers can't see: CPU
throttling, I/O contention from neighbouring containers, OS/kernel settings, and
thermal throttling. spark and friends profile the JVM; tickwarden covers the
layers underneath it.

It's a single static Go binary, plus a small optional Fabric mod that reports
live TPS. It runs in two places:

- **inside the server's container** — for tuning, and for correlating TPS with
  the container's own resource pressure.
- **on the Proxmox/Docker host** — to identify which container is starving the
  others.

Prebuilt binaries (Linux amd64/arm64, macOS arm64) and the companion jar are on
the [releases page](https://github.com/fungiblemoose/tickwarden/releases).

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
tickwarden detect                 # hardware + cgroup limits the server runs under
tickwarden tune                   # recommended settings, each with a reason and confidence
tickwarden tune -mods-dir mods/   # read installed mods; flag conflicts, suggest missing ones
tickwarden tune -players 8        # size view/sim distance for the expected peak players
tickwarden tune -apply            # preview writing those settings to server.properties
tickwarden tune -apply -write     # apply (after a .bak backup); -revert undoes it
```

`tune` labels every recommendation `solid` (trust it), `heuristic` (a sane
default), or `contested` (validate against your own load). Heap/GC and JVM flags
are reported, not auto-written; only `server.properties` keys are applied, and a
`tickwarden.toml` `[lock]` section pins any value you want left alone.

**In-container observability** (TPS needs the companion mod, below):

```sh
tickwarden watch -tps-url http://127.0.0.1:9225/tps   # is a TPS dip the world's fault or the host's?
tickwarden bench -tps-url ... -out before.json        # measure a window: TPS/MSPT + pressure
tickwarden bench-diff before.json after.json          # compare two runs
tickwarden hotspots                                   # loaded chunks ranked by block-entity count
tickwarden pregen                                     # Chunky pregen that yields to players/load
tickwarden loadtest                                   # drive a fixed Chunky load while benchmarking
```

`bench-diff` marks a comparison inconclusive when host pressure differed between
the two runs, since then a TPS change may reflect the host rather than your
change.

**Host-level** (run on the Proxmox/Docker host):

```sh
tickwarden host        # rank containers by CPU/IO load; name the noisy neighbour
tickwarden iostorm     # detect a write-storm starving the server; suggest a cgroup io.max throttle
tickwarden ostune      # CPU governor, swappiness, THP, I/O scheduler — with the exact fix
tickwarden thermal     # CPU temperature / frequency throttling
```

## The companion mod (`companion/`)

`watch`, `bench`, `hotspots`, and `pregen` need the server's real tick health,
which only exists inside the JVM. spark can't supply it over RCON — its commands
reply asynchronously and come back empty — so `companion/` is a small Fabric mod
that times each server tick and serves JSON on a localhost-only endpoint:

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

Inside a container you can read your own cgroup-v2 pressure and throttling, so
you can tell that you were starved without any host access:

```
/sys/fs/cgroup/<self>/{cpu,io,memory}.pressure
/sys/fs/cgroup/<self>/cpu.stat        # nr_throttled, throttled_usec
```

Detecting *that* you were starved covers most of the value and needs no
privileges. Naming the *cause* — which other container saturated the disk — does
need a view of the host, which is what `tickwarden host` provides.

Two things learned running this on a real Proxmox LXC, both now handled in code:

- **Read PSI up the cgroup chain, not just the leaf.** The workload sits in a
  `/.lxc` leaf whose own pressure reads ~0; the real pressure shows up at the
  namespace root one level up. `watch` scans leaf→root and keeps the worst.
- **Host-applied CPU caps are invisible from inside.** A Proxmox `cpulimit` is
  enforced on a cgroup above the container's namespace ceiling, so the in-container
  throttle counter reads 0 even at 90%+ throttling. PSI still shows the symptom;
  `tickwarden host` reads the real number from the host side.

## Philosophy: max performance, node-aware

The goal is the most render and simulation distance the system can sustain at 20
TPS, not a safe-but-timid default. Distance rules are calibrated to a measured
ceiling; view distance is pushed for render range (it costs bandwidth and RAM,
not tick CPU, and is cheap over pregenerated chunks on SSD). The constraint is
that when you share a host, tickwarden sizes to your cgroup's real budget and
uses the `host`/`iostorm` view to keep maxing your server from starving the rest
of the node.

The rules are honest about their footing. Some are well-established; some are
heuristics; the lighting-engine choice is openly `contested`. Each carries its
confidence and the reasoning behind it, and `docs/DECISION_TREE.md` records which
numbers are backed by a measurement versus extrapolated. `bench`/`bench-diff`
exist so a contested rule can be settled with data rather than argument.

## Roadmap

| Tier | What | Status |
|------|------|--------|
| 0 | Hardware/cgroup-aware tuner with reasons (`detect`, `tune`) | done |
| 0.5 | Apply to `server.properties` with locks + revert | done |
| 1 | TPS × cgroup-PSI correlation (`watch`) | done |
| 1.5 | Benchmark + diff with host-load confound detection (`bench`, `bench-diff`) | done; a built-in load driver beyond `loadtest` is still open |
| 2 | Host agent: name the noisy neighbour, read host-applied limits (`host`, `iostorm`) | done |
| below-JVM | OS/kernel (`ostune`), thermal, mod intelligence, lag-by-location (`hotspots`), host-aware pregen | done |
| 3 | Adaptive tuning: scale distances live with players/headroom | not started (risky; needs headroom to give back) |

## License

MIT © Backspin Labs. See [LICENSE](LICENSE).

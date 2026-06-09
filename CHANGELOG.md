# Changelog

All notable changes to tickwarden. Dates are when the work landed.

## Unreleased

- **`tickwarden init` — scaffold a server, don't just tune one.** Provisions a
  complete Fabric server tuned to the hardware from first boot: the server
  launcher jar (Fabric meta API), the recommended perf stack at the right
  versions from Modrinth (lithium, ferrite-core, krypton, c2me, scalablelux,
  spark; a mod without a build for the target MC version is skipped with a
  note), a tuned `server.properties`, a `start.sh` with sized heap and
  G1-Aikar/ZGC flags (the one place tickwarden writes JVM flags — it owns the
  file because it created it), `tickwarden.toml` with the workload assumptions,
  and a systemd unit. Scaffolds, doesn't supervise. Never accepts the EULA for
  you (`-accept-eula` is your explicit act) and refuses to overwrite an
  existing server's files.
- **Daemon web control plane (`-ui`).** A self-contained dashboard served by
  the daemon (not the companion — the game JVM stays minimal): live TPS/MSPT,
  distances, both cause gates with reasons, heap occupancy, the decision
  history (previously journald-only), and live knob editing including the
  dry-run/apply toggle — effective on the next decision, no restart. Off by
  default; non-loopback binds are refused without `-ui-token`. Config keys:
  `ui_addr`, `ui_token`.
- **Companion 0.6.0: `/jvm` endpoint + GC-stall gate.** The companion now
  exposes heap occupancy and cumulative GC counters (MXBeans, zero per-tick
  cost). The daemon diffs them between polls and **holds** when the collector
  ate the window — a 300ms G1 pause is a 6-tick freeze that previously read
  exactly like world load and triggered a pointless distance cut. The TPS-dip
  triage is now complete: world load vs host starvation vs GC, each with its
  own fix named in the decision reason. Knob: `-gc-stall-pct` / `gc_stall_pct`
  (default 15% of wall clock, 0 disables).
- **Adaptive: host-starvation gate.** The controller now reads the container's
  cgroup PSI and CPU-throttle deltas on every decision (the same signals
  `watch` correlates) and **holds** when the host — not the world — explains a
  bad MSPT. A reading taken under starvation measures stolen CPU, not world
  cost: cutting distance wouldn't recover the ticks, and the slow-raise ramp
  would take minutes to undo the pointless cut afterwards. The gate also
  suppresses the panic valve and raises (starved readings are unreliable in
  both directions) and names the real fix (`tickwarden host` / `iostorm`) in
  its decision reason. New knob: `-starve-psi` flag on `adaptive`/`daemon` and
  `starve_psi` in `tickwarden.toml` (PSI some-avg10 %, default 10, 0 disables).
  This is the cross-layer awareness no in-game-only distance scaler has.
- **Adaptive: view distance is never lowered.** View costs bandwidth and RAM,
  not tick CPU, so cutting it recovered zero MSPT — it was a player-visible
  render cut for nothing. The controller now only sheds load via sim distance;
  view holds where it is and only ratchets up to keep its lead when sim raises
  past it. The "players join, my render distance craters" fear is gone by
  construction: render distance never moves down.
- **Adaptive: 50ms panic valve.** At/over `PanicMSPT` (default 50ms — the
  entire tick budget, i.e. ticks are being dropped) sim snaps straight to the
  floor instead of stepping down, then the slow-raise ramp earns it back.
  Closes the safety-valve design question from `docs/ADAPTIVE.md`.
- **Mod intelligence: ServerCore controller-conflict note.** ServerCore scales
  simulation distance dynamically itself; `tune -mods-dir` now warns not to
  also run `tickwarden adaptive -apply` / `daemon -apply`, so two controllers
  never fight over the same setting.

## v1.0.0 — 2026-06-04

The tool is feature-complete: it tunes, observes, diagnoses below the JVM, and
adapts — with a tuning model measured on real hardware rather than guessed.

- **Web dashboard** — the companion (0.5.0) serves a self-contained status page
  at `http://127.0.0.1:9225/`: live TPS, MSPT-vs-budget bar, players/peak, current
  view & simulation distance, and block-entity hotspots.
- **`tickwarden daemon`** — runs the adaptive controller continuously as a
  service; systemd unit + `scripts/install.sh` included.
- **Config file** — `tickwarden.toml` with `defaults < config < flags`.
- **`iostorm -apply`** — auto-apply (and undo) a cgroup `io.max` write throttle
  to a noisy neighbour, with a speed floor and captured prior value.
- **Hardening** — `detect`/`observe` parsers refactored for fixture testing
  (coverage 29→66% / 11→57%); ~150 tests total.

## v0.7.0 — adaptive tuning (Tier 3)

- Live sim/view distance scaling: companion runtime setter (`/setdistance`, no
  restart, no player kick) + a controller with hard render floors, fast-down /
  slow-up ramps, a deadband, and **on-join pre-sizing** (sizes for the expected
  peak so a join never triggers a cut). Opt-in; dry-run by default.

## v0.6.0 — the measured model

- Play-style factor (`-settled`), fly/player/entity load generators, and the
  measurements that validate the model: player count is linear (~8ms/player at
  sim-12), clustered players share one bubble (factor corrected N/2 → N/3),
  flying costs ~2.5× settled, capacity cliff at ~6 spread players. The
  simulation-distance A/B confirmed `players × sim²`.

## v0.4.0 / v0.5.0 — companion persistence & TPS source

- Companion persists `players_peak` across restarts; reports live view/sim.

## v0.2.0 / v0.3.0 — below the JVM

- `host` (name the noisy neighbour), `iostorm`, `ostune` (CPU governor,
  swappiness, THP, I/O scheduler), `thermal`, `hotspots`, host-aware `pregen`,
  mod intelligence, multi-version companion builds.

## v0.1.0 — foundation

- `detect`, `tune` (hardware/cgroup-aware, reasoned recommendations), `tune
  -apply` with locks + revert, `watch` (TPS × cgroup-PSI correlation), `bench` /
  `bench-diff` with host-load confound detection, the Fabric TPS companion.

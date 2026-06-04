# Changelog

All notable changes to tickwarden. Dates are when the work landed.

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

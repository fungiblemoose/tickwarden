# Tier 3: adaptive tuning

Scale simulation/view distance with the *actual* live load instead of picking
one static value for the worst case. Branch: `adaptive`.

**Status: working end-to-end, validated live** (companion 0.4.0). The controller
(`internal/adaptive`), the runtime setter (companion `/setdistance`), and the
`tickwarden adaptive` loop are all built. Apply-mode run against an 8-bot ramp:
it shed sim 12→9→6 as MSPT spiked to 64ms, **held at the sim-6 floor without
cratering** even while briefly over target, kept TPS at 20, then raised back
6→7→8 (debounced) once the bots left. On-join pre-sizing is now in too: the
controller projects current per-player cost to the expected peak (`players_peak`)
and sizes for *that*, so a join doesn't trigger a cut and quiet times hold at the
peak-safe value instead of over-raising for a lone player.

## Why it's safe to attempt now

The reasonable fear with adaptive tuning is "players join, my render distance
craters." Two things defuse it:

1. **We measured the model.** Tick cost follows `players × sim²`, linearly in
   players, with a known capacity cliff (see `docs/DECISION_TREE.md`). The
   controller reasons from that, it doesn't flail.
2. **Hard floors + asymmetric ramps + a deadband.** `internal/adaptive`:
   - never drops sim/view below configured **floors** (the render-drop guard);
   - **drops fast** when over budget (protect TPS), **raises one step at a time**
     and only when comfortably under (no yo-yo);
   - ignores fluctuations inside a **deadband** around the target.

The controller self-calibrates: because MSPT ∝ sim², the distance that lands MSPT
on target is `CurrentSim·√(target/MSPT)`. It reads the live cost and moves toward
that — no hardcoded per-world constant.

## Architecture

```
companion (/tps: players, mspt, current sim/view)
        │  poll
        ▼
internal/adaptive.Decide(state, cfg)  ──►  Decision{sim, view, action, reason}
        │  apply (only on change)
        ▼
companion runtime setter  ──►  PlayerManager.setSimulationDistance / setViewDistance
```

- **`internal/adaptive` — DONE.** Pure `Decide(State, Config) Decision` + tests.
  Floors, asymmetric ramp, deadband, sim²-based self-calibration.
- **Companion runtime setter — TODO.** The crux. Vanilla has no command to change
  simulation/view distance at runtime, but the engine supports it:
  `MinecraftServer.getPlayerList().setSimulationDistance(n)` and
  `setViewDistance(n)` apply live (they re-issue chunk tickets to every player).
  Add a `POST /distance {sim,view}` endpoint (or an RCON-able command) to the
  companion that calls those. It also needs to *report* the current sim/view on
  `/tps` so `Decide` knows where it's starting.
- **`tickwarden adaptive` command — TODO.** Poll the companion every N seconds,
  feed `Decide`, and `POST /distance` when the action isn't `hold`. Add a debounce
  (require the same direction for K consecutive polls before raising) on top of
  the controller's deadband. Dry-run mode that only logs decisions.

## Open questions / decisions before building the rest

- **Apply cadence & debounce.** Raising too eagerly after a quiet minute, then a
  raid spawns, would force a drop. Bias toward stability: long up-debounce, short
  down. Maybe only raise when players are clustered (cheap) per the measured
  cluster factor.
- **On-join behavior.** To avoid the visible drop the user worried about, consider
  pre-emptively sizing for the *expected peak* (the persisted `players_peak`) so a
  join rarely triggers a reduction — adaptive then mostly *raises* during quiet
  times rather than cutting on join.
- **Per-player vs global.** `setViewDistance` is server-global. Paper-style
  per-player view distance isn't available on Fabric without more machinery;
  global is the scope for v1.
- **Interaction with `tune -apply`.** Adaptive owns the live value; the static
  `tune` recommendation becomes the *floor/baseline* it adapts above.
- **Safety valve — BUILT.** At/over `PanicMSPT` (default 50ms, the whole tick
  budget) the controller snaps sim straight to MinSim instead of stepping.
  No K-poll counter needed: MSPT is already a ~5s rolling mean of 100 ticks,
  so a single reading at/over 50ms is sustained overload, not noise.
- **View is decoupled from sim — BUILT.** View distance costs bandwidth and
  RAM, not tick CPU, so lowering it recovers zero MSPT. The controller never
  lowers view (an operator-set value above MaxView is left alone too); it only
  ratchets view up to keep its `ViewBuffer` lead when sim raises past it. This
  removes the render-drop fear entirely: render distance simply never moves
  down.

## Validation plan

Reuse the load harness: run `loadtest -load player` to ramp bots up and down with
`tickwarden adaptive` running in dry-run, and confirm the decisions track the
measured curve (raise toward the cliff, hold in the band, drop before TPS falls).
Then enable apply and confirm it holds 20 TPS across a player ramp.

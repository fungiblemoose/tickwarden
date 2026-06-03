package loadgen

import (
	"fmt"
	"math"
	"time"
)

// loadTag marks every entity this package summons so cleanup can kill exactly
// those and never a player's real mobs or a village.
const loadTag = "tickwarden_load"

// EntityLoad drives a controlled SIMULATION load — the entity-ticking cost that
// simulation distance governs, which a Chunky generation burst never touches.
//
// With no players online, entities only tick in loaded *and ticking* chunks, so
// this first force-loads a region around the point (force-loaded chunks tick
// regardless of players), then summons `count` AI mobs there. AI mobs
// (villagers, zombies) pay real per-tick pathfinding/goal cost; passive entities
// like armor stands tick nearly free, so pick something with a brain to actually
// load the tick thread. After holding for d it kills only its own tagged
// entities and releases the force-load, leaving the world as it found it.
//
// Coordinates are blocks; pick a valid surface spot (e.g. near world spawn) so
// the mobs don't spawn in a wall or fall into a cave. entityType is a namespaced
// id; empty defaults to minecraft:villager (heavy, persistent AI).
//
// EXPERIMENTAL — summoned mobs are not a clean, count-proportional load dial.
// Field testing found: (1) AI cost is collision-dominated, so crowded mobs are
// expensive but self-limiting while spread ones are nearly free; (2) placement
// is Y-sensitive — a wrong Y on unexplored terrain kills them on spawn (no
// load); (3) it must run with spawn suppression (handled here) or natural mobs
// accumulate and linger. It's useful for "is the tick thread the bottleneck?"
// (it is, for simulation — cost shows as MSPT, not CPU pressure), but for a true
// simulation-distance A/B you want a real player-sized sim bubble, i.e. Carpet
// fake-players, not summoned mobs.
func EntityLoad(c Commander, entityType string, x, y, z, count int, d time.Duration) error {
	if entityType == "" {
		entityType = "minecraft:villager"
	}
	if count < 1 {
		count = 1
	}
	// Suppress NATURAL spawns for the duration. Without this, force-loading a
	// region lets hostile/passive mobs spawn and accumulate during the test, and
	// they keep ticking afterward (especially near spawn chunks) — in testing
	// that left a server idling at 22 ms until a restart. With it off, OUR mobs
	// are the only variable, and the region is clean. Restored in clearEntityLoad.
	if _, err := c.Command("gamerule doMobSpawning false"); err != nil {
		return fmt.Errorf("gamerule doMobSpawning false: %w", err)
	}
	// ~48 blocks ≈ 3 chunks each way around the point.
	x1, z1, x2, z2 := x-48, z-48, x+48, z+48
	if _, err := c.Command(fmt.Sprintf("forceload add %d %d %d %d", x1, z1, x2, z2)); err != nil {
		_, _ = c.Command("gamerule doMobSpawning true")
		return fmt.Errorf("forceload add: %w", err)
	}
	// Spread the mobs across the region on a grid rather than stacking them at a
	// point. Stacking is a bad load model: crowded mobs suffocate, push into
	// walls, and their AI throttles, so the per-tick cost is non-monotonic (in
	// testing, 900 stacked villagers ticked *cheaper* than 500). Spreading lets
	// each path and tick independently, giving a load that scales with count.
	side := int(math.Ceil(math.Sqrt(float64(count)))) // grid is side×side
	spacing := 80 / side                              // span ~80 blocks, inside the ±48 forceload
	if spacing < 1 {
		spacing = 1
	}
	for i := 0; i < count; i++ {
		dx := ((i % side) - side/2) * spacing
		dz := ((i / side) - side/2) * spacing
		cmd := fmt.Sprintf(`summon %s %d %d %d {Tags:["%s"]}`, entityType, x+dx, y, z+dz, loadTag)
		if _, err := c.Command(cmd); err != nil {
			_ = clearEntityLoad(c, x1, z1, x2, z2) // best-effort cleanup
			return fmt.Errorf("summon %d/%d: %w", i+1, count, err)
		}
	}
	if d > 0 {
		time.Sleep(d)
	}
	return clearEntityLoad(c, x1, z1, x2, z2)
}

// clearEntityLoad kills the tagged load entities, releases the force-load, and
// re-enables natural spawning — so a finished or aborted run leaves no debris,
// no pinned chunks, and the doMobSpawning gamerule back on. Each step runs even
// if an earlier one errors, so one failure can't strand the others.
func clearEntityLoad(c Commander, x1, z1, x2, z2 int) error {
	_, killErr := c.Command(fmt.Sprintf("kill @e[tag=%s]", loadTag))
	_, flErr := c.Command(fmt.Sprintf("forceload remove %d %d %d %d", x1, z1, x2, z2))
	_, grErr := c.Command("gamerule doMobSpawning true")
	for _, err := range []error{killErr, flErr, grErr} {
		if err != nil {
			return fmt.Errorf("entity-load cleanup: %w", err)
		}
	}
	return nil
}

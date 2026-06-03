package loadgen

import (
	"fmt"
	"time"
)

// botPrefix names every fake player this package spawns, so cleanup can despawn
// exactly ours.
const botPrefix = "twbot"

// PlayerLoad spawns Carpet fake players to drive a real SIMULATION load. This is
// the right tool for a simulation-distance A/B where summoned mobs are not: a
// fake player creates a genuine simulation bubble of simulation-distance radius
// around itself (entities and chunks within it tick exactly as they would for a
// real player), so changing simulation-distance changes the measured tick cost.
// Summoned mobs only tick in force-loaded chunks regardless of sim distance, so
// they can't isolate the setting.
//
// Requires the Carpet mod (the `/player` command) and that RCON runs with
// permission to use it. Spawns `count` bots at the point, holds for d, then
// despawns them all. Coordinates are blocks; pick a valid surface spot.
func PlayerLoad(c Commander, x, y, z, count int, d time.Duration) error {
	if count < 1 {
		count = 1
	}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%s%d", botPrefix, i)
		if _, err := c.Command(fmt.Sprintf("player %s spawn at %d %d %d", name, x, y, z)); err != nil {
			_ = despawnBots(c, count) // best-effort cleanup of any already up
			return fmt.Errorf("spawn %s (is Carpet installed?): %w", name, err)
		}
	}
	if d > 0 {
		time.Sleep(d)
	}
	return despawnBots(c, count)
}

// despawnBots removes every fake player this package may have spawned. It tries
// all of them even if one errors, so a partial spawn still cleans up fully.
func despawnBots(c Commander, count int) error {
	var firstErr error
	for i := 0; i < count; i++ {
		if _, err := c.Command(fmt.Sprintf("player %s%d kill", botPrefix, i)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("despawn %s%d: %w", botPrefix, i, err)
		}
	}
	return firstErr
}

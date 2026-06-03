package loadgen

import (
	"fmt"
	"time"
)

// FlyLoad reproduces the load of fast flight — the worst case the default
// distance budget is calibrated to, and the one a stationary or walking bot
// can't generate (walking is slow; pregenerated chunks don't regenerate).
//
// It spawns a Carpet fake player and TELEPORTS it in steps along a straight line
// through UNGENERATED terrain, forcing the server to generate fresh chunks at
// every step — the same chunk-generation pressure that makes flying into new
// terrain expensive. Pair it against a stationary bot at the same simulation
// distance to measure the generation cost that survival play avoids (the basis
// of the `-settled` budget).
//
// Caveats: start far beyond any pregenerated area so every step hits virgin
// chunks; the teleports are bursty (each loads a view-distance disc at once)
// rather than the smooth front a real elytra produces, so read it as an upper
// bound on flight gen load. Requires Carpet. It generates region files far from
// spawn (a few MB for a short run) — harmless but they persist.
func FlyLoad(c Commander, startX, y, startZ, stepBlocks, steps int, interval time.Duration) error {
	if steps < 1 {
		steps = 1
	}
	bot := botPrefix + "0"
	if _, err := c.Command(fmt.Sprintf("player %s spawn at %d %d %d", bot, startX, y, startZ)); err != nil {
		return fmt.Errorf("spawn %s (is Carpet installed?): %w", bot, err)
	}
	for i := 0; i < steps; i++ {
		x := startX + i*stepBlocks
		if _, err := c.Command(fmt.Sprintf("tp %s %d %d %d", bot, x, y, startZ)); err != nil {
			_ = despawnBots(c, 1) // best-effort
			return fmt.Errorf("tp step %d: %w", i, err)
		}
		if interval > 0 && i < steps-1 {
			time.Sleep(interval)
		}
	}
	return despawnBots(c, 1)
}

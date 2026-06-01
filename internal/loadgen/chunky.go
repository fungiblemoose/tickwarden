package loadgen

import (
	"fmt"
	"time"
)

// Commander is the subset of *Client that the load driver needs. Depending on
// this interface — rather than the concrete *Client — lets us drive ChunkyBurst
// from tests with an in-memory fake, so the load logic is verified without a
// TCP socket or a live Minecraft server. *Client satisfies it.
type Commander interface {
	Command(string) (string, error)
}

// defaultWorld is Chunky's pregen target when the caller passes an empty world.
// The overworld is the universally present dimension, so it is the safe
// default for "just make the server work hard".
const defaultWorld = "minecraft:overworld"

// blocksPerChunk converts the caller's chunk radius into the BLOCK radius that
// the Chunky `radius` command expects. Callers think in chunks (that is the
// unit of generation work); Chunky's command takes blocks. A chunk is 16x16
// blocks.
const blocksPerChunk = 16

// ChunkyBurst runs one reproducible pregeneration burst via the Chunky mod's
// RCON console commands, in this exact order:
//
//	chunky world  <world>
//	chunky center <centerX> <centerZ>
//	chunky radius <radiusChunks*16>   (Chunky takes BLOCKS, not chunks)
//	chunky start
//	(sleep d, unless d == 0)
//	chunky cancel
//
// Ordering matters: world/center/radius are configuration that must be set
// before `start`, and we always `cancel` so the server is left idle and a
// subsequent burst starts from a clean slate.
//
// To produce the HEAVIEST, most reproducible load, point centerX/centerZ at a
// FAR, UNEXPLORED region. Already-generated chunks are merely re-loaded from
// disk (cheap and non-deterministic — it depends what was generated before),
// whereas virgin terrain forces full world GENERATION: noise sampling,
// structure placement, lighting, and saving. That generation cost is what we
// want held constant across before/after bench runs.
//
// ChunkyBurst returns on the first command error (after attempting to cancel
// is NOT done on the config/start failures, to surface the original error;
// callers running benches should treat any error as a void run).
func ChunkyBurst(c Commander, world string, centerX, centerZ, radiusChunks int, d time.Duration) error {
	if world == "" {
		world = defaultWorld
	}
	cmds := []string{
		fmt.Sprintf("chunky world %s", world),
		fmt.Sprintf("chunky center %d %d", centerX, centerZ),
		fmt.Sprintf("chunky radius %d", radiusChunks*blocksPerChunk),
		"chunky start",
	}
	for _, cmd := range cmds {
		if _, err := c.Command(cmd); err != nil {
			return fmt.Errorf("loadgen: %q failed: %w", cmd, err)
		}
	}

	if d > 0 {
		time.Sleep(d)
	}

	if _, err := c.Command("chunky cancel"); err != nil {
		return fmt.Errorf("loadgen: %q failed: %w", "chunky cancel", err)
	}
	return nil
}

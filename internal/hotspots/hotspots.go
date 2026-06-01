// Package hotspots reads the tickwarden companion mod's /hotspots endpoint and
// turns it into a ranked, human-readable view of WHERE a server's tick cost
// lives. Profilers like spark tell you which methods are hot; they don't point
// at "chunk (412,-88) holds 3,900 block entities." This package closes that gap
// by surfacing the loaded chunks carrying the most block entities (hoppers,
// chests, and other classic tick-cost culprits) so lag can be tied to a PLACE.
//
// Stdlib only, matching the rest of the CLI.
package hotspots

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Hotspot is a single loaded chunk and the tickable load it carries. Coordinates
// are CHUNK coordinates (block coords divided by 16), matching what the companion
// reports and what /tp ~ and debug screens show as the "Chunk" line.
type Hotspot struct {
	Dimension     string
	X, Z          int
	BlockEntities int
	Entities      int
}

// Fetch GETs the companion's /hotspots endpoint and returns the chunks it
// reports, ranked highest block-entity count first. The endpoint returns a JSON
// array of objects shaped like:
//
//	[{"dimension":"minecraft:overworld","x":412,"z":-88,
//	  "block_entities":3900,"entities":12}, ...]
//
// Entities may be absent or zero; only block_entities is the load-bearing metric.
func Fetch(url string) ([]Hotspot, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hotspots endpoint returned %d", resp.StatusCode)
	}

	var raw []struct {
		Dimension     string `json:"dimension"`
		X             int    `json:"x"`
		Z             int    `json:"z"`
		BlockEntities int    `json:"block_entities"`
		Entities      int    `json:"entities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding hotspots payload: %w", err)
	}

	hs := make([]Hotspot, 0, len(raw))
	for _, r := range raw {
		hs = append(hs, Hotspot{
			Dimension:     r.Dimension,
			X:             r.X,
			Z:             r.Z,
			BlockEntities: r.BlockEntities,
			Entities:      r.Entities,
		})
	}

	// Rank by block entities desc; the companion may already sort, but don't
	// trust the wire — ties broken by entities then coords for stable output.
	sort.SliceStable(hs, func(i, j int) bool {
		if hs[i].BlockEntities != hs[j].BlockEntities {
			return hs[i].BlockEntities > hs[j].BlockEntities
		}
		if hs[i].Entities != hs[j].Entities {
			return hs[i].Entities > hs[j].Entities
		}
		if hs[i].X != hs[j].X {
			return hs[i].X < hs[j].X
		}
		return hs[i].Z < hs[j].Z
	})
	return hs, nil
}

// Report renders the ranked hotspots for a human reader. The leading line and
// trailing hint explain what a high count actually MEANS (hopper/farm lag),
// because the raw number is meaningless to anyone who hasn't profiled chunks.
func Report(hs []Hotspot) string {
	var b strings.Builder
	if len(hs) == 0 {
		b.WriteString("No loaded chunks reported by the companion.\n")
		b.WriteString("(Either the world is empty or the companion mod predates the /hotspots endpoint — needs companion >= 0.2.0.)\n")
		return b.String()
	}

	b.WriteString("Loaded chunks by block-entity load (likely tick-cost hotspots):\n")
	for i, h := range hs {
		fmt.Fprintf(&b, "%2d. chunk (%d,%d) in %s: %d block entities",
			i+1, h.X, h.Z, h.Dimension, h.BlockEntities)
		if h.Entities > 0 {
			fmt.Fprintf(&b, ", %d entities", h.Entities)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nHint: high block-entity counts in one chunk usually mean a hopper/item-sorter\n")
	b.WriteString("farm or a dense storage hall. Those tick every game tick and are a common\n")
	b.WriteString("source of MSPT spikes — start optimization at the top of this list.\n")
	return b.String()
}

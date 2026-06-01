package detect

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file adds lightweight mod auto-detection. The tuner's per-core budget
// (and thus the safe simulation distance) hinges on whether chunk-system perf
// mods are present — they move chunk generation and lighting off the main tick
// thread. Rather than make the operator assert that by hand, we read the
// server's mods/ directory and recognize known jars by filename. This is a
// deliberately cheap heuristic: we do NOT crack open the jar manifests (that
// would mean parsing zip/fabric.mod.json and pull us past "filename substring"
// for no real gain), so detection is a case-insensitive substring match on the
// jar name. Mods conventionally ship as "<id>-<version>.jar", so the id is
// almost always a literal substring of the filename.

// knownMods maps a canonical mod id to the lower-cased filename substring that
// identifies it. The short tokens (vmp, spark) match aggressively by design —
// per the detection contract we accept the small risk of a false positive on
// an oddly-named jar rather than maintain a brittle regex per mod.
var knownMods = map[string]string{
	"c2me":        "c2me",        // Concurrent Chunk Management Engine — parallel chunk I/O & gen
	"scalablelux": "scalablelux", // parallel lighting engine
	"starlight":   "starlight",   // single-threaded lighting rewrite
	"lithium":     "lithium",     // general tick-loop optimizations
	"ferritecore": "ferritecore", // memory footprint reduction
	"krypton":     "krypton",     // networking stack optimization
	"vmp":         "vmp",         // Very Many Players — server-side scaling
	"modernfix":   "modernfix",   // bug/perf fixes, faster startup
	"spark":       "spark",       // profiler (used by `tickwarden bench`)
	"servercore":  "servercore",  // dynamic sim-distance & perf tweaks
	"chunky":      "chunky",      // pre-generation tool
}

// chunkPerfMods are the mods whose presence justifies the higher per-core
// simulation budget: they take chunk generation and/or lighting off the main
// server thread, which is exactly the cost the budget guards against.
var chunkPerfMods = []string{"c2me", "scalablelux", "starlight"}

// ModSet is the set of recognized mods found in a mods directory. It is a set
// of canonical ids (see knownMods); unrecognized jars are ignored.
type ModSet struct {
	found map[string]struct{}
}

// Has reports whether the canonical mod id was detected.
func (m ModSet) Has(id string) bool {
	_, ok := m.found[id]
	return ok
}

// HasChunkPerfMods reports whether any chunk-system performance mod (C2ME,
// ScalableLux, or Starlight) is present. This is the signal the tuner uses to
// raise its per-core budget, so it maps directly onto tune.Options.PerfMods.
func (m ModSet) HasChunkPerfMods() bool {
	for _, id := range chunkPerfMods {
		if m.Has(id) {
			return true
		}
	}
	return false
}

// Names returns the detected canonical mod ids in sorted order, for display.
func (m ModSet) Names() []string {
	names := make([]string, 0, len(m.found))
	for id := range m.found {
		names = append(names, id)
	}
	sort.Strings(names)
	return names
}

// Len returns how many recognized mods were detected.
func (m ModSet) Len() int { return len(m.found) }

// ScanMods lists the *.jar files in modsDir and detects known mods by a
// case-insensitive filename substring match (see knownMods).
//
// A missing or empty directory is treated as "no mods": it returns an empty
// ModSet and a nil error, so callers can pass a default path unconditionally
// without special-casing the "no mods folder" install. Any OTHER error
// (e.g. a permission failure on a directory that does exist) is propagated —
// silently treating an unreadable mods/ as empty would hand the operator a
// vanilla-sized recommendation for a modded server.
func ScanMods(modsDir string) (ModSet, error) {
	set := ModSet{found: map[string]struct{}{}}
	if modsDir == "" {
		return set, nil
	}
	entries, err := os.ReadDir(modsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return set, nil
		}
		return set, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.EqualFold(filepath.Ext(name), ".jar") {
			continue
		}
		lower := strings.ToLower(name)
		for id, token := range knownMods {
			if strings.Contains(lower, token) {
				set.found[id] = struct{}{}
			}
		}
	}
	return set, nil
}

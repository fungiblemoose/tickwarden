package observe

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// JVMStats is the companion's /jvm reading: heap occupancy plus CUMULATIVE GC
// counters (counts and time since JVM start — that's what the MXBeans expose,
// so consumers diff successive reads, exactly like the cgroup throttle
// counters in Pressure).
type JVMStats struct {
	HeapUsed      uint64 `json:"heap_used"`
	HeapCommitted uint64 `json:"heap_committed"`
	HeapMax       uint64 `json:"heap_max"`
	GCCount       uint64 `json:"gc_count"`
	GCTimeMs      uint64 `json:"gc_time_ms"`
	// Available is set by FetchJVM on success, so a zero-value JVMStats (e.g.
	// companion too old for /jvm, or fetch failed) is distinguishable from a
	// real all-zero reading and never triggers the GC gate.
	Available bool `json:"-"`
}

// FetchJVM reads heap/GC telemetry from the companion's /jvm endpoint
// (companion >= 0.6.0). Errors are expected with older companions; callers
// should degrade to "no GC awareness" rather than fail.
func FetchJVM(url string) (JVMStats, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return JVMStats{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return JVMStats{}, fmt.Errorf("jvm endpoint returned %d", resp.StatusCode)
	}
	var s JVMStats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return JVMStats{}, err
	}
	s.Available = true
	return s, nil
}

// GCStalledNow reports whether the JVM's garbage collector — not the world —
// dominated the wall-clock window between two /jvm reads: GC time delta at/over
// pct percent of the elapsed window. A 300ms G1 pause is a 6-tick freeze that
// reads exactly like world load in the MSPT number; this is how the adaptive
// controller tells them apart (the fix is heap size / GC flags via `tickwarden
// tune`, not a smaller world).
//
// Like StarvedNow, pass prev == cur for the first poll (delta zero). A
// non-positive window or pct, or an unavailable reading, never stalls.
func GCStalledNow(prev, cur JVMStats, window time.Duration, pct float64) (bool, string) {
	if !cur.Available || !prev.Available || pct <= 0 || window <= 0 {
		return false, ""
	}
	if cur.GCTimeMs < prev.GCTimeMs {
		// JVM restarted between polls: counters reset, the delta is meaningless.
		return false, ""
	}
	deltaMs := float64(cur.GCTimeMs - prev.GCTimeMs)
	frac := 100 * deltaMs / float64(window.Milliseconds())
	if frac >= pct {
		return true, fmt.Sprintf("GC ran %.0fms in the last %s (%.0f%% of wall clock, %d collection(s))",
			deltaMs, window, frac, cur.GCCount-prev.GCCount)
	}
	return false, ""
}

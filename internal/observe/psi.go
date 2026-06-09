// Package observe implements tickwarden's differentiator: correlating in-game
// TPS dips with host-side resource starvation, entirely from *inside* the
// server's container.
//
// The key insight is that you don't need an agent on the host to know you're
// being starved. cgroup-v2 exposes Pressure Stall Information (PSI) and CPU
// throttling for your own cgroup, readable from inside the container:
//
//	/sys/fs/cgroup/<self>/cpu.pressure
//	/sys/fs/cgroup/<self>/io.pressure
//	/sys/fs/cgroup/<self>/memory.pressure
//	/sys/fs/cgroup/<self>/cpu.stat   (nr_throttled, throttled_usec)
//
// So when a noisy neighbor (say, a torrent client) saturates a shared pool, the
// symptom — your cgroup's io.pressure spiking or your CPU quota getting
// throttled — is visible to you even though the *cause* is not. Detecting "I
// was starved" is 90% of the value; naming who starved you needs a host agent
// (a later tier).
package observe

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PSI holds the "some" and "full" average-over-10s stall percentages for one
// resource. "some" = at least one task stalled; "full" = all tasks stalled.
type PSI struct {
	SomeAvg10 float64 `json:"some_avg10"`
	FullAvg10 float64 `json:"full_avg10"`
}

// Pressure is a snapshot of all three PSI resources plus CPU throttling.
type Pressure struct {
	CPU           PSI    `json:"cpu"`
	IO            PSI    `json:"io"`
	Memory        PSI    `json:"memory"`
	ThrottledUsec uint64 `json:"throttled_usec"` // cumulative CPU throttle time
	NrThrottled   uint64 `json:"nr_throttled"`   // cumulative throttle events
	Available     bool   `json:"available"`      // false if PSI couldn't be read
	// Source is the cgroup directory the worst CPU pressure came from. It
	// matters because in nested setups (notably Proxmox LXC) the workload runs
	// in a leaf like /.lxc whose own PSI reads ~0, while the binding pressure
	// shows up at an ancestor. We scan leaf→root and keep the worst.
	Source string `json:"source,omitempty"`
}

// StarvedNow reports whether this pressure snapshot shows ACTIVE host
// starvation: PSI some-avg10 at/over pct on any resource, or new CPU-quota
// throttle events since the previous snapshot.
//
// It exists for long-running controllers (the daemon's adaptive loop), where
// Correlate's "NrThrottled > 0" test would be wrong: the counters are
// cumulative since boot, so a throttle event from last week would read as
// starved forever. Comparing against prev confines the signal to "throttled
// since the last poll". For the first poll, pass prev == cur (delta zero) —
// PSI avg10 alone still detects ongoing pressure. Unreadable PSI is never
// starvation: we can't attribute, so we don't.
func StarvedNow(prev, cur Pressure, pct float64) (bool, string) {
	if !cur.Available {
		return false, ""
	}

	worst, worstName := cur.CPU.SomeAvg10, "CPU"
	if cur.IO.SomeAvg10 > worst {
		worst, worstName = cur.IO.SomeAvg10, "I/O"
	}
	if cur.Memory.SomeAvg10 > worst {
		worst, worstName = cur.Memory.SomeAvg10, "memory"
	}

	if pct > 0 && worst >= pct {
		return true, fmt.Sprintf("%s pressure some-avg10=%.1f%%", worstName, worst)
	}
	if cur.NrThrottled > prev.NrThrottled {
		return true, fmt.Sprintf("CPU quota throttled %d time(s) since last poll", cur.NrThrottled-prev.NrThrottled)
	}
	return false, ""
}

// cgroupLevels lists this process's cgroup-v2 directory and every ancestor up
// to (and including) the /sys/fs/cgroup mount root, leaf first.
//
// Walking the chain is essential: cgroup limits and the stalls they cause are
// hierarchical, and the cgroup where the workload's processes live is often
// NOT the cgroup where the binding constraint (and thus the pressure) appears.
// On a Proxmox LXC, /proc/self/cgroup reports /.lxc but the meaningful PSI sits
// at the namespace root one level up.
func cgroupLevels() []string {
	const base = "/sys/fs/cgroup"
	rel := "/"
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		rel = cgroupRel(string(b), rel)
	}
	return cgroupLevelsFrom(base, rel)
}

// cgroupRel picks the unified (0::) hierarchy path out of /proc/self/cgroup
// contents, falling back to def when no such line exists.
func cgroupRel(cgroupContents, def string) string {
	rel := def
	for _, line := range strings.Split(cgroupContents, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" {
			rel = parts[2]
		}
	}
	return rel
}

// cgroupLevelsFrom builds the leaf→root chain of cgroup directories under base
// for a relative cgroup path.
func cgroupLevelsFrom(base, rel string) []string {
	var dirs []string
	for cur := filepath.Join(base, rel); strings.HasPrefix(cur, base); cur = filepath.Dir(cur) {
		dirs = append(dirs, cur)
		if cur == base {
			break
		}
	}
	return dirs
}

// ReadPressure snapshots PSI and CPU throttling, taking the worst reading
// across the cgroup chain so a near-zero leaf doesn't mask real pressure that
// only registers at an ancestor cgroup.
func ReadPressure() Pressure {
	levels := cgroupLevels()
	if len(levels) == 0 {
		return Pressure{}
	}
	p := Pressure{}
	for _, dir := range levels {
		cpu := readPSIFile(filepath.Join(dir, "cpu.pressure"))
		io := readPSIFile(filepath.Join(dir, "io.pressure"))
		mem := readPSIFile(filepath.Join(dir, "memory.pressure"))
		if cpu.SomeAvg10 > 0 || io.SomeAvg10 > 0 || mem.SomeAvg10 > 0 {
			p.Available = true
		}
		if cpu.SomeAvg10 > p.CPU.SomeAvg10 {
			p.CPU = cpu
			p.Source = dir
		}
		if io.SomeAvg10 > p.IO.SomeAvg10 {
			p.IO = io
		}
		if mem.SomeAvg10 > p.Memory.SomeAvg10 {
			p.Memory = mem
		}
		// Throttling is accounted at whichever level actually enforces a quota;
		// keep the largest counters seen. (A host-applied limit above the
		// container's namespace ceiling stays invisible here — see Tier 2.)
		if nr, usec := readCPUStat(filepath.Join(dir, "cpu.stat")); nr > p.NrThrottled {
			p.NrThrottled, p.ThrottledUsec = nr, usec
		}
	}
	// Even all-zero PSI is a valid reading if the files existed.
	if !p.Available {
		if _, err := os.Stat(filepath.Join(levels[0], "cpu.pressure")); err == nil {
			p.Available = true
		}
	}
	return p
}

// readPSIFile parses a PSI file. Format per line:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 ...
func readPSIFile(path string) PSI {
	b, err := os.ReadFile(path)
	if err != nil {
		return PSI{}
	}
	return parsePSI(string(b))
}

// parsePSI parses PSI file contents into some/full avg10 percentages.
func parsePSI(s string) PSI {
	var psi PSI
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		avg10 := parseKV(fields, "avg10")
		switch fields[0] {
		case "some":
			psi.SomeAvg10 = avg10
		case "full":
			psi.FullAvg10 = avg10
		}
	}
	return psi
}

func readCPUStat(path string) (nrThrottled, throttledUsec uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	return parseCPUStat(string(b))
}

// parseCPUStat extracts nr_throttled and throttled_usec from cpu.stat contents.
func parseCPUStat(s string) (nrThrottled, throttledUsec uint64) {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "nr_throttled":
			nrThrottled = v
		case "throttled_usec":
			throttledUsec = v
		}
	}
	return nrThrottled, throttledUsec
}

func parseKV(fields []string, key string) float64 {
	for _, f := range fields {
		if strings.HasPrefix(f, key+"=") {
			v, _ := strconv.ParseFloat(f[len(key)+1:], 64)
			return v
		}
	}
	return 0
}

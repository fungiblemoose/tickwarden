// Package observe implements tickwarden's differentiator: correlating in-game
// TPS dips with host-side resource starvation, entirely from *inside* the
// server's container.
//
// The key insight is that you don't need an agent on the host to know you're
// being starved. cgroup-v2 exposes Pressure Stall Information (PSI) and CPU
// throttling for your own cgroup, readable from inside the container:
//
//   /sys/fs/cgroup/<self>/cpu.pressure
//   /sys/fs/cgroup/<self>/io.pressure
//   /sys/fs/cgroup/<self>/memory.pressure
//   /sys/fs/cgroup/<self>/cpu.stat   (nr_throttled, throttled_usec)
//
// So when a noisy neighbor (say, a torrent client) saturates a shared pool, the
// symptom — your cgroup's io.pressure spiking or your CPU quota getting
// throttled — is visible to you even though the *cause* is not. Detecting "I
// was starved" is 90% of the value; naming who starved you needs a host agent
// (a later tier).
package observe

import (
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
	CPU            PSI     `json:"cpu"`
	IO             PSI     `json:"io"`
	Memory         PSI     `json:"memory"`
	ThrottledUsec  uint64  `json:"throttled_usec"`  // cumulative CPU throttle time
	NrThrottled    uint64  `json:"nr_throttled"`    // cumulative throttle events
	Available      bool    `json:"available"`       // false if PSI couldn't be read
}

// cgroupRoot resolves this process's cgroup-v2 directory.
func cgroupRoot() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" {
			return filepath.Join("/sys/fs/cgroup", parts[2])
		}
	}
	return ""
}

// ReadPressure snapshots the current cgroup's PSI and CPU throttling.
func ReadPressure() Pressure {
	root := cgroupRoot()
	if root == "" {
		return Pressure{}
	}
	p := Pressure{Available: true}
	p.CPU = readPSIFile(filepath.Join(root, "cpu.pressure"))
	p.IO = readPSIFile(filepath.Join(root, "io.pressure"))
	p.Memory = readPSIFile(filepath.Join(root, "memory.pressure"))
	p.NrThrottled, p.ThrottledUsec = readCPUStat(filepath.Join(root, "cpu.stat"))
	return p
}

// readPSIFile parses a PSI file. Format per line:
//   some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//   full avg10=0.00 ...
func readPSIFile(path string) PSI {
	b, err := os.ReadFile(path)
	if err != nil {
		return PSI{}
	}
	var psi PSI
	for _, line := range strings.Split(string(b), "\n") {
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
	for _, line := range strings.Split(string(b), "\n") {
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

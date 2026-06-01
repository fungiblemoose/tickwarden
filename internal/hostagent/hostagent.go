// Package hostagent is the host-side complement to internal/observe: it runs ON
// the Proxmox VE host (not inside a container) and names the noisy neighbor.
//
// observe answers "I was starved" from inside one container. That's 90% of the
// value, but it can't say WHO did the starving — a victim cgroup sees its own
// io.pressure spike, never the aggressor saturating the shared disk/CPU pool.
// hostagent closes that gap. On a Proxmox host every LXC has a host-side cgroup
// at /sys/fs/cgroup/lxc/<CTID>/, and from there we can read every container's
// CPU usage, I/O bytes, throttling, and pressure side by side. Sample the whole
// fleet twice over a short interval, diff the cumulative counters, and the
// container hammering the disk while another sits throttled/pressured is
// directly observable — that's the "name the noisy neighbor" verdict.
//
// Design note for testability: I/O, math, and orchestration are deliberately
// separated. readSnapshot does one cgroup read (parsing + missing-file
// tolerance), computeLoad is a pure function of two snapshots plus an interval
// (all the delta/throttle/MBps math, with no clock and no filesystem), and
// Sample wires read→sleep→read→compute. Tests drive computeLoad with hand-built
// snapshots so the delta math is exercised without any real elapsed time.
package hostagent

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config holds the injectable roots so tests can point at TempDir fixtures
// instead of the live host. Zero values fall back to the real Proxmox paths.
type Config struct {
	// ScanRoot is the directory whose numeric subdirs are LXC CTIDs.
	ScanRoot string
	// PVEConfDir holds <CTID>.conf files with a "hostname:" line.
	PVEConfDir string
	// ProcRoot is "/proc"; used to resolve <pid>/comm for the comm fallback.
	ProcRoot string
	// Interval is the spacing between the two samples used to compute deltas.
	Interval time.Duration
}

// Defaults for the real Proxmox VE host. Applied to any unset field so callers
// can pass an almost-empty Config and tests can override exactly what they need.
const (
	defaultScanRoot   = "/sys/fs/cgroup/lxc"
	defaultPVEConfDir = "/etc/pve/lxc"
	defaultProcRoot   = "/proc"
	defaultInterval   = 1 * time.Second
)

func (c Config) withDefaults() Config {
	if c.ScanRoot == "" {
		c.ScanRoot = defaultScanRoot
	}
	if c.PVEConfDir == "" {
		c.PVEConfDir = defaultPVEConfDir
	}
	if c.ProcRoot == "" {
		c.ProcRoot = defaultProcRoot
	}
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	return c
}

// CgroupLoad is one container's computed resource load over the sample interval.
// CPU/throttle/pressure are rates or fractions so they're comparable regardless
// of interval; MBps is decimal megabytes/sec (bytes / seconds / 1e6).
type CgroupLoad struct {
	Path          string  `json:"path"`
	Name          string  `json:"name"`
	CTID          int     `json:"ctid"`
	CoresUsed     float64 `json:"cores_used"`     // usage_usec delta / interval-µs
	ReadMBps      float64 `json:"read_mbps"`      // io.stat rbytes delta / sec / 1e6
	WriteMBps     float64 `json:"write_mbps"`     // io.stat wbytes delta / sec / 1e6
	ThrottledFrac float64 `json:"throttled_frac"` // nr_throttled delta / nr_periods delta
	CPUPressure   float64 `json:"cpu_pressure"`   // cpu.pressure some avg10 (instantaneous)
	IOPressure    float64 `json:"io_pressure"`    // io.pressure some avg10 (instantaneous)
}

// snapshot is one read of one cgroup's raw cumulative counters plus the
// instantaneous pressure at read time. Counters are monotonic; the load comes
// from diffing two snapshots. Kept unexported: it's the seam between I/O and
// math, not part of the public contract.
type snapshot struct {
	path      string
	name      string
	ctid      int
	usageUsec uint64
	rbytes    uint64
	wbytes    uint64
	nrPeriods uint64
	nrThrott  uint64
	// Pressure is instantaneous (a moving average maintained by the kernel),
	// not a counter, so we don't diff it — we report the second read's value.
	cpuPressure float64
	ioPressure  float64
}

// Sample reads every container's cgroup twice, separated by cfg.Interval, and
// returns the computed per-container load. Reads are taken fleet-wide before
// sleeping (and again after) so all containers are diffed over the same window.
func Sample(cfg Config) ([]CgroupLoad, error) {
	cfg = cfg.withDefaults()
	ctids, err := scanCTIDs(cfg.ScanRoot)
	if err != nil {
		return nil, err
	}

	first := make(map[int]snapshot, len(ctids))
	for _, id := range ctids {
		first[id] = readSnapshot(cfg, id)
	}
	time.Sleep(cfg.Interval)

	loads := make([]CgroupLoad, 0, len(ctids))
	for _, id := range ctids {
		b := readSnapshot(cfg, id)
		loads = append(loads, computeLoad(first[id], b, cfg.Interval))
	}
	return loads, nil
}

// scanCTIDs returns the numeric subdirectory names of root as ints (the CTIDs).
// Non-numeric entries (e.g. systemd-managed siblings) are skipped, not an error.
func scanCTIDs(root string) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if id, err := strconv.Atoi(e.Name()); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids, nil
}

// readSnapshot reads one cgroup dir once. Every file is optional: a freshly
// booted or io-less container may lack io.stat, and pressure files require
// CONFIG_PSI — missing files leave the corresponding fields zero rather than
// failing the whole scan, since one broken container shouldn't blind the fleet.
func readSnapshot(cfg Config, ctid int) snapshot {
	dir := filepath.Join(cfg.ScanRoot, strconv.Itoa(ctid))
	s := snapshot{path: dir, ctid: ctid}

	s.usageUsec, s.nrPeriods, s.nrThrott = readCPUStat(filepath.Join(dir, "cpu.stat"))
	s.rbytes, s.wbytes = readIOStat(filepath.Join(dir, "io.stat"))
	s.cpuPressure = readPressureAvg10(filepath.Join(dir, "cpu.pressure"))
	s.ioPressure = readPressureAvg10(filepath.Join(dir, "io.pressure"))
	s.name = resolveName(cfg, ctid, dir)
	return s
}

// computeLoad turns two snapshots into a rate/fraction load. Pure: no clock, no
// filesystem — the interval is passed in, so tests exercise every delta path
// (including the throttle div0 guard and counter-reset guard) with fixed inputs.
func computeLoad(a, b snapshot, interval time.Duration) CgroupLoad {
	secs := interval.Seconds()
	load := CgroupLoad{
		Path:        b.path,
		Name:        b.name,
		CTID:        b.ctid,
		CPUPressure: b.cpuPressure,
		IOPressure:  b.ioPressure,
	}

	// CoresUsed: CPU-microseconds consumed per microsecond of wall time. 1.0
	// means a full core's worth; >1 means multiple cores. Compared against the
	// interval in µs so it's already core-normalized.
	if iv := float64(interval.Microseconds()); iv > 0 {
		load.CoresUsed = float64(delta(b.usageUsec, a.usageUsec)) / iv
	}

	if secs > 0 {
		load.ReadMBps = float64(delta(b.rbytes, a.rbytes)) / secs / 1e6
		load.WriteMBps = float64(delta(b.wbytes, a.wbytes)) / secs / 1e6
	}

	// ThrottledFrac: fraction of CFS periods in which this cgroup hit its CPU
	// quota and got throttled. Guard div0 — an unlimited (cpu.max=max) cgroup
	// reports periods only when a quota exists, so nr_periods delta can be 0.
	if dp := delta(b.nrPeriods, a.nrPeriods); dp > 0 {
		load.ThrottledFrac = float64(delta(b.nrThrott, a.nrThrott)) / float64(dp)
	}
	return load
}

// delta is b-a for monotonic counters, returning 0 on a decrease. A counter
// going backwards means the cgroup was recreated (container restart) between
// samples; reporting a negative-wrapped huge delta would be a phantom spike.
func delta(b, a uint64) uint64 {
	if b < a {
		return 0
	}
	return b - a
}

// readCPUStat parses cpu.stat's "usage_usec N", "nr_periods N", "nr_throttled N"
// lines. Format is one "key value" per line; we ignore the rest.
func readCPUStat(path string) (usageUsec, nrPeriods, nrThrottled uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "usage_usec":
			usageUsec = v
		case "nr_periods":
			nrPeriods = v
		case "nr_throttled":
			nrThrottled = v
		}
	}
	return usageUsec, nrPeriods, nrThrottled
}

// readIOStat sums rbytes/wbytes across every device line in io.stat. A cgroup's
// I/O is split per block device (e.g. the LVM-thin volume and a backing disk
// show as separate MAJ:MIN lines); for "who's hammering disk" the total across
// devices is what matters, so we aggregate. Line format:
//
//	MAJ:MIN rbytes=.. wbytes=.. rios=.. wios=.. dbytes=.. dios=..
func readIOStat(path string) (rbytes, wbytes uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rbytes += parseKVUint(fields, "rbytes")
		wbytes += parseKVUint(fields, "wbytes")
	}
	return rbytes, wbytes
}

// readPressureAvg10 returns the "some avg10" stall percentage from a PSI file.
// We use "some" (at least one task stalled) avg10 (last 10s) as the
// instantaneous pressure signal, matching internal/observe. Format:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 ...
func readPressureAvg10(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "some" {
			return parseKVFloat(fields, "avg10")
		}
	}
	return 0
}

// resolveName picks a human label with three fallbacks, best first:
//  1. the LXC's configured hostname from <PVEConfDir>/<CTID>.conf — the name
//     the operator actually uses, so it's the most useful;
//  2. the comm of the first PID in cgroup.procs — meaningful when the conf is
//     unreadable (e.g. /etc/pve not mounted) but the container is running;
//  3. the cgroup dir's basename — always available, last resort.
func resolveName(cfg Config, ctid int, dir string) string {
	if h := readPVEHostname(filepath.Join(cfg.PVEConfDir, strconv.Itoa(ctid)+".conf")); h != "" {
		return h
	}
	if c := firstProcComm(cfg, filepath.Join(dir, "cgroup.procs")); c != "" {
		return c
	}
	return filepath.Base(dir)
}

// readPVEHostname extracts the value of the "hostname:" line from an LXC conf.
func readPVEHostname(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "hostname:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// firstProcComm reads the first PID from cgroup.procs and returns its process
// name from <ProcRoot>/<pid>/comm. First-listed is a defensible "representative
// process" choice without scanning every PID's usage.
func firstProcComm(cfg Config, procsPath string) string {
	b, err := os.ReadFile(procsPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(cfg.ProcRoot, pid, "comm"))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(comm))
	}
	return ""
}

func parseKVUint(fields []string, key string) uint64 {
	for _, f := range fields {
		if rest, ok := strings.CutPrefix(f, key+"="); ok {
			v, _ := strconv.ParseUint(rest, 10, 64)
			return v
		}
	}
	return 0
}

func parseKVFloat(fields []string, key string) float64 {
	for _, f := range fields {
		if rest, ok := strings.CutPrefix(f, key+"="); ok {
			v, _ := strconv.ParseFloat(rest, 64)
			return v
		}
	}
	return 0
}

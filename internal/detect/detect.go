// Package detect introspects the host (and, when containerized, the cgroup
// limits that actually bound the process) to produce a Profile that the tune
// package turns into recommendations.
//
// Everything here reads Linux pseudo-filesystems (/proc, /sys/fs/cgroup). On
// non-Linux hosts the reads simply fail and the corresponding fields are left
// zero with a note appended, so the binary still builds and runs everywhere —
// it just can't fully profile a Mac. The target is always a Linux server.
package detect

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Virt describes how the workload is virtualized, which changes how we read
// resource limits: an LXC/Docker container is bounded by its cgroup, not by
// the physical host's core count or RAM.
type Virt string

const (
	VirtNone   Virt = "bare-metal"
	VirtLXC    Virt = "lxc"
	VirtDocker Virt = "docker"
	VirtVM     Virt = "vm"
	VirtOther  Virt = "container"
	VirtNA     Virt = "unknown"
)

// Profile is the detected hardware/limit picture the tuner reasons over.
type Profile struct {
	OS            string  `json:"os"`
	Virt          Virt    `json:"virt"`
	CPUModel      string  `json:"cpu_model"`
	LogicalCores  int     `json:"logical_cores"`
	PhysicalCores int     `json:"physical_cores"`
	// EffectiveCores is the parallelism the process can actually use: the
	// cgroup CPU quota when one is set, otherwise the logical core count.
	// This — not the physical core count — is what thread pools should size to.
	EffectiveCores float64 `json:"effective_cores"`
	CgroupCPUQuota float64 `json:"cgroup_cpu_quota"` // 0 = unlimited
	TotalRAMBytes  uint64  `json:"total_ram_bytes"`
	AvailRAMBytes  uint64  `json:"avail_ram_bytes"`
	CgroupMemLimit uint64  `json:"cgroup_mem_limit_bytes"` // 0 = unlimited
	// MemoryBudgetBytes is the RAM the process should plan around: the cgroup
	// memory limit when set, otherwise total host RAM.
	MemoryBudgetBytes uint64 `json:"memory_budget_bytes"`
	StorageRotational *bool  `json:"storage_rotational"` // nil = couldn't determine
	Notes             []string `json:"notes,omitempty"`
}

// Detect builds a Profile from the current host.
func Detect() Profile {
	p := Profile{
		OS:           runtime.GOOS,
		LogicalCores: runtime.NumCPU(),
		Virt:         VirtNA,
	}

	if runtime.GOOS != "linux" {
		p.note("non-Linux host (" + runtime.GOOS + "): only logical core count is reliable; deploy on the Linux target for a full profile")
		p.EffectiveCores = float64(p.LogicalCores)
		return p
	}

	p.Virt = detectVirt()
	p.CPUModel, p.PhysicalCores = readCPUInfo()
	if p.PhysicalCores == 0 {
		p.PhysicalCores = p.LogicalCores
	}
	p.TotalRAMBytes, p.AvailRAMBytes = readMeminfo()
	p.CgroupCPUQuota = readCgroupCPUMax()
	p.CgroupMemLimit = readCgroupMemMax()
	p.StorageRotational = readRotational()

	// Resolve the budgets the tuner should actually plan against.
	p.EffectiveCores = float64(p.LogicalCores)
	if p.CgroupCPUQuota > 0 && p.CgroupCPUQuota < p.EffectiveCores {
		p.EffectiveCores = p.CgroupCPUQuota
		p.note("cgroup limits CPU to " + trimFloat(p.CgroupCPUQuota) + " cores (host has " + strconv.Itoa(p.LogicalCores) + "); sizing parallelism to the quota")
	} else if p.Virt == VirtLXC {
		// Proxmox `pct cpulimit` is enforced on a host cgroup above the
		// container's namespace ceiling, so the quota is invisible from in here
		// and EffectiveCores can read high even while the container is heavily
		// throttled. PSI still catches the symptom — see `tickwarden watch`.
		p.note("LXC: a host-applied CPU cap (e.g. Proxmox cpulimit) isn't visible from inside the container; effective cores may be lower than shown — use `watch` to detect throttling via pressure")
	}

	p.MemoryBudgetBytes = p.TotalRAMBytes
	if p.CgroupMemLimit > 0 && p.CgroupMemLimit < p.MemoryBudgetBytes {
		p.MemoryBudgetBytes = p.CgroupMemLimit
		p.note("cgroup caps memory at " + humanGiB(p.CgroupMemLimit) + " (host has " + humanGiB(p.TotalRAMBytes) + ")")
	}

	return p
}

func (p *Profile) note(s string) { p.Notes = append(p.Notes, s) }

// detectVirt mirrors the cheap parts of systemd-detect-virt without shelling
// out: container hints live in /proc/1/cgroup and a couple of marker files.
func detectVirt() Virt {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return VirtDocker
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		switch {
		case strings.Contains(s, "lxc"):
			return VirtLXC
		case strings.Contains(s, "docker"):
			return VirtDocker
		}
	}
	// container=lxc is exported into PID 1's environment by LXC.
	if b, err := os.ReadFile("/proc/1/environ"); err == nil {
		s := string(b)
		if strings.Contains(s, "container=lxc") {
			return VirtLXC
		}
		if strings.Contains(s, "container=") {
			return VirtOther
		}
	}
	return VirtNone
}

func readCPUInfo() (model string, physical int) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", 0
	}
	defer f.Close()

	// Count unique (physical id, core id) pairs to get real cores, not threads.
	seen := map[string]struct{}{}
	var pkg, core string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := splitField(line)
		if !ok {
			if line == "" { // record separator
				if pkg != "" || core != "" {
					seen[pkg+"/"+core] = struct{}{}
				}
				pkg, core = "", ""
			}
			continue
		}
		switch k {
		case "model name":
			if model == "" {
				model = v
			}
		case "physical id":
			pkg = v
		case "core id":
			core = v
		}
	}
	if pkg != "" || core != "" {
		seen[pkg+"/"+core] = struct{}{}
	}
	return model, len(seen)
}

func readMeminfo() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		// Values are in kB.
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			avail = kb * 1024
		}
	}
	return total, avail
}

// readCgroupCPUMax reads the effective cgroup-v2 cpu.max for this process and
// returns the allowed core-equivalents (quota/period). Returns 0 if unlimited
// or unreadable.
func readCgroupCPUMax() float64 {
	path := selfCgroupFile("cpu.max")
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 || fields[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(fields[0], 64)
	period, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || period == 0 {
		return 0
	}
	return quota / period
}

func readCgroupMemMax() uint64 {
	path := selfCgroupFile("memory.max")
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0
	}
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

// selfCgroupFile resolves a cgroup-v2 control file for the current process by
// reading the unified hierarchy path from /proc/self/cgroup.
func selfCgroupFile(name string) string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	// cgroup v2 unified line looks like: 0::/some/path
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" {
			return filepath.Join("/sys/fs/cgroup", parts[2], name)
		}
	}
	return ""
}

// readRotational reports whether the root block device is spinning rust.
// Best-effort: returns nil if it can't be resolved (common inside containers
// where the backing device isn't visible).
func readRotational() *bool {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		// Skip loop/ram/dm pseudo-devices; we want a real physical disk.
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "dm-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/sys/block", name, "queue", "rotational"))
		if err != nil {
			continue
		}
		rot := strings.TrimSpace(string(b)) == "1"
		return &rot
	}
	return nil
}

func splitField(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func humanGiB(b uint64) string {
	return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GiB"
}

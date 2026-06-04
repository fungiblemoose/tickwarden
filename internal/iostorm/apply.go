package iostorm

// Auto-apply: actually write the cgroup-v2 io.max write throttle that Detect/Report
// only ever *recommend*. This is the opt-in, dangerous twin of the advise-only path:
// where Report hands the operator a copy-pasteable command, Apply performs the same
// mutation directly on the live host. It is intentionally additive — Detect and
// Report are unchanged and still describe, never act — so a caller that never calls
// Apply behaves exactly as before.
//
// REQUIREMENTS (this writes to /sys/fs/cgroup, a live kernel interface):
//   - Must run as root (the io.max file is root-writable only).
//   - The cgroup-v2 io controller must be delegated to the LXC subtree, i.e. "io"
//     present in the cgroup's parent cgroup.subtree_control, or the write silently
//     no-ops / ENOENTs. On a default Proxmox host the io controller is enabled.
//   - The device major:minor is REQUIRED and is NOT carried by hostagent.CgroupLoad
//     (io.stat is summed across devices upstream). Supply it explicitly via
//     ApplyOptions.Device, or call DiscoverDevice to pick the busiest write device
//     from the writer's own io.stat.
//
// SAFETY MODEL:
//   - Floor: a write cap is never set below FloorWbps (default DefaultFloorWbps).
//     A too-aggressive cap can starve the writer worse than the storm it cures, and a
//     fat-fingered tiny value (bytes instead of MB) would near-freeze the container.
//     Requested caps below the floor are clamped up and flagged (ApplyResult.Floored).
//   - Undo: every Apply first reads and returns the PRIOR io.max contents
//     (ApplyResult.PriorIOMax). Callers should persist this; Unset (write "max")
//     removes the cap. There is no automatic revert — undo is the caller's to drive.
//   - DryRun: returns the fully-resolved planned action (path, device, line, prior
//     value) WITHOUT writing anything. Use it to preview before committing.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultCgroupRoot mirrors hostagent's default LXC cgroup root. Apply writes to
// <root>/<CTID>/io.max. Overridable via ApplyOptions.CgroupRoot so tests target a
// t.TempDir and never touch the real /sys.
const DefaultCgroupRoot = "/sys/fs/cgroup/lxc"

// DefaultFloorWbps is the lowest write cap (bytes/s) Apply will ever set. 50 MB/s is
// well above latency-critical small writes yet low enough to smooth GB-scale bursts;
// it also guards against a mistaken tiny value effectively freezing the writer.
const DefaultFloorWbps int64 = 50 * 1000 * 1000 // 50 MB/s (decimal, matching MBps math)

// ioMaxUnset is the literal io.max takes (as "wbps=max") to clear a write limit.
const ioMaxUnset = "max"

// ApplyOptions configures a single Apply/Unset call. CgroupRoot and Device are the
// host-specific bits Detect cannot know; the rest tune the cap and the safety gates.
type ApplyOptions struct {
	// CgroupRoot is the LXC cgroup root. Empty means DefaultCgroupRoot. Set to a
	// t.TempDir in tests so nothing real is touched.
	CgroupRoot string

	// Device is the backing block device's major:minor (e.g. "252:0"), the value
	// io.max keys its per-device limit on. REQUIRED for Apply (Detect/CgroupLoad do
	// not carry it). Discover it with DiscoverDevice or from `lsblk -o NAME,MAJ:MIN`.
	Device string

	// Wbps is the write-bandwidth cap in bytes/s to apply. Zero means "use the
	// Storm's SuggestedWbps". Subject to the floor below.
	Wbps int64

	// FloorWbps is the minimum cap Apply will set. Zero means DefaultFloorWbps. A
	// requested cap below this is clamped up to it (ApplyResult.Floored set true).
	FloorWbps int64

	// DryRun, when true, resolves and returns the planned action without writing.
	DryRun bool
}

// ApplyResult describes what Apply/Unset did (or, under DryRun, would do). It carries
// everything needed to audit the change and to undo it.
type ApplyResult struct {
	// CgroupPath is the io.max file targeted.
	CgroupPath string
	// Device is the major:minor the limit was keyed on.
	Device string
	// AppliedWbps is the cap actually written (post-floor). Zero for Unset.
	AppliedWbps int64
	// Line is the exact bytes written to io.max ("MAJ:MIN wbps=N" or "MAJ:MIN max").
	Line string
	// PriorIOMax is io.max's full contents BEFORE the write, for undo/auditing. Empty
	// if io.max was empty or absent.
	PriorIOMax string
	// Floored is true when the requested cap was below the floor and clamped up.
	Floored bool
	// DryRun echoes whether this was a preview (nothing was written).
	DryRun bool
}

// Apply writes a per-device write throttle to the WRITER cgroup's io.max, throttling
// the storming container. It enforces the wbps floor, captures the prior io.max for
// undo, and honors DryRun. The device major:minor must be supplied via opts.Device
// (CgroupLoad does not carry it) — use DiscoverDevice to derive it.
//
// Returns an error (writing nothing) when the device is missing, the resolved cap is
// non-positive, or the cgroup io.max path is absent/unwritable.
func (s Storm) Apply(opts ApplyOptions) (ApplyResult, error) {
	root := opts.CgroupRoot
	if root == "" {
		root = DefaultCgroupRoot
	}
	device := strings.TrimSpace(opts.Device)
	if device == "" {
		return ApplyResult{}, fmt.Errorf("iostorm apply: device major:minor is required (CgroupLoad does not carry it; pass ApplyOptions.Device or use DiscoverDevice)")
	}

	wbps := opts.Wbps
	if wbps == 0 {
		wbps = s.SuggestedWbps
	}
	if wbps <= 0 {
		return ApplyResult{}, fmt.Errorf("iostorm apply: write cap must be positive, got %d", wbps)
	}
	floor := opts.FloorWbps
	if floor == 0 {
		floor = DefaultFloorWbps
	}
	floored := false
	if wbps < floor {
		wbps = floor
		floored = true
	}

	path := ioMaxPath(root, s.Writer.CTID)
	prior, err := readIOMax(path)
	if err != nil {
		return ApplyResult{}, err
	}

	line := fmt.Sprintf("%s wbps=%d", device, wbps)
	res := ApplyResult{
		CgroupPath:  path,
		Device:      device,
		AppliedWbps: wbps,
		Line:        line,
		PriorIOMax:  prior,
		Floored:     floored,
		DryRun:      opts.DryRun,
	}
	if opts.DryRun {
		return res, nil
	}
	if err := writeIOMax(path, line); err != nil {
		return ApplyResult{}, err
	}
	return res, nil
}

// Unset removes the write cap on the writer cgroup by writing "MAJ:MIN max" to
// io.max — the cgroup-v2 way to clear a per-device limit. It is the undo for Apply.
// Like Apply it captures the prior io.max, honors DryRun, and requires opts.Device
// (the same device the cap was keyed on). FloorWbps/Wbps are ignored.
func (s Storm) Unset(opts ApplyOptions) (ApplyResult, error) {
	root := opts.CgroupRoot
	if root == "" {
		root = DefaultCgroupRoot
	}
	device := strings.TrimSpace(opts.Device)
	if device == "" {
		return ApplyResult{}, fmt.Errorf("iostorm unset: device major:minor is required (pass ApplyOptions.Device or use DiscoverDevice)")
	}

	path := ioMaxPath(root, s.Writer.CTID)
	prior, err := readIOMax(path)
	if err != nil {
		return ApplyResult{}, err
	}

	line := fmt.Sprintf("%s wbps=%s", device, ioMaxUnset)
	res := ApplyResult{
		CgroupPath: path,
		Device:     device,
		Line:       line,
		PriorIOMax: prior,
		DryRun:     opts.DryRun,
	}
	if opts.DryRun {
		return res, nil
	}
	if err := writeIOMax(path, line); err != nil {
		return ApplyResult{}, err
	}
	return res, nil
}

// DiscoverDevice reads the writer cgroup's io.stat under cgroupRoot and returns the
// major:minor of the device with the most cumulative wbytes — i.e. the device the
// writer is actually hammering, which is the one a write cap should key on. This is
// the bridge across the gap CgroupLoad leaves: io.stat is per-device, but hostagent
// sums it away, so we re-read the raw file when we need the device identity.
//
// Errors if io.stat is absent/unreadable or names no device with write traffic.
func DiscoverDevice(cgroupRoot string, ctid int) (string, error) {
	if cgroupRoot == "" {
		cgroupRoot = DefaultCgroupRoot
	}
	path := filepath.Join(cgroupRoot, strconv.Itoa(ctid), "io.stat")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("iostorm discover device: read %s: %w", path, err)
	}
	bestDev := ""
	var bestW uint64
	for _, raw := range strings.Split(string(b), "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 2 {
			continue
		}
		dev := fields[0]
		if !strings.Contains(dev, ":") {
			continue
		}
		w := kvUint(fields, "wbytes")
		// Strict > keeps the first (file-order) device on ties for determinism.
		if bestDev == "" || w > bestW {
			bestDev, bestW = dev, w
		}
	}
	if bestDev == "" || bestW == 0 {
		return "", fmt.Errorf("iostorm discover device: no device with write traffic in %s", path)
	}
	return bestDev, nil
}

// ioMaxPath is the writer cgroup's io.max file under root.
func ioMaxPath(root string, ctid int) string {
	return filepath.Join(root, strconv.Itoa(ctid), "io.max")
}

// readIOMax returns io.max's current contents (trimmed) for undo/auditing. A missing
// io.max is an error: it means the cgroup path is wrong or the io controller isn't
// delegated, either of which would make a subsequent write fail — better to surface
// it now than to "succeed" writing into a directory that doesn't constrain anything.
func readIOMax(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("iostorm: read io.max %s (need root + cgroup-v2 io controller delegated): %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// writeIOMax writes one io.max directive. io.max is a kernel pseudo-file: a single
// write of "MAJ:MIN key=val ..." sets/merges that device's limits, so no truncation
// or newline games are needed.
func writeIOMax(path, line string) error {
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		return fmt.Errorf("iostorm: write io.max %s (need root + cgroup-v2 io controller delegated): %w", path, err)
	}
	return nil
}

// kvUint pulls an unsigned "key=val" field out of an io.stat line's fields. Returns 0
// if absent or unparseable (a device line may omit a counter).
func kvUint(fields []string, key string) uint64 {
	prefix := key + "="
	for _, f := range fields {
		if strings.HasPrefix(f, prefix) {
			n, err := strconv.ParseUint(f[len(prefix):], 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

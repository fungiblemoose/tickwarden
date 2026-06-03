// Package ostune inspects the OS- and kernel-level latency settings that nobody
// tools for when running a Minecraft server, and recommends fixes.
//
// A Minecraft server's tick is a single-threaded, latency-sensitive loop: every
// 50 ms it must finish or the world falls behind (TPS drops). The usual tuning
// advice stops at server.properties and JVM flags, but the host kernel quietly
// undermines all of that. A CPU stuck in `powersave` clocks down between ticks;
// a high `swappiness` lets the kernel page the JVM heap to disk, freezing the
// loop for whole seconds when it touches that memory again; Transparent Huge
// Page defrag stalls the allocating thread mid-tick; the wrong I/O scheduler
// adds queueing latency to chunk saves. These are exactly the knobs the JVM and
// game config can't reach, and they cause stutter that no amount of in-game
// tuning explains.
//
// Every root is injectable via Config so tests run against t.TempDir fixtures
// and the package never has to touch a live host. Scan emits a Finding ONLY for
// a knob that is not already optimal — an already-tuned host yields an empty
// slice — so Report's "only flags what needs fixing" contract holds trivially.
package ostune

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/fungiblemoose/tickwarden/internal/confidence"
)

// Confidence flags how much to trust a finding. Aliased from the shared
// confidence package; the const names below stay local for readability, but the
// type and values are the same ones tune uses, so reports read consistently.
type Confidence = confidence.Confidence

const (
	// Solid: well-established, low-risk for a latency-sensitive single-thread
	// workload (governor, swappiness, THP).
	Solid = confidence.Solid
	// Heuristic: a sane default, but validate against your load (I/O scheduler).
	Heuristic = confidence.Heuristic
	// Contested: folklore or genuinely workload-dependent; the Reason says why.
	Contested = confidence.Contested
)

// Finding describes one suboptimal kernel knob and how to fix it. A knob that is
// already optimal produces NO Finding — the absence is the "all good" signal.
type Finding struct {
	Knob        string     `json:"knob"`        // human label, e.g. "cpu governor"
	Current     string     `json:"current"`     // the value read from the host
	Recommended string     `json:"recommended"` // what to set it to
	Reason      string     `json:"reason"`      // WHY it matters for tick latency
	ApplyCmd    string     `json:"apply_cmd"`   // exact shell to fix it
	Confidence  Confidence `json:"confidence"`
}

// Config injects the filesystem roots so tests can point them at fixtures. The
// zero value is unusable; callers should start from DefaultConfig().
type Config struct {
	SysRoot  string // default "/sys"
	ProcRoot string // default "/proc"
}

// DefaultConfig returns the production roots.
func DefaultConfig() Config { return Config{SysRoot: "/sys", ProcRoot: "/proc"} }

// withDefaults fills any empty root from DefaultConfig so a Config{} still works.
func (c Config) withDefaults() Config {
	if c.SysRoot == "" {
		c.SysRoot = "/sys"
	}
	if c.ProcRoot == "" {
		c.ProcRoot = "/proc"
	}
	return c
}

// Scan reads every knob and returns a Finding for each one that is NOT already
// optimal. Missing files/dirs are tolerated (a VM with no cpufreq, a kernel
// without THP): such knobs are simply skipped. The error return is reserved for
// a future failure mode; today Scan always returns nil so an empty host (e.g.
// running on macOS, or an empty fixture) yields (nil, nil), not an error.
func Scan(cfg Config) ([]Finding, error) {
	cfg = cfg.withDefaults()
	var findings []Finding
	findings = appendNonNil(findings, scanGovernor(cfg))
	findings = appendNonNil(findings, scanSwappiness(cfg))
	findings = append(findings, scanTHP(cfg)...)
	findings = append(findings, scanIOSchedulers(cfg)...)
	return findings, nil
}

func appendNonNil(dst []Finding, f *Finding) []Finding {
	if f != nil {
		dst = append(dst, *f)
	}
	return dst
}

// --- governor -------------------------------------------------------------

// scanGovernor inspects the CPU frequency scaling governor. The tick thread
// alternates between a burst of work and an idle gap every 50 ms; on `powersave`
// (or `ondemand`/`conservative`) the CPU sees the idle gap and drops to a low
// P-state, so the *next* tick starts on a cold, slow core and the ramp-up eats
// into the 50 ms budget. `performance` pins the max P-state; `schedutil` is the
// modern scheduler-driven governor and reacts fast enough to be acceptable.
//
// It emits a single aggregate Finding across all policy* directories, because
// the standard fix (`echo performance | tee policy*/scaling_governor`) sets them
// all at once. It also reads scaling_available_governors so it never recommends
// `performance` on a host that doesn't offer it.
func scanGovernor(cfg Config) *Finding {
	base := filepath.Join(cfg.SysRoot, "devices", "system", "cpu", "cpufreq")
	policies, _ := filepath.Glob(filepath.Join(base, "policy*"))
	sort.Strings(policies)

	var current []string
	available := map[string]bool{}
	for _, p := range policies {
		g := strings.TrimSpace(readFile(filepath.Join(p, "scaling_governor")))
		if g == "" {
			continue
		}
		current = append(current, g)
		for _, a := range strings.Fields(readFile(filepath.Join(p, "scaling_available_governors"))) {
			available[a] = true
		}
	}
	if len(current) == 0 {
		return nil // no cpufreq (common in VMs/containers) — nothing to recommend
	}

	// Optimal iff every policy is already performance- or schedutil-class.
	if allGovernorsAcceptable(current) {
		return nil
	}

	rec := "performance"
	reason := "tick loop idles between ticks; powersave/ondemand drop the core to a low P-state so the next tick starts cold and the clock ramp eats into the 50ms budget. performance pins max frequency."
	// Honesty: if the host can't offer performance, recommend the best it has.
	if len(available) > 0 && !available["performance"] {
		switch {
		case available["schedutil"]:
			rec = "schedutil"
			reason = "this host doesn't offer performance; schedutil is the scheduler-driven governor and ramps fast enough to keep tick latency low (far better than powersave/ondemand)."
		default:
			rec = bestAvailableGovernor(available)
			reason = "this host doesn't offer performance or schedutil; " + rec + " is the least latency-harming governor available."
		}
	}

	cur := strings.Join(uniqueStrings(current), ",")
	return &Finding{
		Knob:        "cpu governor",
		Current:     cur,
		Recommended: rec,
		Reason:      reason,
		ApplyCmd:    fmt.Sprintf("echo %s | sudo tee /sys/devices/system/cpu/cpufreq/policy*/scaling_governor", rec),
		Confidence:  Solid,
	}
}

// allGovernorsAcceptable reports whether every policy's governor is one we'd not
// flag: performance (ideal) or schedutil (acceptable). A single bad policy makes
// the whole set suboptimal — one slow core can host the tick thread.
func allGovernorsAcceptable(govs []string) bool {
	for _, g := range govs {
		if g != "performance" && g != "schedutil" {
			return false
		}
	}
	return true
}

// bestAvailableGovernor picks the least-bad governor when neither performance
// nor schedutil exists, preferring more-responsive ones.
func bestAvailableGovernor(available map[string]bool) string {
	for _, g := range []string{"ondemand", "conservative", "powersave"} {
		if available[g] {
			return g
		}
	}
	// Can't-happen default: callers only reach here when the available-set is
	// non-empty yet lacks every governor we know; advise the canonical target.
	return "performance"
}

// --- swappiness -----------------------------------------------------------

// swappinessCeiling is the highest vm.swappiness we'll accept without flagging.
// Anything above this risks the kernel paging out cold JVM heap; the next GC or
// tick that touches it faults the page back from disk, stalling the single tick
// thread for tens of milliseconds to seconds. We don't push to 0 — a little
// swap headroom under genuine memory pressure beats the OOM killer — so 10 is
// the conventional "swap only in emergencies" ceiling.
const swappinessCeiling = 10

// scanSwappiness reads /proc/sys/vm/swappiness.
func scanSwappiness(cfg Config) *Finding {
	raw := strings.TrimSpace(readFile(filepath.Join(cfg.ProcRoot, "sys", "vm", "swappiness")))
	if raw == "" {
		return nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return nil // unparseable — don't fabricate a recommendation
	}
	if v <= swappinessCeiling {
		return nil
	}
	return &Finding{
		Knob:        "vm.swappiness",
		Current:     raw,
		Recommended: "1",
		Reason:      "high swappiness lets the kernel page out cold JVM heap; touching it again (a GC, or a tick) faults it back from disk and freezes the single tick thread for tens of ms to seconds. Keep swap for emergencies only.",
		ApplyCmd:    "sudo sysctl -w vm.swappiness=1   # persist in /etc/sysctl.d/99-minecraft.conf",
		Confidence:  Solid,
	}
}

// --- transparent huge pages ----------------------------------------------

// scanTHP inspects THP `enabled` and `defrag`. Both files share the
// "always [madvise] never" format where the bracketed token is active. Aikar's
// well-known JVM guidance is to disable THP (set "never") or at least scope it
// to "madvise": with "always", the kernel synchronously compacts memory to form
// a 2 MB page during an allocation, and that defrag stall lands on whatever
// thread allocated — frequently the tick thread mid-tick.
func scanTHP(cfg Config) []Finding {
	base := filepath.Join(cfg.SysRoot, "kernel", "mm", "transparent_hugepage")
	var out []Finding
	if f := scanTHPKnob(filepath.Join(base, "enabled"), "transparent_hugepage/enabled",
		"THP 'always' makes the kernel synchronously compact memory into 2MB pages during allocations; that defrag stall lands on the allocating thread — often the tick thread mid-tick. Aikar's JVM guidance is 'never' (or at least 'madvise').",
		"/sys/kernel/mm/transparent_hugepage/enabled"); f != nil {
		out = append(out, *f)
	}
	if f := scanTHPKnob(filepath.Join(base, "defrag"), "transparent_hugepage/defrag",
		"THP defrag 'always' forces synchronous memory compaction on allocation, stalling the allocating (tick) thread. Set 'never', or 'madvise' to compact only where explicitly requested.",
		"/sys/kernel/mm/transparent_hugepage/defrag"); f != nil {
		out = append(out, *f)
	}
	return out
}

// scanTHPKnob handles one THP file. "never" is optimal; "madvise" is acceptable
// (no finding, per Aikar's "or at least madvise"); "always" is flagged.
func scanTHPKnob(path, knob, reason, applyPath string) *Finding {
	raw := readFile(path)
	active := activeBracketed(raw)
	if active == "" {
		return nil // THP node absent — nothing to recommend
	}
	if active == "never" || active == "madvise" {
		return nil
	}
	return &Finding{
		Knob:        knob,
		Current:     active,
		Recommended: "never",
		Reason:      reason,
		ApplyCmd:    fmt.Sprintf("echo never | sudo tee %s", applyPath),
		Confidence:  Solid,
	}
}

// --- I/O scheduler --------------------------------------------------------

// skipDevicePrefixes lists block-device name prefixes that aren't real disks the
// server saves chunks to: loopback mounts, ramdisks, device-mapper/LVM targets
// (the scheduler that matters is on the underlying physical device), zram, and
// optical drives. Tuning their scheduler is pointless or harmful.
var skipDevicePrefixes = []string{"loop", "ram", "dm-", "zram", "sr", "fd", "md"}

// scanIOSchedulers inspects each real block device's queue scheduler. Format is
// "[mq-deadline] none kyber" where the bracketed token is active. For SSD/NVMe
// the request reordering a deadline scheduler does buys little and adds latency,
// so "none" (or the lightweight "mq-deadline") is preferred; "bfq"/"cfq" and
// friends add queueing latency that shows up as slow, bursty chunk saves.
//
// Unlike the governor, disks are individually named and a fix targets one device
// path, so this emits ONE finding per flagged disk rather than aggregating.
// Rotational (spinning) disks are skipped: there the reordering is useful, so
// "prefer none" doesn't apply and we'd otherwise advise against our own reason.
func scanIOSchedulers(cfg Config) []Finding {
	devs, _ := filepath.Glob(filepath.Join(cfg.SysRoot, "block", "*"))
	sort.Strings(devs)
	var out []Finding
	for _, dev := range devs {
		name := filepath.Base(dev)
		if shouldSkipDevice(name) {
			continue
		}
		// Spinning disks genuinely benefit from a scheduler's seek reordering, so
		// the "prefer none" advice is solid-state-only — skip rotational devices
		// rather than hand out a recommendation that contradicts its own reason.
		if strings.TrimSpace(readFile(filepath.Join(dev, "queue", "rotational"))) == "1" {
			continue
		}
		active := activeBracketed(readFile(filepath.Join(dev, "queue", "scheduler")))
		if active == "" {
			continue // no scheduler file (e.g. some virtio) — skip
		}
		if active == "none" || active == "mq-deadline" {
			continue
		}
		out = append(out, Finding{
			Knob:        "io scheduler (" + name + ")",
			Current:     active,
			Recommended: "none",
			Reason:      fmt.Sprintf("%q reorders/queues requests; on SSD/NVMe that adds latency for little gain, so chunk saves stall in bursts. Prefer 'none' or the lightweight 'mq-deadline'.", active),
			ApplyCmd:    fmt.Sprintf("echo none | sudo tee /sys/block/%s/queue/scheduler   # persist via udev rule", name),
			Confidence:  Heuristic,
		})
	}
	return out
}

func shouldSkipDevice(name string) bool {
	for _, p := range skipDevicePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// --- shared parsing -------------------------------------------------------

// activeBracketed extracts the active token from the kernel's "list with the
// selected item in brackets" format, e.g. "always [madvise] never" → "madvise"
// and "[mq-deadline] none" → "mq-deadline". Returns "" if there's no bracketed
// token (file absent, empty, or a format we don't recognise).
func activeBracketed(s string) string {
	for _, tok := range strings.Fields(s) {
		if len(tok) >= 2 && tok[0] == '[' && tok[len(tok)-1] == ']' {
			return tok[1 : len(tok)-1]
		}
	}
	return ""
}

// readFile returns a file's contents, or "" on any error. Missing-file
// tolerance is the norm here: absent knobs just mean "nothing to recommend".
func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Report renders the findings as a human-readable table. Because Scan only
// returns knobs that need changing, every row here is something to fix; an empty
// slice means the host is already tuned, and we say so.
func Report(findings []Finding) string {
	var b strings.Builder
	if len(findings) == 0 {
		b.WriteString("OS tuning: all inspected kernel knobs already latency-optimal (governor, swappiness, THP, I/O scheduler).\n")
		return b.String()
	}
	b.WriteString("OS-level tick-latency findings (only knobs that need changing are shown):\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "  [%s] %s: %s → %s\n", f.Confidence, f.Knob, f.Current, f.Recommended)
		fmt.Fprintf(&b, "        ↳ %s\n", f.Reason)
		fmt.Fprintf(&b, "        $ %s\n", f.ApplyCmd)
	}
	b.WriteString("\nLegend: solid = trust it · heuristic = sane default · contested = validate against your load\n")
	b.WriteString("Note: echo|tee writes to /sys and /proc are runtime-only and reset on reboot — persist them (sysctl.d, udev, kernel cmdline, or your init).\n")
	return b.String()
}

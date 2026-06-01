package ostune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates path with content, making parent dirs as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureRoots returns a (sysRoot, procRoot) pair under t.TempDir.
func fixtureRoots(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	return filepath.Join(root, "sys"), filepath.Join(root, "proc")
}

func policyDir(sysRoot, name string) string {
	return filepath.Join(sysRoot, "devices", "system", "cpu", "cpufreq", name)
}

func find(findings []Finding, knobPrefix string) (Finding, bool) {
	for _, f := range findings {
		if strings.HasPrefix(f.Knob, knobPrefix) {
			return f, true
		}
	}
	return Finding{}, false
}

// --- activeBracketed (shared parser) -------------------------------------

func TestActiveBracketed(t *testing.T) {
	cases := map[string]string{
		"always [madvise] never":   "madvise",
		"[always] madvise never":   "always",
		"always madvise [never]":   "never",
		"[mq-deadline] none kyber": "mq-deadline",
		"mq-deadline [none]":       "none",
		"none mq-deadline":         "", // nothing bracketed
		"":                         "",
		"  [madvise]  ":            "madvise",
	}
	for in, want := range cases {
		if got := activeBracketed(in); got != want {
			t.Errorf("activeBracketed(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- governor -------------------------------------------------------------

func TestGovernorPowersaveFlagged(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_governor"), "powersave\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_available_governors"), "performance schedutil powersave\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy1"), "scaling_governor"), "powersave\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy1"), "scaling_available_governors"), "performance schedutil powersave\n")

	got, err := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if err != nil {
		t.Fatal(err)
	}
	f, ok := find(got, "cpu governor")
	if !ok {
		t.Fatal("expected a governor finding")
	}
	if f.Recommended != "performance" {
		t.Errorf("recommended = %q, want performance", f.Recommended)
	}
	if f.Confidence != Solid {
		t.Errorf("confidence = %q, want solid", f.Confidence)
	}
	if !strings.Contains(f.ApplyCmd, "scaling_governor") {
		t.Errorf("apply cmd missing scaling_governor path: %q", f.ApplyCmd)
	}
}

func TestGovernorPerformanceOptimal(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_governor"), "performance\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_available_governors"), "performance schedutil powersave\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "cpu governor"); ok {
		t.Errorf("performance governor should produce no finding; got %+v", got)
	}
}

func TestGovernorSchedutilAcceptable(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_governor"), "schedutil\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_available_governors"), "performance schedutil\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "cpu governor"); ok {
		t.Errorf("schedutil should be accepted (no finding); got %+v", got)
	}
}

func TestGovernorMixedFlagged(t *testing.T) {
	// One bad policy among good ones must still flag — the tick thread can land
	// on the slow core.
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_governor"), "performance\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_available_governors"), "performance powersave\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy1"), "scaling_governor"), "powersave\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy1"), "scaling_available_governors"), "performance powersave\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "cpu governor"); !ok {
		t.Errorf("mixed governors should flag; got %+v", got)
	}
}

func TestGovernorPerformanceUnavailableRecommendsSchedutil(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_governor"), "powersave\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_available_governors"), "schedutil powersave\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	f, ok := find(got, "cpu governor")
	if !ok {
		t.Fatal("expected a governor finding")
	}
	if f.Recommended != "schedutil" {
		t.Errorf("recommended = %q, want schedutil (performance unavailable)", f.Recommended)
	}
}

// --- swappiness -----------------------------------------------------------

func TestSwappinessHighFlagged(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(proc, "sys", "vm", "swappiness"), "60\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	f, ok := find(got, "vm.swappiness")
	if !ok {
		t.Fatal("expected a swappiness finding")
	}
	if f.Current != "60" || f.Confidence != Solid {
		t.Errorf("got current=%q conf=%q", f.Current, f.Confidence)
	}
}

func TestSwappinessLowOptimal(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(proc, "sys", "vm", "swappiness"), "1\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "vm.swappiness"); ok {
		t.Errorf("swappiness=1 should be optimal; got %+v", got)
	}
}

func TestSwappinessCeilingBoundary(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(proc, "sys", "vm", "swappiness"), "10\n")
	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "vm.swappiness"); ok {
		t.Errorf("swappiness=10 (== ceiling) should be optimal; got %+v", got)
	}
	writeFile(t, filepath.Join(proc, "sys", "vm", "swappiness"), "11\n")
	got, _ = Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "vm.swappiness"); !ok {
		t.Errorf("swappiness=11 (> ceiling) should flag")
	}
}

func TestSwappinessUnparseable(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(proc, "sys", "vm", "swappiness"), "garbage\n")
	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "vm.swappiness"); ok {
		t.Errorf("unparseable swappiness should not fabricate a finding")
	}
}

// --- THP ------------------------------------------------------------------

func TestTHPAlwaysFlagged(t *testing.T) {
	sys, proc := fixtureRoots(t)
	base := filepath.Join(sys, "kernel", "mm", "transparent_hugepage")
	writeFile(t, filepath.Join(base, "enabled"), "[always] madvise never\n")
	writeFile(t, filepath.Join(base, "defrag"), "[always] defer madvise never\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	fe, ok := find(got, "transparent_hugepage/enabled")
	if !ok {
		t.Fatal("expected THP enabled finding")
	}
	if fe.Recommended != "never" || fe.Confidence != Solid {
		t.Errorf("THP enabled finding wrong: %+v", fe)
	}
	if _, ok := find(got, "transparent_hugepage/defrag"); !ok {
		t.Error("expected THP defrag finding")
	}
}

func TestTHPMadviseAndNeverOptimal(t *testing.T) {
	sys, proc := fixtureRoots(t)
	base := filepath.Join(sys, "kernel", "mm", "transparent_hugepage")
	writeFile(t, filepath.Join(base, "enabled"), "always [madvise] never\n")
	writeFile(t, filepath.Join(base, "defrag"), "always defer madvise [never]\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "transparent_hugepage"); ok {
		t.Errorf("madvise/never THP should be optimal; got %+v", got)
	}
}

func TestTHPDefragDeferMadviseFlagged(t *testing.T) {
	// "defer+madvise" is an active defrag mode that still allows synchronous
	// compaction under madvise; it isn't "never"/"madvise", so it's flagged.
	sys, proc := fixtureRoots(t)
	base := filepath.Join(sys, "kernel", "mm", "transparent_hugepage")
	writeFile(t, filepath.Join(base, "defrag"), "always defer [defer+madvise] madvise never\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	f, ok := find(got, "transparent_hugepage/defrag")
	if !ok {
		t.Fatal("expected THP defrag finding for defer+madvise")
	}
	if f.Current != "defer+madvise" || f.Recommended != "never" {
		t.Errorf("got current=%q recommended=%q", f.Current, f.Recommended)
	}
}

// --- I/O scheduler --------------------------------------------------------

func TestIOSchedulerBFQFlaggedSSD(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(sys, "block", "nvme0n1", "queue", "scheduler"), "[bfq] mq-deadline none\n")
	writeFile(t, filepath.Join(sys, "block", "nvme0n1", "queue", "rotational"), "0\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	f, ok := find(got, "io scheduler (nvme0n1)")
	if !ok {
		t.Fatal("expected io scheduler finding")
	}
	if f.Confidence != Heuristic {
		t.Errorf("SSD io scheduler should be heuristic; got %q", f.Confidence)
	}
	if !strings.Contains(f.ApplyCmd, "/sys/block/nvme0n1/queue/scheduler") {
		t.Errorf("apply cmd should target the device path: %q", f.ApplyCmd)
	}
}

func TestIOSchedulerRotationalSkipped(t *testing.T) {
	// On a spinning disk, the scheduler's seek reordering is useful, so the
	// "prefer none" advice doesn't apply — we skip rather than mis-recommend.
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(sys, "block", "sda", "queue", "scheduler"), "[bfq] mq-deadline none\n")
	writeFile(t, filepath.Join(sys, "block", "sda", "queue", "rotational"), "1\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "io scheduler (sda)"); ok {
		t.Errorf("rotational disk should be skipped, not flagged; got %+v", got)
	}
}

func TestIOSchedulerNoneOptimal(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(sys, "block", "nvme0n1", "queue", "scheduler"), "[none] mq-deadline\n")
	writeFile(t, filepath.Join(sys, "block", "sdb", "queue", "scheduler"), "[mq-deadline] none\n")

	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "io scheduler"); ok {
		t.Errorf("none/mq-deadline should be optimal; got %+v", got)
	}
}

func TestIOSchedulerSkipsVirtualDevices(t *testing.T) {
	sys, proc := fixtureRoots(t)
	for _, dev := range []string{"loop0", "ram0", "dm-0", "zram0", "sr0", "md0"} {
		writeFile(t, filepath.Join(sys, "block", dev, "queue", "scheduler"), "[bfq] none\n")
	}
	got, _ := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if _, ok := find(got, "io scheduler"); ok {
		t.Errorf("virtual/loop/dm devices must be skipped; got %+v", got)
	}
}

// --- whole-scan behavior --------------------------------------------------

func TestScanAllOptimalNoFindings(t *testing.T) {
	sys, proc := fixtureRoots(t)
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_governor"), "performance\n")
	writeFile(t, filepath.Join(policyDir(sys, "policy0"), "scaling_available_governors"), "performance schedutil\n")
	writeFile(t, filepath.Join(proc, "sys", "vm", "swappiness"), "1\n")
	base := filepath.Join(sys, "kernel", "mm", "transparent_hugepage")
	writeFile(t, filepath.Join(base, "enabled"), "always [never]\n")
	writeFile(t, filepath.Join(base, "defrag"), "always [never]\n")
	writeFile(t, filepath.Join(sys, "block", "nvme0n1", "queue", "scheduler"), "[none] mq-deadline\n")

	got, err := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("fully-tuned host should yield no findings; got %+v", got)
	}
	rep := Report(got)
	if !strings.Contains(rep, "already latency-optimal") {
		t.Errorf("empty Report should say all-optimal; got %q", rep)
	}
}

func TestScanMissingFilesTolerated(t *testing.T) {
	// Empty roots (e.g. a VM with no cpufreq/THP, or running on macOS): Scan
	// must return (nil, nil), never an error.
	sys, proc := fixtureRoots(t)
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Scan(Config{SysRoot: sys, ProcRoot: proc})
	if err != nil {
		t.Fatalf("missing files should be tolerated, got err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("no knobs present should yield no findings; got %+v", got)
	}
}

func TestScanDefaultConfigNonexistentRootsNoPanic(t *testing.T) {
	// A Config with bogus roots (stand-in for production roots absent on this
	// test machine) must not panic and must not error.
	got, err := Scan(Config{SysRoot: filepath.Join(t.TempDir(), "nope"), ProcRoot: filepath.Join(t.TempDir(), "nope")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("absent roots should yield no findings; got %+v", got)
	}
}

func TestReportFlagsOnlyNonOptimal(t *testing.T) {
	findings := []Finding{
		{Knob: "vm.swappiness", Current: "60", Recommended: "1", Reason: "r", ApplyCmd: "c", Confidence: Solid},
	}
	rep := Report(findings)
	if !strings.Contains(rep, "vm.swappiness") || !strings.Contains(rep, "[solid]") {
		t.Errorf("report should show the finding with its confidence; got %q", rep)
	}
	if !strings.Contains(rep, "reset on reboot") {
		t.Errorf("report should warn that /sys-/proc writes are non-persistent; got %q", rep)
	}
}

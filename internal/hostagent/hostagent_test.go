package hostagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile creates dir/name with content, making parent dirs as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureConfig builds a Config rooted at TempDir mirroring a Proxmox host:
//
//	<scan>/110/  heavy writer, hostname in conf  → conf-name fallback + culprit
//	<scan>/120/  throttled + io.pressure, comm   → comm-name fallback + victim
//	<scan>/130/  idle, no conf, no procs         → dir-name fallback
//
// Each cgroup dir gets two states (a/b) for cpu.stat and io.stat so callers can
// read "before" and "after" snapshots; pressure files hold the post-interval
// (instantaneous) values directly.
func fixtureConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	scan := filepath.Join(root, "cgroup", "lxc")
	conf := filepath.Join(root, "pve", "lxc")
	proc := filepath.Join(root, "proc")

	// --- CT 110: the writer. hostname conf present. ---
	writeFile(t, filepath.Join(conf, "110.conf"), "arch: amd64\nhostname: torrentbox\nmemory: 2048\n")
	writeFile(t, filepath.Join(scan, "110", "cpu.pressure"),
		"some avg10=0.10 avg60=0.05 avg300=0.01 total=1\nfull avg10=0.00 total=0\n")
	writeFile(t, filepath.Join(scan, "110", "io.pressure"),
		"some avg10=2.00 avg60=1.00 avg300=0.20 total=99\nfull avg10=0.50 total=10\n")

	// --- CT 120: the victim. no conf; cgroup.procs → comm "java". ---
	writeFile(t, filepath.Join(scan, "120", "cgroup.procs"), "4242\n4243\n")
	writeFile(t, filepath.Join(proc, "4242", "comm"), "java\n")
	writeFile(t, filepath.Join(scan, "120", "cpu.pressure"),
		"some avg10=8.00 avg60=4.00 avg300=1.00 total=500\nfull avg10=1.00 total=20\n")
	writeFile(t, filepath.Join(scan, "120", "io.pressure"),
		"some avg10=30.00 avg60=20.00 avg300=5.00 total=9000\nfull avg10=10.00 total=900\n")

	// --- CT 130: idle. no conf, no procs → dir-name fallback. ---
	writeFile(t, filepath.Join(scan, "130", "cpu.stat"),
		"usage_usec 1000\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n")

	// A non-numeric sibling dir that scanCTIDs must ignore.
	writeFile(t, filepath.Join(scan, "init.scope", "cpu.stat"), "usage_usec 0\n")

	return Config{ScanRoot: scan, PVEConfDir: conf, ProcRoot: proc, Interval: time.Second}
}

func TestReadIOStat_SumsDevices(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "io.stat")
	writeFile(t, p, "252:0 rbytes=100 wbytes=200 rios=1 wios=2 dbytes=0 dios=0\n"+
		"252:1 rbytes=50 wbytes=25 rios=1 wios=1\n")
	r, w := readIOStat(p)
	if r != 150 || w != 225 {
		t.Fatalf("io.stat sum: got r=%d w=%d, want r=150 w=225", r, w)
	}
	// Missing file must be tolerated as zeros.
	if r, w := readIOStat(filepath.Join(dir, "nope")); r != 0 || w != 0 {
		t.Fatalf("missing io.stat should be 0/0, got r=%d w=%d", r, w)
	}
}

func TestReadCPUStat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cpu.stat")
	writeFile(t, p, "usage_usec 5000000\nuser_usec 1\nnr_periods 100\nnr_throttled 30\nthrottled_usec 7\n")
	u, np, nt := readCPUStat(p)
	if u != 5000000 || np != 100 || nt != 30 {
		t.Fatalf("cpu.stat: got usage=%d periods=%d throttled=%d", u, np, nt)
	}
}

func TestReadPressureAvg10(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "io.pressure")
	writeFile(t, p, "some avg10=12.34 avg60=5 avg300=1 total=9\nfull avg10=3 total=2\n")
	if got := readPressureAvg10(p); got != 12.34 {
		t.Fatalf("avg10: got %v want 12.34", got)
	}
	if got := readPressureAvg10(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("missing pressure file should be 0, got %v", got)
	}
}

// computeLoad is pure, so the full delta/MBps/throttle math is testable with a
// fixed interval and zero real elapsed time.
func TestComputeLoad_Deltas(t *testing.T) {
	a := snapshot{usageUsec: 1_000_000, rbytes: 0, wbytes: 0, nrPeriods: 100, nrThrott: 10}
	// Over 2s: +2 core-seconds CPU → 1.0 core; +200MB write → 100 MB/s; +50/+100 periods/throttle.
	b := snapshot{
		path: "/x/110", name: "torrentbox", ctid: 110,
		usageUsec: 1_000_000 + 2_000_000,
		rbytes:    20_000_000, wbytes: 200_000_000,
		nrPeriods: 200, nrThrott: 60,
		cpuPressure: 1.5, ioPressure: 9.0,
	}
	l := computeLoad(a, b, 2*time.Second)
	if !approx(l.CoresUsed, 1.0) {
		t.Errorf("CoresUsed: got %v want 1.0", l.CoresUsed)
	}
	if !approx(l.WriteMBps, 100.0) {
		t.Errorf("WriteMBps: got %v want 100.0", l.WriteMBps)
	}
	if !approx(l.ReadMBps, 10.0) {
		t.Errorf("ReadMBps: got %v want 10.0", l.ReadMBps)
	}
	// nr_throttled delta=50, nr_periods delta=100 → 0.5
	if !approx(l.ThrottledFrac, 0.5) {
		t.Errorf("ThrottledFrac: got %v want 0.5", l.ThrottledFrac)
	}
	if l.CPUPressure != 1.5 || l.IOPressure != 9.0 {
		t.Errorf("pressure should come from second snapshot: cpu=%v io=%v", l.CPUPressure, l.IOPressure)
	}
	if l.Name != "torrentbox" || l.CTID != 110 || l.Path != "/x/110" {
		t.Errorf("identity not carried from second snapshot: %+v", l)
	}
}

func TestComputeLoad_Div0AndReset(t *testing.T) {
	// nr_periods delta = 0 (unlimited cgroup) must not divide by zero.
	a := snapshot{nrPeriods: 5, nrThrott: 5}
	b := snapshot{nrPeriods: 5, nrThrott: 5}
	if got := computeLoad(a, b, time.Second).ThrottledFrac; got != 0 {
		t.Fatalf("div0 guard: ThrottledFrac got %v want 0", got)
	}
	// Counter reset (container restart): b<a must clamp to 0, not wrap huge.
	hi := snapshot{usageUsec: 9_000_000, wbytes: 9_000_000}
	lo := snapshot{usageUsec: 10, wbytes: 10}
	l := computeLoad(hi, lo, time.Second)
	if l.CoresUsed != 0 || l.WriteMBps != 0 {
		t.Fatalf("reset guard: got cores=%v write=%v want 0/0", l.CoresUsed, l.WriteMBps)
	}
}

func TestResolveName_AllThreeFallbacks(t *testing.T) {
	cfg := fixtureConfig(t)
	// 110: conf hostname.
	if got := resolveName(cfg, 110, filepath.Join(cfg.ScanRoot, "110")); got != "torrentbox" {
		t.Errorf("110 should resolve to conf hostname, got %q", got)
	}
	// 120: no conf → comm of first PID.
	if got := resolveName(cfg, 120, filepath.Join(cfg.ScanRoot, "120")); got != "java" {
		t.Errorf("120 should resolve to comm 'java', got %q", got)
	}
	// 130: no conf, no procs → dir basename.
	if got := resolveName(cfg, 130, filepath.Join(cfg.ScanRoot, "130")); got != "130" {
		t.Errorf("130 should fall back to dir name '130', got %q", got)
	}
}

func TestScanCTIDs_SkipsNonNumeric(t *testing.T) {
	cfg := fixtureConfig(t)
	ids, err := scanCTIDs(cfg.ScanRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{110, 120, 130}
	if len(ids) != len(want) {
		t.Fatalf("scanCTIDs got %v want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("scanCTIDs got %v want %v", ids, want)
		}
	}
}

func TestSample_FixtureFleet(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Interval = time.Millisecond // single-read fixtures → deltas are 0, fine
	loads, err := Sample(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(loads) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(loads))
	}
	// Sample must still resolve names and instantaneous pressure even though the
	// static fixtures yield zero counter deltas.
	byID := map[int]CgroupLoad{}
	for _, l := range loads {
		byID[l.CTID] = l
	}
	if byID[120].Name != "java" {
		t.Errorf("120 name via comm: got %q", byID[120].Name)
	}
	if byID[120].IOPressure != 30.0 {
		t.Errorf("120 io.pressure: got %v want 30.0", byID[120].IOPressure)
	}
}

// fleetLoads builds a realistic post-compute fleet: 110 heavy writer, 120
// throttled victim, 130 idle — for ranking and verdict tests.
func fleetLoads() []CgroupLoad {
	return []CgroupLoad{
		{CTID: 130, Name: "idle", CoresUsed: 0.01},
		{CTID: 120, Name: "java", CoresUsed: 0.8, IOPressure: 30.0, ThrottledFrac: 0.4, CPUPressure: 8.0},
		{CTID: 110, Name: "torrentbox", CoresUsed: 0.5, WriteMBps: 180.0, IOPressure: 2.0},
	}
}

func TestRank_HottestFirst(t *testing.T) {
	ranked := Rank(fleetLoads())
	// 110 (180 MB/s write) and 120 (heavy pressure+throttle) must outrank idle 130.
	if ranked[len(ranked)-1].CTID != 130 {
		t.Fatalf("idle 130 should rank last, order=%v", ctids(ranked))
	}
	// Rank must not mutate the caller's slice.
	in := fleetLoads()
	_ = Rank(in)
	if in[0].CTID != 130 {
		t.Fatalf("Rank mutated caller slice: %v", ctids(in))
	}
}

func TestVerdict_NamesWriterAsCulprit(t *testing.T) {
	v := Verdict(fleetLoads())
	if !strings.Contains(v, "torrentbox") || !strings.Contains(v, "CT 110") {
		t.Fatalf("verdict should name writer 110/torrentbox: %q", v)
	}
	if !strings.Contains(v, "java") || !strings.Contains(v, "CT 120") {
		t.Fatalf("verdict should name victim 120/java: %q", v)
	}
}

func TestVerdict_NoCulpritWhenNoVictim(t *testing.T) {
	// Lone heavy writer, nobody suffering → no verdict (don't cry wolf).
	loads := []CgroupLoad{
		{CTID: 110, Name: "torrentbox", WriteMBps: 200.0},
		{CTID: 130, Name: "idle", CoresUsed: 0.01},
	}
	if v := Verdict(loads); v != "" {
		t.Fatalf("expected no verdict without a victim, got %q", v)
	}
}

func TestVerdict_NoCulpritWhenNoWriter(t *testing.T) {
	// Victim is pressured but no container clears the writer threshold.
	loads := []CgroupLoad{
		{CTID: 120, Name: "java", IOPressure: 40.0, ThrottledFrac: 0.5},
		{CTID: 110, Name: "light", WriteMBps: 1.0},
	}
	if v := Verdict(loads); v != "" {
		t.Fatalf("expected no verdict without a heavy writer, got %q", v)
	}
}

func TestReport_RendersTableAndVerdict(t *testing.T) {
	r := Report(fleetLoads())
	for _, want := range []string{"CTID", "torrentbox", "java", "likely culprit"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

func ctids(loads []CgroupLoad) []int {
	out := make([]int, len(loads))
	for i, l := range loads {
		out[i] = l.CTID
	}
	return out
}

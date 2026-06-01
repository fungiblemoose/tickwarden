package thermal

import (
	"os"
	"path/filepath"
	"testing"
)

// writeZone creates /sys/class/thermal/thermal_zoneN/{type,temp} under root.
func writeZone(t *testing.T, root string, n int, typ string, milliC int) {
	t.Helper()
	d := filepath.Join(root, "class", "thermal", zoneName(n))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(d, "type"), typ+"\n")
	mustWrite(t, filepath.Join(d, "temp"), itoa(milliC)+"\n")
}

func zoneName(n int) string { return "thermal_zone" + itoa(n) }

// writePolicy creates a cpufreq policy with the three freq files (kHz).
func writePolicy(t *testing.T, root string, n, cur, cpuinfoMax, scalMax int) {
	t.Helper()
	d := filepath.Join(root, "devices", "system", "cpu", "cpufreq", "policy"+itoa(n))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(d, "scaling_cur_freq"), itoa(cur)+"\n")
	mustWrite(t, filepath.Join(d, "cpuinfo_max_freq"), itoa(cpuinfoMax)+"\n")
	mustWrite(t, filepath.Join(d, "scaling_max_freq"), itoa(scalMax)+"\n")
}

func writeHwmon(t *testing.T, root string, hw, idx int, name string, milliC int) {
	t.Helper()
	d := filepath.Join(root, "class", "hwmon", "hwmon"+itoa(hw))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(d, "name"), name+"\n")
	mustWrite(t, filepath.Join(d, "temp"+itoa(idx)+"_input"), itoa(milliC)+"\n")
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// TestHotZoneThrottles: a hot zone at/over the trip point with a clearly capped
// clock must report Throttling=true.
func TestHotZoneThrottles(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "x86_pkg_temp", 86000)       // 86°C, over TripC
	writePolicy(t, root, 0, 1900000, 3100000, 3100000) // 1.9/3.1 GHz = ~61%
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.Throttling {
		t.Errorf("expected Throttling=true, got false (MaxC=%.0f pct=%.0f)", got.MaxC, got.FreqCappedPct)
	}
	if got.MaxC != 86 {
		t.Errorf("MaxC = %.1f, want 86", got.MaxC)
	}
	if got.FreqCappedPct < 60 || got.FreqCappedPct > 62 {
		t.Errorf("FreqCappedPct = %.1f, want ~61", got.FreqCappedPct)
	}
	if got.Verdict() == "" || got.Verdict() == "thermal headroom OK" {
		t.Errorf("expected throttling verdict, got %q", got.Verdict())
	}
}

// TestWarmCappedThrottles: below TripC but warm (≥WarmC) AND visibly clocked
// down — clause 2 must fire so the frequency signal earns its place.
func TestWarmCappedThrottles(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "soc_thermal", 82000)        // 82°C: warm, under trip
	writePolicy(t, root, 0, 1000000, 2400000, 2400000) // ~42% of max
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Throttling {
		t.Errorf("warm+capped should throttle; MaxC=%.0f pct=%.0f", got.MaxC, got.FreqCappedPct)
	}
}

// TestCoolFullFreqNotThrottling: cool chip at full clock → not throttling.
func TestCoolFullFreqNotThrottling(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "x86_pkg_temp", 45000)       // 45°C
	writePolicy(t, root, 0, 3100000, 3100000, 3100000) // 100%
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got.Throttling {
		t.Errorf("cool+full-freq should NOT throttle; verdict=%q", got.Verdict())
	}
	if got.FreqCappedPct != 100 {
		t.Errorf("FreqCappedPct = %.1f, want 100", got.FreqCappedPct)
	}
}

// TestCoolIdleLowClockNotThrottling: low clock at a COOL temp is normal idle
// power-scaling, never throttling — guards against the idle false-positive.
func TestCoolIdleLowClockNotThrottling(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "x86_pkg_temp", 40000)      // 40°C, cold
	writePolicy(t, root, 0, 800000, 3100000, 3100000) // 26% clock but cold
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got.Throttling {
		t.Errorf("cool idle low-clock must NOT be throttling; verdict=%q", got.Verdict())
	}
}

// TestMultiZoneMax: MaxC must be the hottest of several zones.
func TestMultiZoneMax(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "acpitz", 55000)
	writeZone(t, root, 1, "x86_pkg_temp", 88000)
	writeZone(t, root, 2, "nvme", 47000)
	writePolicy(t, root, 0, 3000000, 3100000, 3100000)
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxC != 88 {
		t.Errorf("MaxC = %.1f, want 88 (hottest of 3 zones)", got.MaxC)
	}
	if len(got.Zones) != 3 {
		t.Errorf("got %d zones, want 3", len(got.Zones))
	}
}

// TestMultiPolicyMaxCurFreq: cur is the busiest cluster's clock; hwMax is the
// highest cpuinfo_max_freq (big.LITTLE aggregation).
func TestMultiPolicyMaxCurFreq(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "soc_thermal", 50000)
	writePolicy(t, root, 0, 1000000, 1800000, 1800000) // little: 1.0/1.8
	writePolicy(t, root, 4, 2200000, 2400000, 2400000) // big:    2.2/2.4
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got.CurFreqMHz != 2200 {
		t.Errorf("CurFreqMHz = %.0f, want 2200 (busiest cluster)", got.CurFreqMHz)
	}
	if got.MaxFreqMHz != 2400 {
		t.Errorf("MaxFreqMHz = %.0f, want 2400 (highest hardware ceiling)", got.MaxFreqMHz)
	}
}

// TestStaticCapNoted: scaling_max_freq below the hardware ceiling is a static
// cap and must be surfaced in Notes even when cool.
func TestStaticCapNoted(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "x86_pkg_temp", 50000)
	writePolicy(t, root, 0, 1400000, 3100000, 1500000) // scaling_max < cpuinfo_max
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	foundCap := false
	for _, n := range got.Notes {
		if contains(n, "frequency limit is set") {
			foundCap = true
		}
	}
	if !foundCap {
		t.Errorf("expected a static-cap note, got Notes=%v", got.Notes)
	}
}

// TestMissingFreqTolerated: a VM with thermal zones but no cpufreq policies must
// still Read successfully, noting the absence rather than erroring.
func TestMissingFreqTolerated(t *testing.T) {
	root := t.TempDir()
	writeZone(t, root, 0, "acpitz", 60000) // zones present, no cpufreq dir
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatalf("missing cpufreq should be tolerated, got err: %v", err)
	}
	if got.MaxC != 60 {
		t.Errorf("MaxC = %.1f, want 60", got.MaxC)
	}
	if got.FreqCappedPct != 0 {
		t.Errorf("FreqCappedPct should be 0 when no cpufreq, got %.1f", got.FreqCappedPct)
	}
	if !hasNote(got.Notes, "no cpufreq") {
		t.Errorf("expected a cpufreq-absent note, got %v", got.Notes)
	}
}

// TestHwmonFallback: when thermal_zone* is absent, hwmon temp inputs are used.
func TestHwmonFallback(t *testing.T) {
	root := t.TempDir()
	writeHwmon(t, root, 0, 1, "coretemp", 72000) // 72°C via hwmon only
	writePolicy(t, root, 0, 3000000, 3100000, 3100000)
	got, err := Read(Config{SysRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxC != 72 {
		t.Errorf("MaxC = %.1f, want 72 (from hwmon)", got.MaxC)
	}
	if len(got.Zones) != 1 || got.Zones[0].Label != "coretemp" {
		t.Errorf("hwmon zone not picked up: %+v", got.Zones)
	}
	if !hasNote(got.Notes, "hwmon") {
		t.Errorf("expected hwmon-fallback note, got %v", got.Notes)
	}
}

// TestEmptyRootErrors: nothing readable at all → error (the only error case).
func TestEmptyRootErrors(t *testing.T) {
	root := t.TempDir()
	got, err := Read(Config{SysRoot: root})
	if err == nil {
		t.Errorf("expected error on empty sysfs root, got %+v", got)
	}
}

// TestDefaultSysRoot: zero Config defaults to /sys (it won't error on a real
// Linux box, but on this test host it may have no data — just confirm it
// doesn't panic and returns something sane).
func TestDefaultSysRoot(t *testing.T) {
	_, _ = Read(Config{}) // must not panic; result is host-dependent
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if contains(n, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}

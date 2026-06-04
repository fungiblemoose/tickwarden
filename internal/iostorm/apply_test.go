package iostorm

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fungiblemoose/tickwarden/internal/hostagent"
)

// testStorm is a minimal storm whose writer is CT 110, matching the canonical
// incident. SuggestedWbps is the package default unless a test overrides it.
func testStorm() Storm {
	return Storm{
		Writer:        hostagent.CgroupLoad{CTID: 110, Name: "torrentbox"},
		Victim:        hostagent.CgroupLoad{CTID: 120, Name: "java"},
		SuggestedWbps: DefaultSuggestedWbps,
	}
}

// writeCgroupFile creates <root>/<ctid>/<name> with content, making parent dirs.
func writeCgroupFile(t *testing.T, root string, ctid int, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(ctid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestApply_WritesExpectedLine(t *testing.T) {
	root := t.TempDir()
	// io.max must already exist (it's the kernel pseudo-file). Seed it as empty.
	path := writeCgroupFile(t, root, 110, "io.max", "")

	res, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "252:0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	want := "252:0 wbps=" + strconv.FormatInt(DefaultSuggestedWbps, 10)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != want {
		t.Errorf("io.max contents: got %q want %q", got, want)
	}
	if res.Line != want {
		t.Errorf("result Line: got %q want %q", res.Line, want)
	}
	if res.AppliedWbps != DefaultSuggestedWbps {
		t.Errorf("AppliedWbps: got %d want %d", res.AppliedWbps, DefaultSuggestedWbps)
	}
	if res.Floored {
		t.Errorf("default cap should not be floored")
	}
	if res.CgroupPath != path {
		t.Errorf("CgroupPath: got %q want %q", res.CgroupPath, path)
	}
}

func TestApply_UsesExplicitWbps(t *testing.T) {
	root := t.TempDir()
	path := writeCgroupFile(t, root, 110, "io.max", "")

	// 200 MB/s, well above the floor → applied verbatim.
	want := int64(200 * 1000 * 1000)
	res, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "8:0", Wbps: want})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.AppliedWbps != want || res.Floored {
		t.Fatalf("explicit cap: got %d floored=%v want %d floored=false", res.AppliedWbps, res.Floored, want)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "8:0 wbps="+strconv.FormatInt(want, 10) {
		t.Errorf("io.max: got %q", got)
	}
}

func TestApply_FloorEnforced(t *testing.T) {
	root := t.TempDir()
	path := writeCgroupFile(t, root, 110, "io.max", "")

	// Request an absurdly low cap (1 MB/s); must clamp up to the floor.
	res, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "252:0", Wbps: 1_000_000})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.AppliedWbps != DefaultFloorWbps {
		t.Errorf("AppliedWbps: got %d want floor %d", res.AppliedWbps, DefaultFloorWbps)
	}
	if !res.Floored {
		t.Errorf("Floored should be true when clamped")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "wbps="+strconv.FormatInt(DefaultFloorWbps, 10)) {
		t.Errorf("io.max should carry floor value: %q", got)
	}
}

func TestApply_CustomFloor(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, 110, "io.max", "")

	floor := int64(100 * 1000 * 1000)
	res, err := testStorm().Apply(ApplyOptions{
		CgroupRoot: root, Device: "252:0", Wbps: 60_000_000, FloorWbps: floor,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.AppliedWbps != floor || !res.Floored {
		t.Errorf("custom floor: got %d floored=%v want %d floored=true", res.AppliedWbps, res.Floored, floor)
	}
}

func TestApply_DryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	path := writeCgroupFile(t, root, 110, "io.max", "8:0 wbps=999\n")

	res, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "252:0", DryRun: true})
	if err != nil {
		t.Fatalf("Apply dry run: %v", err)
	}
	if !res.DryRun {
		t.Errorf("DryRun flag should be set in result")
	}
	if res.Line != "252:0 wbps="+strconv.FormatInt(DefaultSuggestedWbps, 10) {
		t.Errorf("dry run should still resolve the planned Line, got %q", res.Line)
	}
	if res.PriorIOMax != "8:0 wbps=999" {
		t.Errorf("dry run should capture prior io.max, got %q", res.PriorIOMax)
	}
	// File must be untouched.
	got, _ := os.ReadFile(path)
	if string(got) != "8:0 wbps=999\n" {
		t.Errorf("dry run must not write; io.max changed to %q", got)
	}
}

func TestApply_CapturesPriorForUndo(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, 110, "io.max", "8:0 wbps=12345\n")

	res, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "8:0"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.PriorIOMax != "8:0 wbps=12345" {
		t.Errorf("PriorIOMax: got %q want %q", res.PriorIOMax, "8:0 wbps=12345")
	}
}

func TestUnset_RestoresMax(t *testing.T) {
	root := t.TempDir()
	path := writeCgroupFile(t, root, 110, "io.max", "")

	// First apply a cap, then unset it.
	if _, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "252:0"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	res, err := testStorm().Unset(ApplyOptions{CgroupRoot: root, Device: "252:0"})
	if err != nil {
		t.Fatalf("Unset: %v", err)
	}
	if res.Line != "252:0 wbps=max" {
		t.Errorf("Unset Line: got %q want %q", res.Line, "252:0 wbps=max")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "252:0 wbps=max" {
		t.Errorf("io.max after Unset: got %q want %q", got, "252:0 wbps=max")
	}
}

func TestUnset_DryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	path := writeCgroupFile(t, root, 110, "io.max", "252:0 wbps=150000000")

	res, err := testStorm().Unset(ApplyOptions{CgroupRoot: root, Device: "252:0", DryRun: true})
	if err != nil {
		t.Fatalf("Unset dry run: %v", err)
	}
	if res.Line != "252:0 wbps=max" || res.PriorIOMax != "252:0 wbps=150000000" {
		t.Errorf("unset dry run: line=%q prior=%q", res.Line, res.PriorIOMax)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "252:0 wbps=150000000" {
		t.Errorf("unset dry run must not write, got %q", got)
	}
}

func TestApply_MissingDeviceErrors(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, 110, "io.max", "")
	if _, err := testStorm().Apply(ApplyOptions{CgroupRoot: root}); err == nil {
		t.Fatalf("expected error when device missing")
	}
	if _, err := testStorm().Unset(ApplyOptions{CgroupRoot: root}); err == nil {
		t.Fatalf("expected error when device missing for Unset")
	}
}

func TestApply_MissingPathErrors(t *testing.T) {
	root := t.TempDir() // no cgroup dir for CT 110 at all
	_, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "252:0"})
	if err == nil {
		t.Fatalf("expected error when io.max path is absent")
	}
	if !strings.Contains(err.Error(), "io.max") {
		t.Errorf("error should mention io.max, got %v", err)
	}
}

func TestApply_NonPositiveCapErrors(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, 110, "io.max", "")
	// Storm with no suggestion and no explicit Wbps → resolves to 0 → error.
	s := Storm{Writer: hostagent.CgroupLoad{CTID: 110}}
	if _, err := s.Apply(ApplyOptions{CgroupRoot: root, Device: "252:0"}); err == nil {
		t.Fatalf("expected error for non-positive cap")
	}
}

func TestApply_UnwritablePathErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions; cannot test unwritable path as root")
	}
	root := t.TempDir()
	path := writeCgroupFile(t, root, 110, "io.max", "")
	if err := os.Chmod(path, 0o400); err != nil { // read-only
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: "252:0"})
	if err == nil {
		t.Fatalf("expected error writing to read-only io.max")
	}
}

func TestDiscoverDevice_PicksBusiestWriter(t *testing.T) {
	root := t.TempDir()
	// 8:0 has more wbytes than 252:0 → discovery should pick 8:0.
	iostat := "252:0 rbytes=1000 wbytes=500 rios=1 wios=1\n" +
		"8:0 rbytes=2000 wbytes=9000 rios=2 wios=2\n"
	writeCgroupFile(t, root, 110, "io.stat", iostat)

	dev, err := DiscoverDevice(root, 110)
	if err != nil {
		t.Fatalf("DiscoverDevice: %v", err)
	}
	if dev != "8:0" {
		t.Errorf("device: got %q want %q", dev, "8:0")
	}
}

func TestDiscoverDevice_MissingFileErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := DiscoverDevice(root, 110); err == nil {
		t.Fatalf("expected error when io.stat absent")
	}
}

func TestDiscoverDevice_NoWriteTrafficErrors(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, 110, "io.stat", "252:0 rbytes=1000 wbytes=0 rios=1 wios=0\n")
	if _, err := DiscoverDevice(root, 110); err == nil {
		t.Fatalf("expected error when no device has write traffic")
	}
}

// Integration: discover the device from io.stat, then apply a cap keyed on it.
func TestApply_WithDiscoveredDevice(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, 110, "io.stat", "8:0 rbytes=10 wbytes=999999\n")
	path := writeCgroupFile(t, root, 110, "io.max", "")

	dev, err := DiscoverDevice(root, 110)
	if err != nil {
		t.Fatalf("DiscoverDevice: %v", err)
	}
	if _, err := testStorm().Apply(ApplyOptions{CgroupRoot: root, Device: dev}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(got), "8:0 wbps=") {
		t.Errorf("io.max should be keyed on discovered device 8:0, got %q", got)
	}
}

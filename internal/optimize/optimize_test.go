package optimize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fungiblemoose/tickwarden/internal/detect"
	"github.com/fungiblemoose/tickwarden/internal/observe"
	"github.com/fungiblemoose/tickwarden/internal/tune"
)

// fixtureResult builds a Result by hand so Report() can be tested deterministic-
// ally without touching the real host. ModSet's state is unexported, so the only
// honest way to get a populated one is detect.ScanMods over seeded fake jars.
func fixtureResult(t *testing.T) Result {
	t.Helper()

	dir := t.TempDir()
	for _, jar := range []string{"lithium-0.11.jar", "c2me-0.2.jar", "starlight-1.1.jar", "scalablelux-0.1.jar"} {
		if err := os.WriteFile(filepath.Join(dir, jar), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed jar: %v", err)
		}
	}
	mods, err := detect.ScanMods(dir)
	if err != nil {
		t.Fatalf("ScanMods: %v", err)
	}

	ssd := false // rotational=false => SSD
	prof := detect.Profile{
		OS:                "linux",
		Virt:              detect.VirtLXC,
		EffectiveCores:    3,
		MemoryBudgetBytes: 8 << 30,
		StorageRotational: &ssd,
	}
	plan := tune.Recommend(prof, tune.Options{Players: 4, PerfMods: true})

	return Result{
		Profile:     prof,
		Mods:        mods,
		ModAdvice:   detect.Advise(mods), // starlight+scalablelux => a real conflict line
		Plan:        plan,
		Pressure:    observe.Pressure{Available: true, CPU: observe.PSI{SomeAvg10: 42.0}},
		Players:     4,
		modsScanned: true,
	}
}

func TestReportSections(t *testing.T) {
	out := fixtureResult(t).Report()

	// 1. system summary line.
	if !strings.Contains(out, "tickwarden optimize — lxc") {
		t.Errorf("missing system summary line:\n%s", out)
	}

	// 2. server.properties section with an actual properties rec (sim distance is
	// always emitted as TargetProperties).
	if !strings.Contains(out, "server.properties changes") {
		t.Error("missing server.properties section header")
	}
	if !strings.Contains(out, "simulation-distance =") {
		t.Errorf("server.properties section missing the simulation-distance rec:\n%s", out)
	}

	// 3. by-hand section with a JVM rec (Xmx) and a mod-config rec (c2me.threads).
	if !strings.Contains(out, "Apply by hand") {
		t.Error("missing by-hand section header")
	}
	if !strings.Contains(out, "Xmx =") || !strings.Contains(out, "c2me.threads =") {
		t.Errorf("by-hand section missing JVM/mod-config recs:\n%s", out)
	}

	// 4. mod advice — the two-light-engine conflict must surface.
	if !strings.Contains(out, "conflict:") || !strings.Contains(out, "two lighting engines") {
		t.Errorf("mod advice conflict not reported:\n%s", out)
	}
	if !strings.Contains(out, "detected:") {
		t.Error("missing detected mods line")
	}

	// 5. below-the-JVM pointer + current-pressure callout (CPU at 42% > threshold).
	if !strings.Contains(out, "ON the Proxmox host") {
		t.Errorf("missing below-the-JVM host pointer:\n%s", out)
	}
	if !strings.Contains(out, "CPU pressure is 42.0%") {
		t.Errorf("expected high-CPU-pressure callout:\n%s", out)
	}
}

// TestReportNoModsScan checks the "never looked" path reads differently from a
// scan that found nothing — the operator must know sizing assumed perf mods.
func TestReportNoModsScan(t *testing.T) {
	prof := detect.Profile{Virt: detect.VirtNone, EffectiveCores: 4, MemoryBudgetBytes: 4 << 30}
	r := Result{
		Profile:     prof,
		Plan:        tune.Recommend(prof, tune.DefaultOptions()),
		Players:     4,
		modsScanned: false,
	}
	out := r.Report()
	if !strings.Contains(out, "no mods dir scanned") {
		t.Errorf("expected the not-scanned notice:\n%s", out)
	}
	if !strings.Contains(out, "assumed chunk-perf mods") {
		t.Errorf("expected the assumed-perf-mods caveat:\n%s", out)
	}
}

// TestReportLowPressureNoCallout ensures a quiet box doesn't get a false alarm.
func TestReportLowPressureNoCallout(t *testing.T) {
	r := fixtureResult(t)
	r.Pressure = observe.Pressure{Available: true, CPU: observe.PSI{SomeAvg10: 1.0}, IO: observe.PSI{SomeAvg10: 0.0}}
	out := r.Report()
	if strings.Contains(out, "right now:") {
		t.Errorf("low pressure should not trigger a callout:\n%s", out)
	}
}

// TestAnalyzeScansMods exercises the scan->advise->recommend flow end to end via
// a seeded mods dir. Detect()/ReadPressure() read the real host (this runs on a
// dev box), so we assert only host-independent invariants.
func TestAnalyzeScansMods(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c2me-0.2.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(Options{ModsDir: dir, Players: 6})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !r.modsScanned {
		t.Error("expected modsScanned=true after scanning a dir")
	}
	if !r.Mods.Has("c2me") {
		t.Error("expected c2me detected from seeded jar")
	}
	if r.Players != 6 {
		t.Errorf("Players = %d, want 6", r.Players)
	}
	if len(r.Plan.Recs) == 0 {
		t.Error("expected a non-empty plan")
	}
	if r.PlayersMeasured {
		t.Error("PlayersMeasured should be false with no PlayersURL")
	}
}

// TestAnalyzeDefaultPlayers verifies the 0 => DefaultOptions substitution (NOT
// Recommend's clamp-to-1): an unset player count must size for the friends-
// server default, not a single player.
func TestAnalyzeDefaultPlayers(t *testing.T) {
	r, err := Analyze(Options{}) // Players 0, no dir, no url
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if want := tune.DefaultOptions().Players; r.Players != want {
		t.Errorf("Players = %d, want default %d", r.Players, want)
	}
}

// TestAnalyzeBadModsDir confirms a genuine read failure is propagated (a dir
// that exists but isn't readable). A non-existent dir is treated as "no mods" by
// ScanMods, so we make an unreadable real dir instead.
func TestAnalyzeBadModsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "noperm")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("dir is readable (likely running as root); can't exercise the error path")
	}
	if _, err := Analyze(Options{ModsDir: dir}); err == nil {
		t.Error("expected an error from an unreadable mods dir")
	}
}

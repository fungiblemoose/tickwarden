package apply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fungiblemoose/tickwarden/internal/tune"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func samplePlan() tune.Plan {
	return tune.Plan{Recs: []tune.Rec{
		{Key: "view-distance", Value: "12", Confidence: tune.Heuristic, Target: tune.TargetProperties},
		{Key: "simulation-distance", Value: "8", Confidence: tune.Solid, Target: tune.TargetProperties},
		{Key: "Xmx", Value: "6G", Target: tune.TargetJVM}, // must be ignored by apply
		{Key: "lighting-engine", Value: "ScalableLux", Target: tune.TargetManual},
	}}
}

func TestPlanProperties_Classifies(t *testing.T) {
	dir := t.TempDir()
	// view-distance differs (10→12), simulation-distance already matches (8).
	props := writeFile(t, dir, "server.properties", "# comment\nview-distance=10\nsimulation-distance=8\nmotd=hi\n")
	locks := map[string]bool{}

	plan, err := PlanProperties(props, samplePlan(), locks)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Key != "view-distance" || plan.Changes[0].New != "12" {
		t.Fatalf("expected one change to view-distance, got %+v", plan.Changes)
	}
	if len(plan.Unchanged) != 1 || plan.Unchanged[0].Key != "simulation-distance" {
		t.Fatalf("expected simulation-distance unchanged, got %+v", plan.Unchanged)
	}
	// JVM and manual targets must never appear as property changes.
	for _, c := range append(plan.Changes, plan.Unchanged...) {
		if c.Key == "Xmx" || c.Key == "lighting-engine" {
			t.Fatalf("non-properties target leaked into plan: %s", c.Key)
		}
	}
}

func TestPlanProperties_RespectsLock(t *testing.T) {
	dir := t.TempDir()
	props := writeFile(t, dir, "server.properties", "view-distance=10\nsimulation-distance=8\n")
	locks := map[string]bool{"view-distance": true}

	plan, err := PlanProperties(props, samplePlan(), locks)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("locked key should not be in Changes, got %+v", plan.Changes)
	}
	if len(plan.Locked) != 1 || plan.Locked[0].Key != "view-distance" {
		t.Fatalf("expected view-distance in Locked, got %+v", plan.Locked)
	}
}

func TestWrite_EditsInPlaceAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	original := "# my server\nview-distance=10\nsimulation-distance=8\nmotd=hi\n"
	props := writeFile(t, dir, "server.properties", original)

	plan, err := PlanProperties(props, samplePlan(), nil)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := plan.Write()
	if err != nil {
		t.Fatal(err)
	}

	// Backup must hold the original verbatim.
	if b, _ := os.ReadFile(backup); string(b) != original {
		t.Fatalf("backup mismatch:\n%s", b)
	}
	got, _ := os.ReadFile(props)
	gs := string(got)
	if !strings.Contains(gs, "view-distance=12") {
		t.Fatalf("view-distance not updated:\n%s", gs)
	}
	// Comment, untouched key, and order preserved.
	if !strings.HasPrefix(gs, "# my server\n") || !strings.Contains(gs, "motd=hi") {
		t.Fatalf("formatting not preserved:\n%s", gs)
	}
	if strings.Count(gs, "view-distance=") != 1 {
		t.Fatalf("duplicate key written:\n%s", gs)
	}
}

func TestWrite_AppendsNewKey(t *testing.T) {
	dir := t.TempDir()
	// view-distance absent → should be appended.
	props := writeFile(t, dir, "server.properties", "simulation-distance=8\n")
	plan, err := PlanProperties(props, samplePlan(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(props)
	if !strings.Contains(string(got), "view-distance=12") {
		t.Fatalf("new key not appended:\n%s", got)
	}
}

func TestRevert(t *testing.T) {
	dir := t.TempDir()
	original := "view-distance=10\nsimulation-distance=8\n"
	props := writeFile(t, dir, "server.properties", original)
	plan, _ := PlanProperties(props, samplePlan(), nil)
	if _, err := plan.Write(); err != nil {
		t.Fatal(err)
	}
	if err := Revert(props); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(props); string(b) != original {
		t.Fatalf("revert did not restore original:\n%s", b)
	}
}

func TestLoadLocks(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "tickwarden.toml", "# locks\n[lock]\nview-distance = true\nsimulation-distance = false\n[other]\nsim = true\n")
	locks, err := LoadLocks(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !locks["view-distance"] {
		t.Fatal("view-distance should be locked")
	}
	if locks["simulation-distance"] {
		t.Fatal("simulation-distance=false should not lock")
	}
	if locks["sim"] {
		t.Fatal("key outside [lock] section must not lock")
	}
}

func TestLoadLocks_MissingFileIsEmpty(t *testing.T) {
	locks, err := LoadLocks(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("expected no locks, got %v", locks)
	}
}

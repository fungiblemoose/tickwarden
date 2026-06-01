package detect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeFiles drops empty files with the given names into dir.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
}

func TestScanModsDetection(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"c2me-0.3.4.jar",            // chunk perf mod, mixed case token survives lowercasing
		"ScalableLux-1.2.jar",       // chunk perf mod, capitalized
		"lithium-fabric-0.16.3.jar", // recognized, not a chunk perf mod
		"FerriteCore-7.0.0.jar",     // recognized, case-insensitive
		"unknownmod-1.0.jar",        // jar but not in the catalog
		"notes.txt",                 // not a jar
	)

	set, err := ScanMods(dir)
	if err != nil {
		t.Fatalf("ScanMods: %v", err)
	}

	want := []string{"c2me", "ferritecore", "lithium", "scalablelux"}
	if got := set.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if !set.Has("c2me") || !set.Has("lithium") {
		t.Fatal("expected c2me and lithium to be detected")
	}
	if set.Has("krypton") {
		t.Fatal("krypton should not be detected")
	}
	if set.Len() != 4 {
		t.Fatalf("Len() = %d, want 4", set.Len())
	}
}

func TestScanModsHasChunkPerfMods(t *testing.T) {
	// A set with c2me must report chunk perf mods.
	dir := t.TempDir()
	writeFiles(t, dir, "c2me-0.3.4.jar", "spark-1.10.jar")
	set, err := ScanMods(dir)
	if err != nil {
		t.Fatalf("ScanMods: %v", err)
	}
	if !set.HasChunkPerfMods() {
		t.Fatal("c2me present: HasChunkPerfMods should be true")
	}

	// A set with only non-chunk mods must report false.
	dir2 := t.TempDir()
	writeFiles(t, dir2, "lithium-0.16.jar", "krypton-0.2.jar", "chunky-1.4.jar")
	set2, err := ScanMods(dir2)
	if err != nil {
		t.Fatalf("ScanMods: %v", err)
	}
	if set2.HasChunkPerfMods() {
		t.Fatal("no c2me/scalablelux/starlight present: HasChunkPerfMods should be false")
	}
}

func TestScanModsMissingDir(t *testing.T) {
	// A path that does not exist is tolerated as "no mods".
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	set, err := ScanMods(missing)
	if err != nil {
		t.Fatalf("missing dir should be nil error, got %v", err)
	}
	if set.Len() != 0 || set.HasChunkPerfMods() {
		t.Fatal("missing dir should yield an empty ModSet")
	}

	// An empty existing dir is likewise empty.
	set2, err := ScanMods(t.TempDir())
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if set2.Len() != 0 {
		t.Fatal("empty dir should yield an empty ModSet")
	}

	// An empty path string is tolerated too.
	set3, err := ScanMods("")
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if set3.Len() != 0 {
		t.Fatal("empty path should yield an empty ModSet")
	}
}

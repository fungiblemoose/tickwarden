package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTemp writes contents to a temp tickwarden.toml and returns its path.
func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tickwarden.toml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadFullConfig(t *testing.T) {
	const toml = `
# tickwarden config
companion_url = "http://127.0.0.1:9225"
players = 12
target_mspt = 45.5
min_sim = 6
max_sim = 16
clustered = true
settled = false
mods_dir = '/srv/mc/mods'

[lock]
view-distance = true
simulation-distance = true
not-locked = false
`
	cfg, err := Load(writeTemp(t, toml))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}

	if cfg.CompanionURL != "http://127.0.0.1:9225" {
		t.Errorf("CompanionURL = %q", cfg.CompanionURL)
	}
	if cfg.Players != 12 {
		t.Errorf("Players = %d, want 12", cfg.Players)
	}
	if cfg.TargetMSPT != 45.5 {
		t.Errorf("TargetMSPT = %v, want 45.5", cfg.TargetMSPT)
	}
	if cfg.MinSim != 6 || cfg.MaxSim != 16 {
		t.Errorf("MinSim/MaxSim = %d/%d, want 6/16", cfg.MinSim, cfg.MaxSim)
	}
	if !cfg.Clustered {
		t.Errorf("Clustered = false, want true")
	}
	if cfg.Settled {
		t.Errorf("Settled = true, want false")
	}
	if cfg.ModsDir != "/srv/mc/mods" {
		t.Errorf("ModsDir = %q", cfg.ModsDir)
	}

	// Every recognized key (including settled=false) should be marked set.
	for _, k := range []string{
		"companion_url", "players", "target_mspt", "min_sim", "max_sim",
		"clustered", "settled", "mods_dir",
	} {
		if !cfg.IsSet(k) {
			t.Errorf("expected key %q to be marked set", k)
		}
	}

	// [lock] captured with the value==true semantics.
	if !cfg.Lock["view-distance"] || !cfg.Lock["simulation-distance"] {
		t.Errorf("expected locks captured, got %v", cfg.Lock)
	}
	if cfg.Lock["not-locked"] {
		t.Errorf("not-locked should not be locked (value was false)")
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if len(cfg.Set) != 0 {
		t.Errorf("expected no keys set, got %v", cfg.Set)
	}
	if len(cfg.Lock) != 0 {
		t.Errorf("expected no locks, got %v", cfg.Lock)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", cfg.Warnings)
	}
	// Zero-value fields.
	if cfg.Players != 0 || cfg.CompanionURL != "" {
		t.Errorf("expected zero Config, got %+v", cfg)
	}
}

func TestLoadBadValueSkippedAndWarned(t *testing.T) {
	const toml = `
players = not-a-number
target_mspt = also-bad
clustered = maybe
min_sim = 8
`
	cfg, err := Load(writeTemp(t, toml))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	// The good key still lands.
	if cfg.MinSim != 8 || !cfg.IsSet("min_sim") {
		t.Errorf("min_sim should be set to 8, got %d set=%v", cfg.MinSim, cfg.IsSet("min_sim"))
	}

	// Bad keys: field left zero, NOT marked set.
	for _, k := range []string{"players", "target_mspt", "clustered"} {
		if cfg.IsSet(k) {
			t.Errorf("bad key %q should not be marked set", k)
		}
	}
	if cfg.Players != 0 || cfg.TargetMSPT != 0 || cfg.Clustered {
		t.Errorf("bad values should leave fields zero, got %+v", cfg)
	}

	// One warning per bad value.
	if len(cfg.Warnings) != 3 {
		t.Errorf("expected 3 warnings, got %d: %v", len(cfg.Warnings), cfg.Warnings)
	}
}

func TestLoadUnknownKeyWarned(t *testing.T) {
	const toml = `
companion_url = "http://x"
wat = 5
also_unknown = "hi"
`
	cfg, err := Load(writeTemp(t, toml))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if !cfg.IsSet("companion_url") {
		t.Errorf("known key should still be set")
	}
	if cfg.IsSet("wat") || cfg.IsSet("also_unknown") {
		t.Errorf("unknown keys should never be marked set")
	}
	if len(cfg.Warnings) != 2 {
		t.Errorf("expected 2 unknown-key warnings, got %d: %v", len(cfg.Warnings), cfg.Warnings)
	}
}

func TestLoadIgnoresUnrelatedSections(t *testing.T) {
	const toml = `
players = 4

[lock]
view-distance = true

[other]
players = 999
target_mspt = nonsense
`
	cfg, err := Load(writeTemp(t, toml))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	// Top-level players wins; [other].players is ignored, not re-applied.
	if cfg.Players != 4 {
		t.Errorf("Players = %d, want 4 (other section must not override)", cfg.Players)
	}
	// Keys inside an unrelated section must not produce warnings.
	if len(cfg.Warnings) != 0 {
		t.Errorf("unrelated-section keys should not warn, got %v", cfg.Warnings)
	}
	if !cfg.Lock["view-distance"] {
		t.Errorf("lock before the unrelated section should still be captured")
	}
}

func TestUnquote(t *testing.T) {
	cases := map[string]string{
		`"hello"`:    "hello",
		`'hello'`:    "hello",
		`hello`:      "hello",
		`"`:          `"`, // too short to be a pair
		`"mismatch'`: `"mismatch'`,
		`""`:         "",
	}
	for in, want := range cases {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

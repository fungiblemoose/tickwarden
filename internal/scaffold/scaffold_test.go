package scaffold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fungiblemoose/tickwarden/internal/detect"
	"github.com/fungiblemoose/tickwarden/internal/tune"
)

// fakeFetcher serves canned JSON by URL substring and records downloads.
type fakeFetcher struct {
	json      map[string]string // URL substring -> body
	downloads []string
	failURLs  map[string]bool
}

func (f *fakeFetcher) GetJSON(u string, v any) error {
	for sub, body := range f.json {
		if strings.Contains(u, sub) {
			return json.Unmarshal([]byte(body), v)
		}
	}
	return fmt.Errorf("no canned response for %s", u)
}

func (f *fakeFetcher) Download(u, dest string) error {
	if f.failURLs[u] {
		return fmt.Errorf("simulated failure")
	}
	f.downloads = append(f.downloads, dest)
	return os.WriteFile(dest, []byte("jar-bytes"), 0o644)
}

func metaFetcher() *fakeFetcher {
	return &fakeFetcher{json: map[string]string{
		"/versions/loader/1.21.5": `[{"loader":{"version":"0.19.3","stable":true}}]`,
		"/versions/installer":     `[{"version":"1.1.1","stable":true}]`,
		"project/lithium/":        `[{"files":[{"url":"https://cdn/lithium.jar","filename":"lithium-mc1.21.5.jar","primary":true}]}]`,
		"project/ferrite-core/":   `[{"files":[{"url":"https://cdn/fc.jar","filename":"ferritecore.jar","primary":false}]}]`,
		"project/krypton/":        `[]`, // no build for this MC version
		"project/c2me-fabric/":    `[{"files":[{"url":"https://cdn/c2me.jar","filename":"c2me.jar","primary":true}]}]`,
		"project/scalablelux/":    `[{"files":[{"url":"https://cdn/slux.jar","filename":"scalablelux.jar","primary":true}]}]`,
		"project/spark/":          `[{"files":[{"url":"https://cdn/spark.jar","filename":"spark.jar","primary":true}]}]`,
	}}
}

func TestResolve(t *testing.T) {
	m, err := Resolve("1.21.5", metaFetcher())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.LoaderVersion != "0.19.3" || m.InstallerVersion != "1.1.1" {
		t.Fatalf("bad loader/installer: %+v", m)
	}
	if !strings.Contains(m.ServerJar.URL, "/versions/loader/1.21.5/0.19.3/1.1.1/server/jar") {
		t.Fatalf("server jar URL should be the meta triple: %s", m.ServerJar.URL)
	}
	if len(m.Mods) != 5 {
		t.Fatalf("want 5 resolvable mods (krypton has no build), got %d: %+v", len(m.Mods), m.Mods)
	}
	// A mod without a build for this version is a note, not a failure.
	if len(m.Missing) != 1 || !strings.Contains(m.Missing[0], "krypton") {
		t.Fatalf("krypton should be reported missing: %v", m.Missing)
	}
	// Mods land under mods/, never elsewhere (filenames come from the network —
	// never let one path-traverse out of the server dir).
	for _, mod := range m.Mods {
		if !strings.HasPrefix(mod.File, "mods"+string(filepath.Separator)) {
			t.Fatalf("mod file should live under mods/: %s", mod.File)
		}
	}
}

func TestResolve_UnknownVersionFails(t *testing.T) {
	f := metaFetcher()
	f.json["/versions/loader/9.99.9"] = `[]`
	if _, err := Resolve("9.99.9", f); err == nil || !strings.Contains(err.Error(), "no loader") {
		t.Fatalf("unknown MC version should fail loudly, got %v", err)
	}
}

// testPlan builds a tune plan from a synthetic profile so the rendered files
// are deterministic regardless of the machine running the tests.
func testPlan(players int) tune.Plan {
	ssd := false
	p := detect.Profile{
		EffectiveCores:    4,
		MemoryBudgetBytes: 8 << 30,
		StorageRotational: &ssd,
	}
	opts := tune.DefaultOptions()
	opts.Players = players
	return tune.Recommend(p, opts)
}

func TestRenderFiles(t *testing.T) {
	opts := Options{Dir: "/srv/mc", MCVersion: "1.21.5", Players: 4}
	files := RenderFiles(opts, testPlan(4))

	byPath := map[string]File{}
	for _, f := range files {
		byPath[f.Path] = f
	}

	props := byPath["server.properties"].Content
	if !strings.Contains(props, "view-distance=") || !strings.Contains(props, "simulation-distance=") {
		t.Fatalf("server.properties should carry the tuned distances:\n%s", props)
	}

	// EULA is NOT accepted unless asked.
	if !strings.Contains(byPath["eula.txt"].Content, "eula=false") {
		t.Fatalf("default eula.txt must be eula=false:\n%s", byPath["eula.txt"].Content)
	}
	accepted := RenderFiles(Options{AcceptEULA: true}, testPlan(4))
	for _, f := range accepted {
		if f.Path == "eula.txt" && !strings.Contains(f.Content, "eula=true") {
			t.Fatalf("-accept-eula should write eula=true:\n%s", f.Content)
		}
	}

	start := byPath["start.sh"]
	if start.Mode != 0o755 {
		t.Fatalf("start.sh should be executable, mode %o", start.Mode)
	}
	// 8 GiB budget, 4 players → 4G heap (2 + 0.5/player), G1+Aikar (under 12G).
	if !strings.Contains(start.Content, "-Xmx4G") || !strings.Contains(start.Content, "-Xms4G") {
		t.Fatalf("start.sh should pin the tuned heap:\n%s", start.Content)
	}
	if !strings.Contains(start.Content, "UseG1GC") || strings.Contains(start.Content, "UseZGC") {
		t.Fatalf("4G heap should get G1+Aikar, not ZGC:\n%s", start.Content)
	}

	toml := byPath["tickwarden.toml"].Content
	if !strings.Contains(toml, "players = 4") || !strings.Contains(toml, `mods_dir = "mods"`) {
		t.Fatalf("tickwarden.toml should carry the workload assumptions:\n%s", toml)
	}

	if !strings.Contains(byPath["minecraft.service"].Content, "ExecStart=") {
		t.Fatal("unit file should have an ExecStart")
	}
}

// A big enough box flips the GC recommendation to ZGC and start.sh must follow.
func TestRenderStartShZGC(t *testing.T) {
	ssd := false
	p := detect.Profile{EffectiveCores: 16, MemoryBudgetBytes: 64 << 30, StorageRotational: &ssd}
	opts := tune.DefaultOptions()
	opts.Players = 30 // 2 + 15 = 17G heap >= 12G → ZGC
	plan := tune.Recommend(p, opts)

	sh := renderStartSh(plan)
	if !strings.Contains(sh, "UseZGC") || strings.Contains(sh, "UseG1GC") {
		t.Fatalf("17G heap should get ZGC:\n%s", sh)
	}
}

func TestWriteFilesRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	files := RenderFiles(Options{Dir: dir, Players: 2}, testPlan(2))

	if err := WriteFiles(dir, files); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mods")); err != nil {
		t.Fatal("WriteFiles should create mods/")
	}

	// A second init into the same dir must refuse rather than overwrite the
	// server's (possibly operator-edited) properties.
	err := WriteFiles(dir, files)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("re-init should refuse to clobber, got %v", err)
	}
}

func TestFetchAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := metaFetcher()
	m, err := Resolve("1.21.5", f)
	if err != nil {
		t.Fatal(err)
	}

	// One failing mod degrades to a note; the scaffold still succeeds.
	f.failURLs = map[string]bool{"https://cdn/spark.jar": true}
	failed, err := FetchAll(m, dir, f, nil)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "spark") {
		t.Fatalf("spark download failure should be reported: %v", failed)
	}
	if _, err := os.Stat(filepath.Join(dir, "server.jar")); err != nil {
		t.Fatal("server.jar should be downloaded")
	}
	if _, err := os.Stat(filepath.Join(dir, "mods", "lithium-mc1.21.5.jar")); err != nil {
		t.Fatal("lithium jar should land in mods/")
	}

	// A failed SERVER jar is fatal — nothing works without it.
	f.failURLs[m.ServerJar.URL] = true
	if _, err := FetchAll(m, dir, f, nil); err == nil {
		t.Fatal("server jar failure must be fatal")
	}
}

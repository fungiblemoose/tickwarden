// Package scaffold turns `tickwarden tune`'s knowledge into a brand-new server:
// `tickwarden init` provisions a Fabric server that is tuned to the hardware
// from its very first boot, instead of detecting an existing setup and listing
// what's wrong with it.
//
// Everything the scaffold decides comes from machinery that already exists —
// the decision tree in internal/tune (heap, GC, distances), and the
// recommended-mod stack in internal/detect.Advise. This package adds only the
// plumbing: resolving download URLs (Fabric meta + Modrinth), rendering files,
// and fetching jars.
//
// Scope: scaffold, don't supervise. It writes a start.sh and a systemd unit
// and leaves process lifecycle to systemd/Docker — becoming a process
// supervisor would duplicate what every host already has.
//
// The EULA is never accepted silently: eula.txt is written as eula=false
// unless the operator passes -accept-eula, because agreeing to Mojang's EULA
// is the operator's act, not the tool's.
package scaffold

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/tune"
)

// Options shape the scaffold. Dir is created if missing; everything else
// mirrors the workload flags `tune` takes.
type Options struct {
	Dir        string // target directory for the server
	MCVersion  string // e.g. "1.21.5"
	Players    int    // expected peak, sizes heap and distances
	Clustered  bool   // players congregate (cheaper) vs scatter
	Settled    bool   // survival in pregenerated terrain vs flying
	AcceptEULA bool   // write eula=true; the operator's explicit act
}

// Fetcher is the injected network seam, so resolution and download logic are
// testable with canned JSON and no real Modrinth/Fabric traffic.
type Fetcher interface {
	// GetJSON fetches url and decodes the response body into v.
	GetJSON(url string, v any) error
	// Download streams url into the file at dest (parent must exist).
	Download(url, dest string) error
}

// Download is one resolved artifact: where it comes from and where it lands
// relative to the server directory.
type Download struct {
	Name string `json:"name"` // human name, e.g. "lithium"
	File string `json:"file"` // destination path relative to Dir, e.g. "mods/lithium-....jar"
	URL  string `json:"url"`
}

// Manifest is everything Resolve worked out from the network: the exact
// server jar and mod files to fetch, plus the mods that had no build for this
// Minecraft version (skipped with a note rather than failing the scaffold).
type Manifest struct {
	MCVersion        string     `json:"mc_version"`
	LoaderVersion    string     `json:"loader_version"`
	InstallerVersion string     `json:"installer_version"`
	ServerJar        Download   `json:"server_jar"`
	Mods             []Download `json:"mods"`
	Missing          []string   `json:"missing,omitempty"` // "slug: why"
}

// File is one rendered file: path relative to Dir, full content, and mode.
type File struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// recommendedMods is the baseline perf stack — the same set
// internal/detect.Advise suggests when missing, by their Modrinth slugs:
// tick (lithium), memory (ferrite-core), network (krypton), chunk parallelism
// (c2me-fabric), lighting (scalablelux), profiler (spark). Installing them up
// front is also why the tuner may assume PerfMods=true for this scaffold.
var recommendedMods = []struct{ name, slug string }{
	{"lithium", "lithium"},
	{"ferritecore", "ferrite-core"},
	{"krypton", "krypton"},
	{"c2me", "c2me-fabric"},
	{"scalablelux", "scalablelux"},
	{"spark", "spark"},
}

const (
	fabricMeta  = "https://meta.fabricmc.net/v2"
	modrinthAPI = "https://api.modrinth.com/v2"
)

// Resolve works out the concrete download set for a Minecraft version: the
// Fabric server launcher jar (latest stable loader + installer) and the
// newest build of each recommended mod. A mod with no build for this version
// is recorded in Missing, not an error — the server is still viable without
// any single perf mod, and failing the whole scaffold over one would be worse.
func Resolve(mc string, f Fetcher) (Manifest, error) {
	m := Manifest{MCVersion: mc}

	// Fabric loader for this game version. The meta API lists newest first,
	// with a stable flag; prefer the first stable entry.
	var loaders []struct {
		Loader struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		} `json:"loader"`
	}
	if err := f.GetJSON(fabricMeta+"/versions/loader/"+url.PathEscape(mc), &loaders); err != nil {
		return m, fmt.Errorf("resolve fabric loader: %w", err)
	}
	if len(loaders) == 0 {
		return m, fmt.Errorf("fabric has no loader for Minecraft %s — check the version string", mc)
	}
	m.LoaderVersion = loaders[0].Loader.Version
	for _, l := range loaders {
		if l.Loader.Stable {
			m.LoaderVersion = l.Loader.Version
			break
		}
	}

	var installers []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := f.GetJSON(fabricMeta+"/versions/installer", &installers); err != nil {
		return m, fmt.Errorf("resolve fabric installer: %w", err)
	}
	if len(installers) == 0 {
		return m, fmt.Errorf("fabric meta returned no installers")
	}
	m.InstallerVersion = installers[0].Version
	for _, i := range installers {
		if i.Stable {
			m.InstallerVersion = i.Version
			break
		}
	}

	// The meta server assembles a ready-to-run server launcher jar at a
	// deterministic URL for this (game, loader, installer) triple.
	m.ServerJar = Download{
		Name: "fabric server " + mc,
		File: "server.jar",
		URL:  fmt.Sprintf("%s/versions/loader/%s/%s/%s/server/jar", fabricMeta, url.PathEscape(mc), m.LoaderVersion, m.InstallerVersion),
	}

	// Mods from Modrinth: newest version filtered to this game version and the
	// fabric loader. files[] has a primary flag; fall back to the first file.
	for _, mod := range recommendedMods {
		// Modrinth's filter values are JSON arrays and MUST be URL-encoded —
		// raw brackets/quotes in the query string get a 400.
		params := url.Values{}
		params.Set("game_versions", fmt.Sprintf("[%q]", mc))
		params.Set("loaders", `["fabric"]`)
		q := fmt.Sprintf("%s/project/%s/version?%s", modrinthAPI, mod.slug, params.Encode())
		var versions []struct {
			Files []struct {
				URL      string `json:"url"`
				Filename string `json:"filename"`
				Primary  bool   `json:"primary"`
			} `json:"files"`
		}
		if err := f.GetJSON(q, &versions); err != nil {
			m.Missing = append(m.Missing, fmt.Sprintf("%s: %v", mod.slug, err))
			continue
		}
		if len(versions) == 0 || len(versions[0].Files) == 0 {
			m.Missing = append(m.Missing, fmt.Sprintf("%s: no build published for MC %s yet", mod.slug, mc))
			continue
		}
		file := versions[0].Files[0]
		for _, fl := range versions[0].Files {
			if fl.Primary {
				file = fl
				break
			}
		}
		m.Mods = append(m.Mods, Download{
			Name: mod.name,
			File: filepath.Join("mods", filepath.Base(file.Filename)),
			URL:  file.URL,
		})
	}

	return m, nil
}

// RenderFiles turns the tuning plan into the server's config files. Pure: no
// I/O, fully testable. The plan should come from tune.Recommend with
// PerfMods=true (we install the perf stack) and the same players/clustered/
// settled the operator gave Options.
func RenderFiles(opts Options, plan tune.Plan) []File {
	var files []File

	// server.properties: only the keys the decision tree has an opinion on,
	// plus a header saying where they came from. The server fills in every
	// other key with its defaults on first boot and PRESERVES these.
	var props strings.Builder
	props.WriteString("# Generated by `tickwarden init` — tuned to this machine's hardware.\n")
	props.WriteString("# Each value's reasoning: run `tickwarden tune`. Re-tune after hardware changes.\n")
	for _, r := range plan.Recs {
		if r.Target == tune.TargetProperties {
			fmt.Fprintf(&props, "%s=%s\n", r.Key, r.Value)
		}
	}
	files = append(files, File{Path: "server.properties", Content: props.String(), Mode: 0o644})

	// eula.txt: never accepted on the operator's behalf.
	eula := "# By changing the setting below to true you are indicating your agreement to Minecraft's EULA\n" +
		"# (https://aka.ms/MinecraftEULA).\n"
	if opts.AcceptEULA {
		eula += "eula=true\n"
	} else {
		eula += "# tickwarden init does not accept the EULA for you — re-run with -accept-eula or edit this file.\n" +
			"eula=false\n"
	}
	files = append(files, File{Path: "eula.txt", Content: eula, Mode: 0o644})

	files = append(files, File{Path: "start.sh", Content: renderStartSh(plan), Mode: 0o755})
	files = append(files, File{Path: "tickwarden.toml", Content: renderToml(opts), Mode: 0o644})
	files = append(files, File{Path: "minecraft.service", Content: renderUnit(opts), Mode: 0o644})

	return files
}

// renderStartSh builds the launch script from the plan's JVM recommendations.
// This is the one place tickwarden writes JVM flags: at init time there is no
// existing setup to clobber, so the "report but never write" rule for live
// servers doesn't apply — we own this file because we created it.
func renderStartSh(plan tune.Plan) string {
	heap := ""
	zgc := false
	for _, r := range plan.Recs {
		if r.Target != tune.TargetJVM {
			continue
		}
		switch r.Key {
		case "Xmx":
			heap = r.Value
		case "gc":
			zgc = strings.Contains(r.Value, "ZGC")
		}
	}
	if heap == "" {
		heap = "2G" // no readable memory budget (non-Linux render); safe floor
	}

	var gcFlags string
	if zgc {
		gcFlags = "-XX:+UseZGC -XX:+ZGenerational -XX:+AlwaysPreTouch"
	} else {
		// Aikar's G1 flags — the proven low-pause set for small/medium heaps.
		gcFlags = strings.Join([]string{
			"-XX:+UseG1GC", "-XX:+ParallelRefProcEnabled", "-XX:MaxGCPauseMillis=200",
			"-XX:+UnlockExperimentalVMOptions", "-XX:+DisableExplicitGC", "-XX:+AlwaysPreTouch",
			"-XX:G1NewSizePercent=30", "-XX:G1MaxNewSizePercent=40", "-XX:G1HeapRegionSize=8M",
			"-XX:G1ReservePercent=20", "-XX:G1HeapWastePercent=5", "-XX:G1MixedGCCountTarget=4",
			"-XX:InitiatingHeapOccupancyPercent=15", "-XX:G1MixedGCLiveThresholdPercent=90",
			"-XX:G1RSetUpdatingPauseTimePercent=5", "-XX:SurvivorRatio=32",
			"-XX:+PerfDisableSharedMem", "-XX:MaxTenuringThreshold=1",
		}, " ")
	}

	return fmt.Sprintf(`#!/bin/sh
# Generated by tickwarden init. Heap and GC are sized to this machine —
# run "tickwarden tune" to see the reasoning, and re-run init (or edit here)
# after hardware changes.
cd "$(dirname "$0")"
exec java -Xms%s -Xmx%s %s -jar server.jar nogui
`, heap, heap, gcFlags)
}

// renderToml writes the tickwarden config alongside the server so every later
// command (tune, daemon, optimize) picks up the same workload assumptions the
// scaffold was built with.
func renderToml(opts Options) string {
	var b strings.Builder
	b.WriteString("# tickwarden settings for this server (defaults < this file < flags).\n")
	fmt.Fprintf(&b, "players = %d\n", opts.Players)
	fmt.Fprintf(&b, "clustered = %v\n", opts.Clustered)
	fmt.Fprintf(&b, "settled = %v\n", opts.Settled)
	b.WriteString("mods_dir = \"mods\"\n")
	b.WriteString("companion_url = \"http://127.0.0.1:9225\"\n")
	b.WriteString("\n# Daemon web control plane (uncomment to enable):\n")
	b.WriteString("# ui_addr = \"127.0.0.1:9226\"\n")
	return b.String()
}

// renderUnit writes a systemd unit the operator can install — scaffold, don't
// supervise: lifecycle stays with systemd.
func renderUnit(opts Options) string {
	dir := opts.Dir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return fmt.Sprintf(`# Generated by tickwarden init. Install with:
#   sudo cp minecraft.service /etc/systemd/system/ && sudo systemctl daemon-reload
#   sudo systemctl enable --now minecraft
[Unit]
Description=Minecraft server (scaffolded by tickwarden)
After=network.target

[Service]
WorkingDirectory=%s
ExecStart=%s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
`, dir, filepath.Join(dir, "start.sh"))
}

// WriteFiles materializes rendered files under dir, creating it (and mods/,
// which downloads need) as required. It refuses to overwrite an existing
// server.properties or eula.txt — re-running init on a live server must not
// clobber state the operator may have changed.
func WriteFiles(dir string, files []File) error {
	if err := os.MkdirAll(filepath.Join(dir, "mods"), 0o755); err != nil {
		return err
	}
	for _, f := range files {
		dest := filepath.Join(dir, f.Path)
		if f.Path == "server.properties" || f.Path == "eula.txt" {
			if _, err := os.Stat(dest); err == nil {
				return fmt.Errorf("%s already exists — init scaffolds NEW servers; use `tickwarden tune -apply` to retune an existing one", dest)
			}
		}
		if err := os.WriteFile(dest, []byte(f.Content), f.Mode); err != nil {
			return err
		}
	}
	return nil
}

// FetchAll downloads the manifest's server jar and mods into dir, reporting
// each file through progress (may be nil). One failed mod download degrades to
// a note via the returned slice rather than failing the scaffold; a failed
// SERVER JAR is fatal because nothing works without it.
func FetchAll(m Manifest, dir string, f Fetcher, progress func(string)) (failed []string, err error) {
	if progress == nil {
		progress = func(string) {}
	}
	progress(fmt.Sprintf("downloading %s -> %s", m.ServerJar.Name, m.ServerJar.File))
	if err := f.Download(m.ServerJar.URL, filepath.Join(dir, m.ServerJar.File)); err != nil {
		return nil, fmt.Errorf("server jar: %w", err)
	}
	for _, mod := range m.Mods {
		progress(fmt.Sprintf("downloading %s -> %s", mod.Name, mod.File))
		if err := f.Download(mod.URL, filepath.Join(dir, mod.File)); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", mod.Name, err))
		}
	}
	return failed, nil
}

// HTTPFetcher is the production Fetcher.
type HTTPFetcher struct {
	Client *http.Client
}

// NewHTTPFetcher builds a fetcher with a timeout generous enough for a server
// jar on a slow line but bounded so a wedged CDN can't hang init forever.
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{Client: &http.Client{Timeout: 5 * time.Minute}}
}

func (h *HTTPFetcher) GetJSON(u string, v any) error {
	resp, err := h.Client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (h *HTTPFetcher) Download(u, dest string) error {
	resp, err := h.Client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d", u, resp.StatusCode)
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Write-then-rename so a half-downloaded jar never sits where the server
	// (or a retry) would mistake it for a whole one.
	return os.Rename(tmp, dest)
}

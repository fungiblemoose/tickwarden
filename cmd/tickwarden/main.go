// Command tickwarden is a hardware-aware, host-aware tuning + observability
// tool for self-hosted Minecraft servers.
//
//	tickwarden detect          print the detected host/cgroup profile
//	tickwarden tune            recommend server settings (with reasons)
//	tickwarden watch           correlate TPS dips with host-side starvation
//
// See docs/DECISION_TREE.md for the tuning rules and README.md for the roadmap.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/apply"
	"github.com/fungiblemoose/tickwarden/internal/detect"
	"github.com/fungiblemoose/tickwarden/internal/hostagent"
	"github.com/fungiblemoose/tickwarden/internal/hotspots"
	"github.com/fungiblemoose/tickwarden/internal/iostorm"
	"github.com/fungiblemoose/tickwarden/internal/loadgen"
	"github.com/fungiblemoose/tickwarden/internal/observe"
	"github.com/fungiblemoose/tickwarden/internal/optimize"
	"github.com/fungiblemoose/tickwarden/internal/ostune"
	"github.com/fungiblemoose/tickwarden/internal/pregen"
	"github.com/fungiblemoose/tickwarden/internal/thermal"
	"github.com/fungiblemoose/tickwarden/internal/tune"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "optimize":
		cmdOptimize(os.Args[2:])
	case "detect":
		cmdDetect(os.Args[2:])
	case "tune":
		cmdTune(os.Args[2:])
	case "watch":
		cmdWatch(os.Args[2:])
	case "bench":
		cmdBench(os.Args[2:])
	case "bench-diff":
		cmdBenchDiff(os.Args[2:])
	case "host":
		cmdHost(os.Args[2:])
	case "iostorm":
		cmdIOStorm(os.Args[2:])
	case "loadtest":
		cmdLoadtest(os.Args[2:])
	case "ostune":
		cmdOstune(os.Args[2:])
	case "thermal":
		cmdThermal(os.Args[2:])
	case "hotspots":
		cmdHotspots(os.Args[2:])
	case "pregen":
		cmdPregen(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("tickwarden", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tickwarden — hardware-aware, host-aware Minecraft server tuning

Usage:
  tickwarden optimize [flags]      one-shot: detect + mods + players → the full maxed-but-safe plan
  tickwarden detect [-json]        print the detected host/cgroup profile
  tickwarden tune   [-json]        recommend server settings, with reasons
  tickwarden watch  [flags]        correlate TPS dips with host starvation
  tickwarden bench  [flags]        measure a window: TPS/MSPT + pressure stats
  tickwarden bench-diff A.json B.json   compare two bench runs (before/after a change)
  tickwarden host   [flags]        name the noisy neighbor (run ON the Proxmox host)
  tickwarden iostorm [flags]       detect an I/O storm + advise a throttle (on the host)
  tickwarden ostune [flags]        OS/kernel latency knobs: governor, swappiness, THP, I/O sched
  tickwarden thermal [flags]       CPU thermal / frequency-throttle check
  tickwarden hotspots [flags]      top loaded chunks by block-entity count (lag by location)
  tickwarden pregen [flags]        host-load-aware Chunky pregen (pauses when busy/players on)
  tickwarden loadtest [flags]      drive a reproducible Chunky load while benchmarking
  tickwarden version

Run a command with -h for its flags.
`)
}

func cmdOptimize(args []string) {
	fs := flag.NewFlagSet("optimize", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the full analysis as JSON")
	modsDir := fs.String("mods-dir", "", "scan this server mods/ dir to detect mods + advise")
	players := fs.Int("players", 0, "expected PEAK players (0 => default friends-server sizing)")
	playersURL := fs.String("players-url", "", "companion endpoint for the MEASURED peak (overrides -players)")
	clustered := fs.Bool("clustered", false, "players congregate (shared base/hub) rather than scatter")
	doApply := fs.Bool("apply", false, "apply the server.properties recommendations (dry-run unless -write)")
	write := fs.Bool("write", false, "with -apply, actually write the file (after a .bak backup)")
	props := fs.String("properties", "server.properties", "path to server.properties")
	cfg := fs.String("config", "tickwarden.toml", "path to lock config")
	fs.Parse(args)

	res, err := optimize.Analyze(optimize.Options{
		ModsDir: *modsDir, Players: *players, PlayersURL: *playersURL, Clustered: *clustered,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "optimize failed:", err)
		os.Exit(1)
	}
	if *doApply {
		runApply(res.Plan, *props, *cfg, *write)
		return
	}
	if *asJSON {
		printJSON(res)
		return
	}
	fmt.Print(res.Report())
}

func cmdDetect(args []string) {
	fs := flag.NewFlagSet("detect", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON")
	fs.Parse(args)

	p := detect.Detect()
	if *asJSON {
		printJSON(p)
		return
	}
	fmt.Printf("Host profile\n")
	fmt.Printf("  os:               %s\n", p.OS)
	fmt.Printf("  virt:             %s\n", p.Virt)
	fmt.Printf("  cpu:              %s\n", orNA(p.CPUModel))
	fmt.Printf("  logical cores:    %d\n", p.LogicalCores)
	fmt.Printf("  physical cores:   %d\n", p.PhysicalCores)
	fmt.Printf("  effective cores:  %.2f\n", p.EffectiveCores)
	fmt.Printf("  total ram:        %s\n", giB(p.TotalRAMBytes))
	fmt.Printf("  available ram:    %s\n", giB(p.AvailRAMBytes))
	fmt.Printf("  memory budget:    %s\n", giB(p.MemoryBudgetBytes))
	fmt.Printf("  storage:          %s\n", rotational(p.StorageRotational))
	for _, n := range p.Notes {
		fmt.Printf("  • %s\n", n)
	}
}

func cmdTune(args []string) {
	fs := flag.NewFlagSet("tune", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit JSON")
	doApply := fs.Bool("apply", false, "apply server.properties recommendations (dry-run unless -write)")
	write := fs.Bool("write", false, "with -apply, actually write the file (after backing it up)")
	revert := fs.Bool("revert", false, "restore server.properties from its .bak backup")
	props := fs.String("properties", "server.properties", "path to server.properties")
	cfg := fs.String("config", "tickwarden.toml", "path to lock config (keys you want left alone)")
	players := fs.Int("players", tune.DefaultOptions().Players, "expected PEAK concurrent players to size distances for")
	perfMods := fs.Bool("perf-mods", tune.DefaultOptions().PerfMods, "chunk-system perf mods (C2ME/ScalableLux) installed")
	playersURL := fs.String("players-url", "", "companion endpoint to read the OBSERVED peak player count from (overrides -players)")
	modsDir := fs.String("mods-dir", "", "scan this server mods/ dir to auto-set -perf-mods from installed mods")
	clustered := fs.Bool("clustered", false, "players congregate (shared base/hub) rather than scatter — allows a higher sim distance")
	fs.Parse(args)

	if *revert {
		if err := apply.Revert(*props); err != nil {
			fmt.Fprintln(os.Stderr, "revert failed:", err)
			os.Exit(1)
		}
		fmt.Printf("reverted %s from %s.bak\n", *props, *props)
		return
	}

	opts := tune.Options{Players: *players, PerfMods: *perfMods, Clustered: *clustered}
	if *modsDir != "" {
		mods, err := detect.ScanMods(*modsDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scanning mods dir %s: %v\n", *modsDir, err)
			os.Exit(1)
		}
		opts.PerfMods = mods.HasChunkPerfMods()
		fmt.Fprintf(os.Stderr, "detected mods: %s (chunk-perf mods: %v)\n", strings.Join(mods.Names(), ", "), opts.PerfMods)
		adv := detect.Advise(mods)
		if !adv.Empty() {
			fmt.Fprintln(os.Stderr, "mod advice:")
			for _, c := range adv.Conflicts {
				fmt.Fprintf(os.Stderr, "  ⚠ conflict: %s\n", c)
			}
			for _, r := range adv.Redundant {
				fmt.Fprintf(os.Stderr, "  · redundant: %s\n", r)
			}
			for _, s := range adv.Suggestions {
				fmt.Fprintf(os.Stderr, "  + suggest:  %s\n", s)
			}
			for _, n := range adv.Notes {
				fmt.Fprintf(os.Stderr, "  i note:     %s\n", n)
			}
		}
	}
	if *playersURL != "" {
		if peak, err := observe.FetchPlayerPeak(*playersURL); err != nil {
			fmt.Fprintf(os.Stderr, "couldn't read peak players from %s (%v); falling back to -players %d\n", *playersURL, err, *players)
		} else {
			// Size for at least 1; an idle server still wants headroom for joins.
			if peak < 1 {
				peak = 1
			}
			opts.Players, opts.PlayersAuto = peak, true
		}
	}

	plan := tune.Recommend(detect.Detect(), opts)

	if *doApply {
		runApply(plan, *props, *cfg, *write)
		return
	}

	if *asJSON {
		printJSON(plan)
		return
	}
	fmt.Printf("Recommended tuning for this host (%s, %.1f effective cores, %s budget)\n\n",
		plan.Profile.Virt, plan.Profile.EffectiveCores, giB(plan.Profile.MemoryBudgetBytes))
	for _, r := range plan.Recs {
		fmt.Printf("  [%s] %s = %s\n", r.Confidence, r.Key, r.Value)
		fmt.Printf("        ↳ %s\n", r.Reason)
	}
	fmt.Printf("\nLegend: solid = trust it · heuristic = sane default · contested = validate against your load\n")
	fmt.Printf("Tip: `tune -apply` previews writing the server.properties settings; add -write to commit.\n")
}

func runApply(plan tune.Plan, propsPath, cfgPath string, write bool) {
	locks, err := apply.LoadLocks(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading lock config:", err)
		os.Exit(1)
	}
	ap, err := apply.PlanProperties(propsPath, plan, locks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "can't read %s: %v\n", propsPath, err)
		os.Exit(1)
	}

	fmt.Printf("Apply plan for %s\n\n", propsPath)
	if len(ap.Changes) == 0 {
		fmt.Println("  nothing to change — server.properties already matches the recommendations.")
	}
	for _, c := range ap.Changes {
		old := c.Old
		if !c.Present {
			old = "(absent)"
		}
		fmt.Printf("  [%s] %s: %s → %s\n", c.Confidence, c.Key, old, c.New)
		fmt.Printf("        ↳ %s\n", c.Reason)
	}
	for _, c := range ap.Locked {
		fmt.Printf("  [locked] %s: keeping your %s (tickwarden suggests %s)\n", c.Key, c.Old, c.New)
	}
	if len(ap.Unchanged) > 0 {
		var keys []string
		for _, c := range ap.Unchanged {
			keys = append(keys, c.Key)
		}
		fmt.Printf("  unchanged (already optimal): %s\n", strings.Join(keys, ", "))
	}

	// Remind which recommendations this command can't touch.
	var manual []string
	for _, r := range plan.Recs {
		if r.Target == tune.TargetJVM || r.Target == tune.TargetModConfig || r.Target == tune.TargetManual {
			manual = append(manual, fmt.Sprintf("%s=%s (%s)", r.Key, r.Value, r.Target))
		}
	}
	if len(manual) > 0 {
		fmt.Printf("\n  apply by hand (not server.properties): %s\n", strings.Join(manual, "; "))
	}

	if !write {
		fmt.Printf("\nDry run. Re-run with -write to apply the %d change(s); a .bak backup is made first, and `tune -revert` undoes it.\n", len(ap.Changes))
		return
	}
	backup, err := ap.Write()
	if err != nil {
		fmt.Fprintln(os.Stderr, "write failed:", err)
		os.Exit(1)
	}
	if backup == "" {
		fmt.Println("\nNo changes written.")
		return
	}
	fmt.Printf("\nWrote %d change(s) to %s (backup at %s). Restart the server to apply; `tune -revert` to undo.\n", len(ap.Changes), propsPath, backup)
}

func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	endpoint := fs.String("tps-url", "", "HTTP endpoint returning {\"tps\":..,\"mspt\":..}; empty uses a stub reader")
	interval := fs.Duration("interval", 5*time.Second, "sampling interval")
	floor := fs.Float64("tps-floor", observe.DefaultThresholds().TPSFloor, "TPS below this is unhealthy")
	asJSON := fs.Bool("json", false, "emit one JSON object per sample")
	fs.Parse(args)

	var reader observe.TPSReader = observe.StubReader{}
	if *endpoint != "" {
		reader = observe.NewSparkHTTPReader(*endpoint)
	}
	thresholds := observe.DefaultThresholds()
	thresholds.TPSFloor = *floor

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "watching TPS via %s every %s (floor %.1f); ctrl-c to stop\n", reader.Name(), *interval, *floor)

	emit := func(s observe.Sample) {
		if *asJSON {
			printJSON(s)
			return
		}
		fmt.Printf("%s  [%s] %s\n", s.Time.Format("15:04:05"), s.Verdict, s.Detail)
	}

	err := observe.Watch(ctx, reader, thresholds, *interval, time.Now, emit)
	if err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "watch stopped:", err)
		os.Exit(1)
	}
}

func cmdBench(args []string) {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	endpoint := fs.String("tps-url", "", "HTTP endpoint returning {\"tps\":..,\"mspt\":..}; empty uses a stub reader")
	interval := fs.Duration("interval", 1*time.Second, "sampling interval")
	duration := fs.Duration("duration", 60*time.Second, "total measurement window")
	label := fs.String("label", "", "label for this run (e.g. the tuning being tested)")
	asJSON := fs.Bool("json", false, "emit the stats as JSON (suitable for before/after diffing)")
	out := fs.String("out", "", "also write the stats JSON to this file (for `bench-diff`)")
	fs.Parse(args)

	var reader observe.TPSReader = observe.StubReader{}
	if *endpoint != "" {
		reader = observe.NewSparkHTTPReader(*endpoint)
	}
	count := int(*duration / *interval)
	if count < 1 {
		count = 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "benchmarking %d samples @ %s via %s — keep the load constant; ctrl-c to stop early\n",
		count, *interval, reader.Name())

	stats, _ := observe.Bench(ctx, reader, observe.DefaultThresholds(), *interval, count, *label, time.Now)
	if *out != "" {
		if err := writeJSONFile(*out, stats); err != nil {
			fmt.Fprintln(os.Stderr, "writing -out file:", err)
		} else {
			fmt.Fprintf(os.Stderr, "saved stats to %s\n", *out)
		}
	}
	if *asJSON {
		printJSON(stats)
		return
	}
	printBenchStats(stats)
	fmt.Printf("\nTo compare a tuning change, run this again with the SAME load + a new -label, and diff the two.\n")
}

func printBenchStats(stats observe.BenchStats) {
	fmt.Printf("\nBenchmark%s — %d samples over %s\n", labelSuffix(stats.Label), stats.Samples, stats.Duration)
	fmt.Printf("  TPS:            min %.1f · mean %.2f\n", stats.TPSMin, stats.TPSMean)
	fmt.Printf("  MSPT:           mean %.2f · p95 %.2f · max %.2f ms\n", stats.MSPTMean, stats.MSPTP95, stats.MSPTMax)
	fmt.Printf("  CPU pressure:   mean %.1f%% · peak %.1f%%\n", stats.CPUPressureMean, stats.CPUPressurePeak)
	fmt.Printf("  I/O pressure:   mean %.1f%% · peak %.1f%%\n", stats.IOPressureMean, stats.IOPressurePeak)
	fmt.Printf("  mem pressure:   peak %.1f%%\n", stats.MemPressurePeak)
	fmt.Printf("  CPU throttled:  %d events during window\n", stats.ThrottledDelta)
	fmt.Printf("  verdicts:       ")
	for _, v := range []observe.Verdict{observe.Healthy, observe.HostStarved, observe.GameBound, observe.Unknown} {
		if n := stats.Verdicts[v]; n > 0 {
			fmt.Printf("%s=%d ", v, n)
		}
	}
	fmt.Println()
}

func cmdHost(args []string) {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	interval := fs.Duration("interval", time.Second, "delta sampling window")
	root := fs.String("root", "/sys/fs/cgroup/lxc", "cgroup root to scan (numeric subdirs = LXC CTIDs)")
	asJSON := fs.Bool("json", false, "emit ranked []CgroupLoad as JSON")
	fs.Parse(args)

	loads, err := hostagent.Sample(hostagent.Config{ScanRoot: *root, Interval: *interval})
	if err != nil {
		fmt.Fprintf(os.Stderr, "host scan failed (%v)\nRun this ON the Proxmox host as root (it reads %s).\n", err, *root)
		os.Exit(1)
	}
	if *asJSON {
		printJSON(hostagent.Rank(loads))
		return
	}
	fmt.Print(hostagent.Report(loads))
}

func cmdLoadtest(args []string) {
	fs := flag.NewFlagSet("loadtest", flag.ExitOnError)
	loadType := fs.String("load", "chunky", "load type: chunky (generation) | entity (mobs) | player (Carpet fake-player sim bubble)")
	rconAddr := fs.String("rcon-addr", "127.0.0.1:25575", "RCON address of the target server")
	tpsURL := fs.String("tps-url", "http://127.0.0.1:9225/tps", "companion TPS endpoint to sample")
	world := fs.String("world", "minecraft:overworld", "world to pregen (chunky load)")
	cx := fs.Int("center-x", 100000, "center X — for chunky, pick FAR/unexplored terrain to force generation")
	cz := fs.Int("center-z", 100000, "center Z")
	radius := fs.Int("radius-chunks", 16, "Chunky pregen radius in chunks (chunky load)")
	entityType := fs.String("entity-type", "minecraft:villager", "mob to summon (entity load) — pick one with AI")
	entityY := fs.Int("entity-y", 70, "Y to summon/spawn at — pick a valid surface (entity/player load)")
	entityCount := fs.Int("entity-count", 200, "how many AI mobs to summon (entity load)")
	playerCount := fs.Int("player-count", 1, "how many Carpet fake players to spawn (player load)")
	duration := fs.Duration("duration", 30*time.Second, "measurement + load window")
	interval := fs.Duration("interval", time.Second, "bench sampling interval")
	label := fs.String("label", "", "label for this run")
	out := fs.String("out", "", "write the bench stats JSON here (for bench-diff)")
	fs.Parse(args)
	if *loadType != "chunky" && *loadType != "entity" && *loadType != "player" {
		fmt.Fprintln(os.Stderr, "-load must be 'chunky', 'entity', or 'player'")
		os.Exit(2)
	}

	// Read the RCON password from the environment, never a flag: process args are
	// world-readable (`ps`), and leaking a server credential there is exactly the
	// kind of plaintext-secret mistake to avoid.
	pw := os.Getenv("TICKWARDEN_RCON_PASSWORD")
	if pw == "" {
		fmt.Fprintln(os.Stderr, "set TICKWARDEN_RCON_PASSWORD in the environment (not a flag — argv is world-readable)")
		os.Exit(2)
	}
	client, err := loadgen.Dial(*rconAddr, pw, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcon dial failed:", err)
		os.Exit(1)
	}
	defer client.Close()

	// Drive the controlled load concurrently so it spans the whole measurement
	// window (with a small margin on each end).
	loadWindow := *duration + 2*time.Second
	loadErr := make(chan error, 1)
	var loadDesc string
	if *loadType == "player" {
		loadDesc = fmt.Sprintf("%d Carpet fake player(s) at (%d,%d,%d) [real sim-distance bubble]", *playerCount, *cx, *entityY, *cz)
		go func() {
			loadErr <- loadgen.PlayerLoad(client, *cx, *entityY, *cz, *playerCount, loadWindow)
		}()
	} else if *loadType == "entity" {
		loadDesc = fmt.Sprintf("%d %s at (%d,%d,%d) [simulation load]", *entityCount, *entityType, *cx, *entityY, *cz)
		go func() {
			loadErr <- loadgen.EntityLoad(client, *entityType, *cx, *entityY, *cz, *entityCount, loadWindow)
		}()
	} else {
		loadDesc = fmt.Sprintf("Chunky burst at (%d,%d) r=%d chunks [generation load]", *cx, *cz, *radius)
		go func() {
			loadErr <- loadgen.ChunkyBurst(client, *world, *cx, *cz, *radius, loadWindow)
		}()
	}

	reader := observe.NewSparkHTTPReader(*tpsURL)
	count := int(*duration / *interval)
	if count < 1 {
		count = 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "loadtest: %s while sampling %d×%s\n", loadDesc, count, *interval)

	stats, _ := observe.Bench(ctx, reader, observe.DefaultThresholds(), *interval, count, *label, time.Now)
	if err := <-loadErr; err != nil {
		fmt.Fprintln(os.Stderr, "load driver warning:", err)
	}
	if *out != "" {
		if err := writeJSONFile(*out, stats); err != nil {
			fmt.Fprintln(os.Stderr, "writing -out:", err)
		} else {
			fmt.Fprintf(os.Stderr, "saved stats to %s\n", *out)
		}
	}
	printBenchStats(stats)
	fmt.Printf("\nFor an A/B: change ONE setting, restart, then re-run — but advance -center-x/-center-z by >2× radius onto\nvirgin terrain, or you'll measure cheap disk reloads instead of generation. Then `bench-diff` the two -out files.\n")
}

func cmdBenchDiff(args []string) {
	fs := flag.NewFlagSet("bench-diff", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the diff as JSON")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "usage: tickwarden bench-diff <before.json> <after.json>")
		os.Exit(2)
	}

	var a, b observe.BenchStats
	if err := readJSONFile(rest[0], &a); err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", rest[0], err)
		os.Exit(1)
	}
	if err := readJSONFile(rest[1], &b); err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", rest[1], err)
		os.Exit(1)
	}

	d := observe.Diff(a, b)
	if *asJSON {
		printJSON(d)
		return
	}
	fmt.Printf("A%s  vs  B%s\n\n", labelSuffix(a.Label), labelSuffix(b.Label))
	fmt.Printf("  TPS mean:       %.2f → %.2f  (%+.2f)\n", a.TPSMean, b.TPSMean, d.TPSMeanDelta)
	fmt.Printf("  MSPT mean:      %.2f → %.2f ms  (%+.2f)\n", a.MSPTMean, b.MSPTMean, d.MSPTMeanDelta)
	fmt.Printf("  MSPT p95:       %.2f → %.2f ms  (%+.2f)\n", a.MSPTP95, b.MSPTP95, d.MSPTP95Delta)
	fmt.Printf("  CPU pressure:   %.1f%% → %.1f%%  (%+.1fpp)\n", a.CPUPressureMean, b.CPUPressureMean, d.CPUPressureMeanDelta)
	fmt.Printf("  I/O pressure:   %.1f%% → %.1f%%  (%+.1fpp)\n", a.IOPressureMean, b.IOPressureMean, d.IOPressureMeanDelta)
	fmt.Printf("\n  → %s\n", d.Verdict)
	for _, n := range d.Notes {
		fmt.Printf("  ⚠ %s\n", n)
	}
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func labelSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " [" + s + "]"
}

func cmdIOStorm(args []string) {
	fs := flag.NewFlagSet("iostorm", flag.ExitOnError)
	interval := fs.Duration("interval", time.Second, "delta sampling window")
	root := fs.String("root", "/sys/fs/cgroup/lxc", "cgroup root to scan (numeric subdirs = LXC CTIDs)")
	asJSON := fs.Bool("json", false, "emit detected storms as JSON")
	fs.Parse(args)

	loads, err := hostagent.Sample(hostagent.Config{ScanRoot: *root, Interval: *interval})
	if err != nil {
		fmt.Fprintf(os.Stderr, "host scan failed (%v)\nRun this ON the Proxmox host as root (it reads %s).\n", err, *root)
		os.Exit(1)
	}
	storms := iostorm.Detect(loads)
	if *asJSON {
		printJSON(storms)
		return
	}
	fmt.Print(iostorm.Report(storms))
}

func cmdOstune(args []string) {
	fs := flag.NewFlagSet("ostune", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit findings as JSON")
	fs.Parse(args)

	findings, err := ostune.Scan(ostune.DefaultConfig())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ostune scan failed:", err)
		os.Exit(1)
	}
	if *asJSON {
		printJSON(findings)
		return
	}
	fmt.Print(ostune.Report(findings))
}

func cmdThermal(args []string) {
	fs := flag.NewFlagSet("thermal", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the thermal reading as JSON")
	fs.Parse(args)

	t, err := thermal.Read(thermal.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "thermal read failed:", err)
		os.Exit(1)
	}
	if *asJSON {
		printJSON(t)
		return
	}
	fmt.Println(t.Verdict())
	for _, z := range t.Zones {
		fmt.Printf("  %-18s %.1f°C\n", z.Label, z.Celsius)
	}
	if t.MaxFreqMHz > 0 {
		fmt.Printf("  clock: %.0f / %.0f MHz (%.0f%% of max)\n", t.CurFreqMHz, t.MaxFreqMHz, t.FreqCappedPct)
	}
	for _, n := range t.Notes {
		fmt.Printf("  • %s\n", n)
	}
	if t.Throttling {
		os.Exit(1) // alert-friendly
	}
}

func cmdHotspots(args []string) {
	fs := flag.NewFlagSet("hotspots", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:9225/hotspots", "companion /hotspots endpoint")
	asJSON := fs.Bool("json", false, "emit the hotspots as JSON")
	fs.Parse(args)

	hs, err := hotspots.Fetch(*url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hotspots fetch failed (%v)\nNeeds the companion mod >= 0.2.0 running on the server.\n", err)
		os.Exit(1)
	}
	if *asJSON {
		printJSON(hs)
		return
	}
	fmt.Print(hotspots.Report(hs))
}

// psiHeadroom adapts cgroup PSI to the pregen controller's LoadSampler: the box
// has headroom unless its own CPU or I/O is already under meaningful stall.
type psiHeadroom struct{ cpuMax, ioMax float64 }

func (h psiHeadroom) Headroom() (bool, string) {
	p := observe.ReadPressure()
	if !p.Available {
		return false, "cgroup PSI unavailable — staying paused to be safe"
	}
	if p.CPU.SomeAvg10 > h.cpuMax {
		return false, fmt.Sprintf("CPU pressure %.1f%% > %.1f%%", p.CPU.SomeAvg10, h.cpuMax)
	}
	if p.IO.SomeAvg10 > h.ioMax {
		return false, fmt.Sprintf("I/O pressure %.1f%% > %.1f%%", p.IO.SomeAvg10, h.ioMax)
	}
	return true, "idle"
}

type playersFromURL string

func (u playersFromURL) Online() (int, error) { return observe.FetchPlayers(string(u)) }

func cmdPregen(args []string) {
	fs := flag.NewFlagSet("pregen", flag.ExitOnError)
	rconAddr := fs.String("rcon-addr", "127.0.0.1:25575", "RCON address of the target server")
	tpsURL := fs.String("tps-url", "http://127.0.0.1:9225/tps", "companion endpoint (players + TPS)")
	poll := fs.Duration("poll", 20*time.Second, "decision interval")
	floor := fs.Float64("tps-floor", 19.0, "pause pregen if TPS drops below this")
	cpuMax := fs.Float64("cpu-pressure-max", 25.0, "pause pregen if CPU PSI some-avg10 exceeds this")
	ioMax := fs.Float64("io-pressure-max", 15.0, "pause pregen if I/O PSI some-avg10 exceeds this")
	fs.Parse(args)

	pw := os.Getenv("TICKWARDEN_RCON_PASSWORD")
	if pw == "" {
		fmt.Fprintln(os.Stderr, "set TICKWARDEN_RCON_PASSWORD in the environment (not a flag — argv is world-readable)")
		os.Exit(2)
	}
	client, err := loadgen.Dial(*rconAddr, pw, 5*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcon dial failed:", err)
		os.Exit(1)
	}
	defer client.Close()

	reader := observe.NewSparkHTTPReader(*tpsURL)
	ctrl := &pregen.Controller{
		Cmd:      client,
		Load:     psiHeadroom{cpuMax: *cpuMax, ioMax: *ioMax},
		Players:  playersFromURL(*tpsURL),
		TPS:      func() (float64, error) { tps, _, e := reader.Read(); return tps, e },
		TPSFloor: *floor,
		Poll:     *poll,
		OnStep: func(action string, err error) {
			if err != nil {
				fmt.Fprintln(os.Stderr, "pregen step error:", err)
				return
			}
			if action != pregen.ActionHold {
				fmt.Printf("%s  chunky %s\n", time.Now().Format("15:04:05"), action)
			}
		},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "pregen controller: resumes Chunky only when empty + TPS>=%.1f + low pressure; pauses otherwise. ctrl-c to stop.\n", *floor)
	fmt.Fprintln(os.Stderr, "(a Chunky task must already exist — `chunky world ... && chunky start` once — this only pause/continues it)")
	if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "pregen stopped:", err)
	}
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func orNA(s string) string {
	if s == "" {
		return "(unavailable on this platform)"
	}
	return s
}

func giB(b uint64) string {
	if b == 0 {
		return "(unknown)"
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

func rotational(r *bool) string {
	switch {
	case r == nil:
		return "(undetermined)"
	case *r:
		return "rotational (HDD)"
	default:
		return "solid-state (SSD/NVMe)"
	}
}

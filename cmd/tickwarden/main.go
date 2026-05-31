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
	"syscall"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/detect"
	"github.com/fungiblemoose/tickwarden/internal/observe"
	"github.com/fungiblemoose/tickwarden/internal/tune"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "detect":
		cmdDetect(os.Args[2:])
	case "tune":
		cmdTune(os.Args[2:])
	case "watch":
		cmdWatch(os.Args[2:])
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
  tickwarden detect [-json]        print the detected host/cgroup profile
  tickwarden tune   [-json]        recommend server settings, with reasons
  tickwarden watch  [flags]        correlate TPS dips with host starvation
  tickwarden version

Run a command with -h for its flags.
`)
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
	fs.Parse(args)

	plan := tune.Recommend(detect.Detect())
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

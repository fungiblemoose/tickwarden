package optimize

import (
	"fmt"
	"strings"

	"github.com/fungiblemoose/tickwarden/internal/tune"
)

// highPressurePct is the PSI some-avg10 above which we call out current
// stalling in the report. It's a soft "worth mentioning" line, not a verdict —
// `tickwarden watch`/`bench` are the tools for a real pressure judgement — so we
// keep the bar modest (a tick loop already feels a 20% stall).
const highPressurePct = 20.0

// Report renders the Result as the one-screen operator summary: a system line,
// the auto-appliable server.properties changes, the by-hand JVM/mod-config
// settings, the mod picture, and a pointer to the host-level tools this
// in-container run is blind to. It is the payoff — everything the other
// commands surface, prioritized into a single read.
//
// Recs are grouped by Target so the split mirrors what `apply` can and can't
// write: TargetProperties is auto-appliable; JVM/ModConfig/Manual are by-hand;
// TargetInfo recs (storage notes, the container-contention caveat) are folded
// into the system summary so nothing the tuner said silently disappears.
func (r Result) Report() string {
	var b strings.Builder

	// 1. One-line system summary.
	p := r.Profile
	fmt.Fprintf(&b, "tickwarden optimize — %s, %.1f effective cores, %s budget, %s\n",
		p.Virt, p.EffectiveCores, giB(p.MemoryBudgetBytes), rotational(p.StorageRotational))
	fmt.Fprintf(&b, "Sized for %d peak players (%s).\n", r.Players, measuredTag(r.PlayersMeasured))

	// TargetInfo recs are observations, not actions — surface them right under
	// the summary so storage warnings and the container caveat aren't lost.
	info := recsByTarget(r.Plan, tune.TargetInfo)
	for _, rec := range info {
		fmt.Fprintf(&b, "  i %s: %s\n", rec.Key, rec.Value)
	}

	// 2. server.properties changes — auto-appliable.
	b.WriteString("\nserver.properties changes (auto-appliable with -apply):\n")
	props := recsByTarget(r.Plan, tune.TargetProperties)
	if len(props) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, rec := range props {
		fmt.Fprintf(&b, "  [%s] %s = %s\n", rec.Confidence, rec.Key, rec.Value)
		fmt.Fprintf(&b, "        ↳ %s\n", rec.Reason)
	}

	// 3. Apply by hand — JVM flags, mod configs, manual swaps. These live outside
	// server.properties (a systemd unit, a mod TOML, swapping a jar), so apply
	// won't touch them; the operator does.
	b.WriteString("\nApply by hand (not server.properties — JVM flags, mod config, mod swaps):\n")
	var byHand []tune.Rec
	byHand = append(byHand, recsByTarget(r.Plan, tune.TargetJVM)...)
	byHand = append(byHand, recsByTarget(r.Plan, tune.TargetModConfig)...)
	byHand = append(byHand, recsByTarget(r.Plan, tune.TargetManual)...)
	if len(byHand) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, rec := range byHand {
		fmt.Fprintf(&b, "  [%s] %s = %s\n", rec.Confidence, rec.Key, rec.Value)
		fmt.Fprintf(&b, "        ↳ %s\n", rec.Reason)
	}

	// 4. Mods — detected list + curated advice. Distinguish "scanned, found
	// nothing" from "never looked": with no mods dir we made no claim about the
	// stack and assumed perf mods for the budget, which the operator should know.
	b.WriteString("\nMods:\n")
	if !r.modsScanned {
		b.WriteString("  no mods dir scanned — pass -mods-dir to detect installed mods and verify the stack.\n")
		b.WriteString("  (sizing assumed chunk-perf mods are present; pass -mods-dir to confirm or correct.)\n")
	} else {
		names := r.Mods.Names()
		if len(names) == 0 {
			b.WriteString("  detected: none recognized\n")
		} else {
			fmt.Fprintf(&b, "  detected: %s\n", strings.Join(names, ", "))
		}
		adv := r.ModAdvice
		if adv.Empty() {
			b.WriteString("  stack looks good — nothing to flag.\n")
		} else {
			for _, c := range adv.Conflicts {
				fmt.Fprintf(&b, "  ⚠ conflict:  %s\n", c)
			}
			for _, rd := range adv.Redundant {
				fmt.Fprintf(&b, "  · redundant: %s\n", rd)
			}
			for _, s := range adv.Suggestions {
				fmt.Fprintf(&b, "  + suggest:   %s\n", s)
			}
			for _, n := range adv.Notes {
				fmt.Fprintf(&b, "  i note:      %s\n", n)
			}
		}
	}

	// 5. Look below the JVM. This run is inside the container; OS-level and
	// cross-container findings are structurally invisible from here. Point at the
	// host-side tools that CAN see them, and if our own cgroup PSI shows current
	// stalling, say so — that's the one host symptom visible from in here.
	b.WriteString("\nLook below the JVM:\n")
	b.WriteString("  run `tickwarden ostune` / `tickwarden host` ON the Proxmox host for OS-level + cross-container\n")
	b.WriteString("  findings (this in-container run can't see them).\n")
	if r.Pressure.Available {
		if cpu := r.Pressure.CPU.SomeAvg10; cpu >= highPressurePct {
			fmt.Fprintf(&b, "  ⚠ right now: CPU pressure is %.1f%% (some-avg10) — you're being stalled; `tickwarden watch` to correlate.\n", cpu)
		}
		if io := r.Pressure.IO.SomeAvg10; io >= highPressurePct {
			fmt.Fprintf(&b, "  ⚠ right now: I/O pressure is %.1f%% (some-avg10) — storage/neighbor stall; `tickwarden watch` to correlate.\n", io)
		}
	}

	b.WriteString("\nLegend: solid = trust it · heuristic = sane default · contested = validate against your load\n")
	return b.String()
}

// recsByTarget returns the plan's recommendations for one target, preserving the
// tuner's emit order (which is already prioritized).
func recsByTarget(plan tune.Plan, t tune.Target) []tune.Rec {
	var out []tune.Rec
	for _, rec := range plan.Recs {
		if rec.Target == t {
			out = append(out, rec)
		}
	}
	return out
}

func measuredTag(measured bool) string {
	if measured {
		return "measured from companion"
	}
	return "assumed — not measured"
}

// giB and rotational format the system-summary line for the optimize report.
// They parallel main.go's display helpers but are deliberately worded for this
// report (e.g. "unknown memory" vs main.go's "(unknown)"); Report can't reach
// package main's copies anyway, so the formatting lives here.
func giB(b uint64) string {
	if b == 0 {
		return "unknown memory"
	}
	return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
}

func rotational(r *bool) string {
	switch {
	case r == nil:
		return "storage undetermined"
	case *r:
		return "rotational disk (HDD)"
	default:
		return "solid-state (SSD/NVMe)"
	}
}

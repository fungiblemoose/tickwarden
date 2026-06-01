// Package iostorm detects an "I/O storm" on a shared-storage Proxmox host — one
// container saturating the disk with write bursts while another container starves
// — and recommends a concrete cgroup-v2 throttle to smooth it out. It is strictly
// ADVISE-ONLY: nothing here touches the host. Detect inspects a fleet snapshot and
// Report renders human guidance; applying any fix is a deliberate manual step the
// operator performs after reading (and editing) the suggested command.
//
// Why this exists: this generalizes a real incident. A torrent container's
// multi-gigabyte write bursts to a shared NVMe LVM-thin pool periodically froze the
// Minecraft server's tick thread — the JVM's fsync/journal writes queued behind the
// torrent client's, so io.pressure on the Minecraft cgroup spiked and ticks stalled
// even though neither container was CPU-bound. internal/hostagent already *names*
// such a noisy neighbor (Verdict); iostorm goes one step further and hands the
// operator the exact knob to fix it: a per-cgroup write-bandwidth cap on the writer.
//
// Relationship to hostagent.Verdict: Verdict answers "who is the culprit?" in one
// line. iostorm answers "what do I type to stop it?" and is intentionally a bit
// stricter on the writer threshold — a throttle recommendation is an action, so we
// only emit one when the writer is clearly storming (not merely "busier than 20
// MB/s"), to avoid recommending caps on containers doing legitimate sustained I/O.
//
// Design: Detect is a pure function of []hostagent.CgroupLoad (no clock, no
// filesystem, no host), so the entire detection + recommendation logic is unit
// tested with synthetic fleets. Watch is a thin convenience that samples the live
// host repeatedly and feeds each snapshot through Detect; it carries no logic of
// its own beyond the sampling loop.
package iostorm

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fungiblemoose/tickwarden/internal/hostagent"
)

// Detection thresholds. A storm requires BOTH halves: a writer clearly saturating
// the disk AND a *distinct* victim visibly suffering for it. Either alone is not a
// storm — a lone heavy writer with no victim is just a container doing its job, and
// a pressured container with no heavy writer is being starved by something we can't
// name (and shouldn't guess at by throttling an innocent neighbor).
const (
	// WriterMinWriteMBps is the sustained write rate above which a container is a
	// candidate storm source. Set higher than hostagent's culprit threshold (20
	// MB/s) because emitting a throttle *recommendation* is an action: we want to
	// be confident the writer is genuinely flooding the pool, not merely warm.
	// ~80 MB/s sustained is well past normal config/log writes and into
	// bulk-transfer territory (torrent flush, backup, large file copy).
	WriterMinWriteMBps = 80.0

	// VictimMinIOPressure is the "some avg10" io.pressure (percent of the last 10s
	// in which at least one task stalled on I/O) above which a container counts as
	// starved. 10% sustained I/O stall is already enough to drag a latency-
	// sensitive tick loop below its 20ms budget.
	VictimMinIOPressure = 10.0

	// VictimMinThrottledFrac is an alternate victim signal: fraction of CFS periods
	// in which the container was CPU-throttled. A storm can manifest as the victim
	// being unable to make progress; either pressure OR throttling qualifies it.
	VictimMinThrottledFrac = 0.10
)

// DefaultSuggestedWbps is the write-bandwidth cap (bytes/second) recommended for a
// storming writer's cgroup. 150 MB/s is chosen deliberately to sit in the gap
// between two regimes:
//
//   - ABOVE typical home-internet download speeds. Even a gigabit residential line
//     tops out around ~110 MB/s of sustained download, so a torrent/download
//     container capped at 150 MB/s of *disk write* will never be the bottleneck for
//     legitimate downloads — they're network-bound long before they hit this cap.
//   - BELOW the disk's burst ceiling. An NVMe thin-pool can absorb gigabytes in a
//     few seconds; that instantaneous flood is exactly what queues a victim's
//     fsync behind it. Capping the writer at 150 MB/s spreads a GB-scale burst over
//     several seconds instead of milliseconds, leaving headroom for the victim's
//     small latency-critical writes to interleave.
//
// In short: large bursts get smoothed, ordinary downloads are untouched.
const DefaultSuggestedWbps int64 = 150 * 1000 * 1000 // 150 MB/s (decimal, matching MBps math)

// DevicePlaceholder is emitted in ApplyCmd where the block device's major:minor
// number belongs. CgroupLoad carries per-container I/O totals but NOT the device
// identity (io.stat is summed across devices in hostagent), so we cannot fill this
// in. The operator substitutes it from `lsblk` — see Report's guidance.
const DevicePlaceholder = "<device-maj:min>"

// Storm is one detected writer→victim contention pair plus the recommended fix.
type Storm struct {
	// Writer is the container saturating shared disk with writes.
	Writer hostagent.CgroupLoad
	// Victim is a distinct container showing I/O pressure and/or CPU throttling
	// consistent with being starved by Writer.
	Victim hostagent.CgroupLoad
	// SuggestedWbps is the write-bandwidth cap (bytes/s) to apply to Writer's
	// cgroup. Defaults to DefaultSuggestedWbps.
	SuggestedWbps int64
	// ApplyCmd is the exact cgroup-v2 io.max command to run ON the host to apply
	// SuggestedWbps to Writer's cgroup. It contains DevicePlaceholder, which the
	// operator MUST replace with the real block device major:minor (from `lsblk`)
	// before running — see the package doc and Report output.
	ApplyCmd string
}

// Detect scans a fleet snapshot and returns every writer→victim storm it finds.
// Pure: depends only on its argument (no host, no clock), so it's fully unit
// tested. The result is deterministic — writers are considered heaviest-first and,
// for each, the worst-suffering distinct victim is paired with it.
//
// Pairing model: a single dominant writer is the usual culprit, but we don't assume
// exactly one. Each container clearing WriterMinWriteMBps is treated as a potential
// storm source and matched to the most-pressured *other* container that clears a
// victim threshold. A container can be both (heavy writer that is itself pressured);
// it simply can't be its own victim. Returns nil when no pair qualifies.
func Detect(loads []hostagent.CgroupLoad) []Storm {
	writers := heaviestWritersFirst(loads)

	var storms []Storm
	for _, w := range writers {
		victim, ok := worstVictimExcept(loads, w.CTID)
		if !ok {
			continue
		}
		storms = append(storms, newStorm(w, victim))
	}
	return storms
}

// heaviestWritersFirst returns the loads clearing WriterMinWriteMBps, sorted by
// write rate descending (CTID breaks ties for determinism).
func heaviestWritersFirst(loads []hostagent.CgroupLoad) []hostagent.CgroupLoad {
	var writers []hostagent.CgroupLoad
	for _, l := range loads {
		if l.WriteMBps >= WriterMinWriteMBps {
			writers = append(writers, l)
		}
	}
	sort.SliceStable(writers, func(i, j int) bool {
		if writers[i].WriteMBps != writers[j].WriteMBps {
			return writers[i].WriteMBps > writers[j].WriteMBps
		}
		return writers[i].CTID < writers[j].CTID
	})
	return writers
}

// worstVictimExcept returns the most-pressured container other than exceptCTID that
// clears a victim threshold (io.pressure OR throttling). "Worst" ranks on
// io.pressure first since that's the direct signal of disk-starvation; among
// equal-pressure candidates a higher throttled fraction wins, then lower CTID.
func worstVictimExcept(loads []hostagent.CgroupLoad, exceptCTID int) (hostagent.CgroupLoad, bool) {
	var best hostagent.CgroupLoad
	found := false
	for _, l := range loads {
		if l.CTID == exceptCTID {
			continue
		}
		if !isVictim(l) {
			continue
		}
		if !found || victimWorse(l, best) {
			best, found = l, true
		}
	}
	return best, found
}

// isVictim reports whether a load shows starvation by either signal.
func isVictim(l hostagent.CgroupLoad) bool {
	return l.IOPressure >= VictimMinIOPressure || l.ThrottledFrac >= VictimMinThrottledFrac
}

// victimWorse reports whether a is a worse (more-starved) victim than b.
func victimWorse(a, b hostagent.CgroupLoad) bool {
	if a.IOPressure != b.IOPressure {
		return a.IOPressure > b.IOPressure
	}
	if a.ThrottledFrac != b.ThrottledFrac {
		return a.ThrottledFrac > b.ThrottledFrac
	}
	return a.CTID < b.CTID
}

// newStorm builds a Storm with the default cap and a ready-to-edit ApplyCmd.
func newStorm(writer, victim hostagent.CgroupLoad) Storm {
	return Storm{
		Writer:        writer,
		Victim:        victim,
		SuggestedWbps: DefaultSuggestedWbps,
		ApplyCmd:      applyCmd(writer.CTID, DefaultSuggestedWbps),
	}
}

// applyCmd renders the cgroup-v2 io.max write-throttle command for a CTID. io.max
// takes a "MAJ:MIN wbps=<n>" line; the device major:minor is unknown to us (see
// DevicePlaceholder) so it's left for the operator to fill from `lsblk`.
func applyCmd(ctid int, wbps int64) string {
	return fmt.Sprintf(`echo "%s wbps=%d" > /sys/fs/cgroup/lxc/%d/io.max`,
		DevicePlaceholder, wbps, ctid)
}

// Report renders detected storms as human-readable guidance with copy-pasteable
// (but device-placeholder-bearing) apply commands. Returns a clear all-clear line
// when no storms were detected, so the caller can print the result unconditionally.
func Report(storms []Storm) string {
	var b strings.Builder
	if len(storms) == 0 {
		b.WriteString("no I/O storm detected\n")
		return b.String()
	}

	plural := "storm"
	if len(storms) > 1 {
		plural = "storms"
	}
	fmt.Fprintf(&b, "%d I/O %s detected (advise-only — nothing was changed):\n", len(storms), plural)

	for i, s := range storms {
		fmt.Fprintf(&b, "\n[%d] writer %s (CT %d) is flooding shared disk at %.0f MB/s\n",
			i+1, s.Writer.Name, s.Writer.CTID, s.Writer.WriteMBps)
		fmt.Fprintf(&b, "    victim %s (CT %d) is starved (io.pressure %.0f, throttled %.0f%%)\n",
			s.Victim.Name, s.Victim.CTID, s.Victim.IOPressure, s.Victim.ThrottledFrac*100)
		fmt.Fprintf(&b, "    suggest capping the writer to %d MB/s of disk writes:\n",
			s.SuggestedWbps/1_000_000)
		fmt.Fprintf(&b, "        %s\n", s.ApplyCmd)
	}

	b.WriteString("\nBefore running: replace ")
	b.WriteString(DevicePlaceholder)
	b.WriteString(" with the backing block device's major:minor from `lsblk`\n")
	b.WriteString("(the column shown by `lsblk -o NAME,MAJ:MIN`). The cap smooths GB-scale\n")
	b.WriteString("write bursts while staying above home-internet download speeds, so normal\n")
	b.WriteString("downloads are unaffected. To remove a cap later: write \"MAJ:MIN wbps=max\".\n")
	return b.String()
}

package hostagent

import (
	"fmt"
	"sort"
	"strings"
)

// severity scores a load for ranking. It's a deliberately simple composite, not
// a calibrated model: contention shows up as either burning a shared resource
// (CPU cores, disk MBps) or suffering for it (pressure, throttling). We weight
// both so a heavy writer and a throttled victim both float to the top of the
// report — the human reading it wants to see both ends of the contention.
func severity(l CgroupLoad) float64 {
	return l.CoresUsed +
		(l.ReadMBps+l.WriteMBps)/100 + // 100 MB/s ≈ one core's worth of "hot"
		l.CPUPressure/100 +
		l.IOPressure/100 +
		l.ThrottledFrac
}

// Rank returns loads sorted hottest-first by severity. It sorts a copy so the
// caller's slice order is preserved; ties break on CTID for determinism (tests
// and stable output depend on this).
func Rank(loads []CgroupLoad) []CgroupLoad {
	out := make([]CgroupLoad, len(loads))
	copy(out, loads)
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := severity(out[i]), severity(out[j])
		if si != sj {
			return si > sj
		}
		return out[i].CTID < out[j].CTID
	})
	return out
}

// Verdict thresholds. A culprit is only named when one container is clearly
// driving I/O (writerMinMBps) AND at least one *other* container is visibly
// suffering (victimMinPressure pressure or victimMinThrottle throttling). Both
// halves must hold — a lone heavy writer with no victim isn't contention, it's
// just a busy container doing its job, and crying wolf erodes trust in the tool.
const (
	writerMinMBps     = 20.0
	victimMinPressure = 5.0
	victimMinThrottle = 0.05
)

// Verdict inspects ranked loads and returns a one-line culprit conclusion. The
// signal we trust most for "noisy neighbor on a shared host" is the disk: one
// container writing hard while another shows io.pressure or CPU throttling is
// the classic Proxmox shared-storage starvation. Returns "" when no clear
// culprit/victim pair exists.
func Verdict(loads []CgroupLoad) string {
	writer, victim := culpritPair(loads)
	if writer == nil || victim == nil {
		return ""
	}
	return fmt.Sprintf(
		"likely culprit: %s (CT %d) writing %.0f MB/s while %s (CT %d) is starved (io.pressure %.0f, throttled %.0f%%)",
		writer.Name, writer.CTID, writer.WriteMBps,
		victim.Name, victim.CTID, victim.IOPressure, victim.ThrottledFrac*100,
	)
}

// culpritPair finds the heaviest writer and the most-starved *different*
// container, returning both only if each clears its threshold. Pointers into a
// local copy keep the result independent of the caller's slice.
func culpritPair(loads []CgroupLoad) (writer, victim *CgroupLoad) {
	ranked := Rank(loads) // also gives us a stable copy to take addresses into
	for i := range ranked {
		l := &ranked[i]
		if l.WriteMBps >= writerMinMBps && (writer == nil || l.WriteMBps > writer.WriteMBps) {
			writer = l
		}
	}
	if writer == nil {
		return nil, nil
	}
	for i := range ranked {
		l := &ranked[i]
		if l.CTID == writer.CTID {
			continue
		}
		starved := l.IOPressure >= victimMinPressure || l.ThrottledFrac >= victimMinThrottle
		if !starved {
			continue
		}
		// Prefer the worst-pressured victim for a clear narrative.
		if victim == nil || l.IOPressure > victim.IOPressure {
			victim = l
		}
	}
	return writer, victim
}

// Report renders a human-readable "who's hot" table (hottest first) followed by
// the culprit verdict, suitable for printing to a terminal. Designed to be the
// at-a-glance answer an operator wants when a server feels laggy.
func Report(loads []CgroupLoad) string {
	ranked := Rank(loads)
	var b strings.Builder
	b.WriteString("CTID  NAME              CORES   READ_MBps  WRITE_MBps  CPU_PSI  IO_PSI  THROTTLE\n")
	for _, l := range ranked {
		name := l.Name
		if len(name) > 16 {
			name = name[:16]
		}
		fmt.Fprintf(&b, "%-5d %-16s  %5.2f  %9.1f  %10.1f  %6.1f  %6.1f  %6.0f%%\n",
			l.CTID, name, l.CoresUsed, l.ReadMBps, l.WriteMBps,
			l.CPUPressure, l.IOPressure, l.ThrottledFrac*100)
	}
	if v := Verdict(loads); v != "" {
		b.WriteString("\n")
		b.WriteString(v)
		b.WriteString("\n")
	} else {
		b.WriteString("\nno clear noisy-neighbor contention detected\n")
	}
	return b.String()
}

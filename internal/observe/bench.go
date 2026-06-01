package observe

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// BenchStats summarizes a measurement window: the distribution of tick health
// AND the host pressure observed alongside it. The pressure columns are the
// point — a benchmark that only reported TPS would miss that the box was
// starved, which is the very confound this tool exists to expose.
type BenchStats struct {
	Label           string          `json:"label"`
	Samples         int             `json:"samples"`
	Duration        string          `json:"duration"`
	TPSMin          float64         `json:"tps_min"`
	TPSMean         float64         `json:"tps_mean"`
	MSPTMean        float64         `json:"mspt_mean"`
	MSPTP95         float64         `json:"mspt_p95"`
	MSPTMax         float64         `json:"mspt_max"`
	CPUPressureMean float64         `json:"cpu_pressure_mean"`
	CPUPressurePeak float64         `json:"cpu_pressure_peak"`
	IOPressureMean  float64         `json:"io_pressure_mean"`
	IOPressurePeak  float64         `json:"io_pressure_peak"`
	MemPressurePeak float64         `json:"mem_pressure_peak"`
	ThrottledDelta  uint64          `json:"throttled_delta"` // throttle events during the window
	Verdicts        map[Verdict]int `json:"verdicts"`
}

// Bench runs a fixed-length measurement window, sampling at interval, and
// returns aggregated stats. It reuses the same TPS reader and pressure source
// as Watch so a benchmark and live monitoring agree by construction.
//
// To compare two tunings, run Bench under the SAME controlled load (a fixed
// Chunky region, or a set number of fake players) before and after the change
// and diff the results. Without a repeatable load the numbers aren't
// comparable — that caveat is the whole reason this harness exists.
func Bench(ctx context.Context, r TPSReader, t Thresholds, interval time.Duration, count int, label string, now func() time.Time) (BenchStats, []Sample) {
	samples := make([]Sample, 0, count)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var firstThrottle, lastThrottle uint64
	for len(samples) < count {
		select {
		case <-ctx.Done():
			goto done
		case <-ticker.C:
			tps, mspt, err := r.Read()
			p := ReadPressure()
			s := Sample{Time: now(), Pressure: p, TPS: tps, MSPT: mspt}
			if err != nil {
				s.Verdict, s.Detail = Unknown, "TPS source error: "+err.Error()
			} else {
				s.Verdict, s.Detail = Correlate(tps, mspt, p, t)
			}
			if len(samples) == 0 {
				firstThrottle = p.NrThrottled
			}
			lastThrottle = p.NrThrottled
			samples = append(samples, s)
		}
	}
done:
	return summarize(samples, label, interval, firstThrottle, lastThrottle), samples
}

func summarize(samples []Sample, label string, interval time.Duration, firstThrottle, lastThrottle uint64) BenchStats {
	st := BenchStats{
		Label:    label,
		Samples:  len(samples),
		Duration: (interval * time.Duration(len(samples))).String(),
		Verdicts: map[Verdict]int{},
		TPSMin:   20.0,
	}
	if len(samples) == 0 {
		st.TPSMin = 0
		return st
	}
	if lastThrottle >= firstThrottle {
		st.ThrottledDelta = lastThrottle - firstThrottle
	}

	var tpsSum, msptSum, cpuSum, ioSum float64
	mspts := make([]float64, 0, len(samples))
	for _, s := range samples {
		tpsSum += s.TPS
		if s.TPS < st.TPSMin {
			st.TPSMin = s.TPS
		}
		msptSum += s.MSPT
		mspts = append(mspts, s.MSPT)
		if s.MSPT > st.MSPTMax {
			st.MSPTMax = s.MSPT
		}
		cpuSum += s.Pressure.CPU.SomeAvg10
		if s.Pressure.CPU.SomeAvg10 > st.CPUPressurePeak {
			st.CPUPressurePeak = s.Pressure.CPU.SomeAvg10
		}
		ioSum += s.Pressure.IO.SomeAvg10
		if s.Pressure.IO.SomeAvg10 > st.IOPressurePeak {
			st.IOPressurePeak = s.Pressure.IO.SomeAvg10
		}
		if s.Pressure.Memory.SomeAvg10 > st.MemPressurePeak {
			st.MemPressurePeak = s.Pressure.Memory.SomeAvg10
		}
		st.Verdicts[s.Verdict]++
	}
	n := float64(len(samples))
	st.TPSMean = tpsSum / n
	st.MSPTMean = msptSum / n
	st.CPUPressureMean = cpuSum / n
	st.IOPressureMean = ioSum / n
	st.MSPTP95 = percentile(mspts, 95)
	return st
}

// BenchDiff compares two benchmark runs (A = before, B = after a change).
type BenchDiff struct {
	A, B                 BenchStats `json:"-"`
	TPSMeanDelta         float64    `json:"tps_mean_delta"`
	MSPTMeanDelta        float64    `json:"mspt_mean_delta"` // negative = faster (good)
	MSPTP95Delta         float64    `json:"mspt_p95_delta"`
	CPUPressureMeanDelta float64    `json:"cpu_pressure_mean_delta"`
	IOPressureMeanDelta  float64    `json:"io_pressure_mean_delta"`
	Verdict              string     `json:"verdict"`
	Trustworthy          bool       `json:"trustworthy"`
	Notes                []string   `json:"notes"`
}

// Thresholds for calling a difference real rather than noise.
const (
	msptNoiseMs       = 0.5  // sub-0.5ms MSPT moves are noise
	msptNoiseFraction = 0.05 // ...or moves under 5%
	pressureConfound  = 5.0  // >5pp pressure difference means the load wasn't constant
)

// Diff compares two runs and, crucially, refuses to trust the comparison if the
// host pressure differed between them — because then any TPS/MSPT change might
// just reflect what else the box was doing, not the tuning. That confound is
// exactly what every naive before/after benchmark misses.
func Diff(a, b BenchStats) BenchDiff {
	d := BenchDiff{
		A:                    a,
		B:                    b,
		TPSMeanDelta:         b.TPSMean - a.TPSMean,
		MSPTMeanDelta:        b.MSPTMean - a.MSPTMean,
		MSPTP95Delta:         b.MSPTP95 - a.MSPTP95,
		CPUPressureMeanDelta: b.CPUPressureMean - a.CPUPressureMean,
		IOPressureMeanDelta:  b.IOPressureMean - a.IOPressureMean,
		Trustworthy:          true,
	}

	if abs(d.CPUPressureMeanDelta) > pressureConfound {
		d.Trustworthy = false
		d.Notes = append(d.Notes, fmt.Sprintf("host CPU pressure differed by %.1fpp between runs (A=%.1f%%, B=%.1f%%) — the load wasn't constant; this comparison may reflect host conditions, not your tuning", d.CPUPressureMeanDelta, a.CPUPressureMean, b.CPUPressureMean))
	}
	if abs(d.IOPressureMeanDelta) > pressureConfound {
		d.Trustworthy = false
		d.Notes = append(d.Notes, fmt.Sprintf("host I/O pressure differed by %.1fpp between runs (A=%.1f%%, B=%.1f%%) — re-run under a constant load", d.IOPressureMeanDelta, a.IOPressureMean, b.IOPressureMean))
	}
	if a.Samples < 10 || b.Samples < 10 {
		d.Notes = append(d.Notes, "small sample (<10) — treat the result as directional, not precise")
	}

	// MSPT (mean tick time) is the sensitive signal; TPS is usually pinned at 20.
	meaningful := abs(d.MSPTMeanDelta) > msptNoiseMs && abs(d.MSPTMeanDelta) > a.MSPTMean*msptNoiseFraction
	switch {
	case !meaningful:
		d.Verdict = "no meaningful tick-time difference (within noise)"
	case d.MSPTMeanDelta < 0:
		d.Verdict = fmt.Sprintf("B is faster: mean tick %.2f→%.2f ms (%.1f%%)", a.MSPTMean, b.MSPTMean, pct(d.MSPTMeanDelta, a.MSPTMean))
	default:
		d.Verdict = fmt.Sprintf("B is slower: mean tick %.2f→%.2f ms (+%.1f%%)", a.MSPTMean, b.MSPTMean, pct(d.MSPTMeanDelta, a.MSPTMean))
	}
	if !d.Trustworthy {
		d.Verdict = "INCONCLUSIVE — " + d.Verdict
	}
	return d
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func pct(delta, base float64) float64 {
	if base == 0 {
		return 0
	}
	return delta / base * 100
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	rank := int(p / 100 * float64(len(sorted)-1))
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

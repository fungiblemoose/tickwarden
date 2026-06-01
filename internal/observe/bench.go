package observe

import (
	"context"
	"sort"
	"time"
)

// BenchStats summarizes a measurement window: the distribution of tick health
// AND the host pressure observed alongside it. The pressure columns are the
// point — a benchmark that only reported TPS would miss that the box was
// starved, which is the very confound this tool exists to expose.
type BenchStats struct {
	Label    string            `json:"label"`
	Samples  int               `json:"samples"`
	Duration string            `json:"duration"`
	TPSMin   float64           `json:"tps_min"`
	TPSMean  float64           `json:"tps_mean"`
	MSPTMean float64           `json:"mspt_mean"`
	MSPTP95  float64           `json:"mspt_p95"`
	MSPTMax  float64           `json:"mspt_max"`
	CPUPressureMean float64    `json:"cpu_pressure_mean"`
	CPUPressurePeak float64    `json:"cpu_pressure_peak"`
	IOPressureMean  float64    `json:"io_pressure_mean"`
	IOPressurePeak  float64    `json:"io_pressure_peak"`
	MemPressurePeak float64    `json:"mem_pressure_peak"`
	ThrottledDelta  uint64     `json:"throttled_delta"` // throttle events during the window
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

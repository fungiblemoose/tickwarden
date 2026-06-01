package observe

import (
	"context"
	"fmt"
	"time"
)

// Verdict classifies a TPS sample by correlating it with host pressure.
type Verdict string

const (
	Healthy     Verdict = "healthy"      // TPS fine
	HostStarved Verdict = "host-starved" // TPS low AND cgroup under pressure/throttled
	GameBound   Verdict = "game-bound"   // TPS low but no host pressure → it's the modpack/world
	Unknown     Verdict = "unknown"      // TPS low, pressure unreadable
)

// Sample is one correlated observation.
type Sample struct {
	Time     time.Time `json:"time"`
	TPS      float64   `json:"tps"`
	MSPT     float64   `json:"mspt"`
	Pressure Pressure  `json:"pressure"`
	Verdict  Verdict   `json:"verdict"`
	Detail   string    `json:"detail"`
}

// Thresholds tune the correlation. Defaults are deliberately conservative.
type Thresholds struct {
	TPSFloor       float64 // below this, a tick is "unhealthy"
	PSIPressurePct float64 // some-avg10 above this counts as starvation
}

func DefaultThresholds() Thresholds {
	return Thresholds{TPSFloor: 19.0, PSIPressurePct: 10.0}
}

// Correlate is the core diagnostic: given a TPS reading and a pressure
// snapshot, decide whether a tick dip is the host's fault or the game's. This
// is the sentence tickwarden exists to print — "your modpack is fine, the host
// choked your I/O" — and it needs no host-side access to say it.
func Correlate(tps, mspt float64, p Pressure, t Thresholds) (Verdict, string) {
	if tps >= t.TPSFloor {
		return Healthy, fmt.Sprintf("%.1f TPS / %.1f MSPT — nominal", tps, mspt)
	}
	if !p.Available {
		return Unknown, fmt.Sprintf("%.1f TPS but cgroup PSI unreadable (not cgroup-v2, or not containerized) — can't attribute", tps)
	}

	// Find the most pressured resource.
	worst, worstName := p.CPU.SomeAvg10, "CPU"
	if p.IO.SomeAvg10 > worst {
		worst, worstName = p.IO.SomeAvg10, "I/O"
	}
	if p.Memory.SomeAvg10 > worst {
		worst, worstName = p.Memory.SomeAvg10, "memory"
	}

	throttled := p.NrThrottled > 0
	if worst >= t.PSIPressurePct || throttled {
		detail := fmt.Sprintf("%.1f TPS coincides with %s pressure (some-avg10=%.1f%%)", tps, worstName, worst)
		if throttled {
			detail += fmt.Sprintf("; CPU quota throttled %d times — a neighbor or your own quota is starving the tick thread", p.NrThrottled)
		} else {
			detail += " — the bottleneck is below the JVM, not your modpack"
		}
		return HostStarved, detail
	}

	return GameBound, fmt.Sprintf("%.1f TPS / %.1f MSPT with no host pressure — it's the world/modpack, profile with spark", tps, mspt)
}

// Watch runs the correlation loop until ctx is cancelled, invoking emit for
// every sample (so the caller decides whether to log JSON, print lines, etc).
func Watch(ctx context.Context, r TPSReader, t Thresholds, interval time.Duration, now func() time.Time, emit func(Sample)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tps, mspt, err := r.Read()
			p := ReadPressure()
			s := Sample{Time: now(), Pressure: p, TPS: tps, MSPT: mspt}
			if err != nil {
				s.Verdict = Unknown
				s.Detail = "TPS source error: " + err.Error()
			} else {
				s.Verdict, s.Detail = Correlate(tps, mspt, p, t)
			}
			emit(s)
		}
	}
}

package observe

import (
	"strings"
	"testing"
)

// avail builds an Available pressure snapshot with the given some-avg10 values.
func avail(cpu, io, mem float64) Pressure {
	return Pressure{
		CPU:       PSI{SomeAvg10: cpu},
		IO:        PSI{SomeAvg10: io},
		Memory:    PSI{SomeAvg10: mem},
		Available: true,
	}
}

func TestStarvedNow_PSIOverThreshold(t *testing.T) {
	cur := avail(2, 42, 1)
	starved, detail := StarvedNow(cur, cur, 10)
	if !starved {
		t.Fatal("42% I/O pressure at a 10% threshold should be starved")
	}
	if !strings.Contains(detail, "I/O") || !strings.Contains(detail, "42.0") {
		t.Fatalf("detail should name the worst resource and value: %q", detail)
	}
}

func TestStarvedNow_BelowThresholdNotStarved(t *testing.T) {
	cur := avail(4, 6, 0)
	if starved, _ := StarvedNow(cur, cur, 10); starved {
		t.Fatal("pressure below the threshold must not be starved")
	}
}

// The throttle counters are cumulative since boot: only an INCREASE between
// polls means "throttled just now".
func TestStarvedNow_ThrottleDeltaOnly(t *testing.T) {
	prev := avail(1, 1, 0)
	prev.NrThrottled = 500 // old history, e.g. from before the daemon started

	same := prev
	if starved, _ := StarvedNow(prev, same, 10); starved {
		t.Fatal("an unchanged cumulative throttle count must not be starved")
	}

	cur := prev
	cur.NrThrottled = 503
	starved, detail := StarvedNow(prev, cur, 10)
	if !starved {
		t.Fatal("new throttle events since the last poll should be starved")
	}
	if !strings.Contains(detail, "3 time(s)") {
		t.Fatalf("detail should carry the throttle delta: %q", detail)
	}
}

// Unreadable PSI can't attribute anything, so it is never "starved".
func TestStarvedNow_UnavailableNeverStarved(t *testing.T) {
	cur := Pressure{CPU: PSI{SomeAvg10: 99}, NrThrottled: 9} // Available: false
	if starved, _ := StarvedNow(Pressure{}, cur, 10); starved {
		t.Fatal("unavailable pressure must never read as starved")
	}
}

// pct <= 0 disables the PSI test but throttle deltas still count: throttling
// is a hard fact, not a threshold judgement.
func TestStarvedNow_ZeroPctStillSeesThrottling(t *testing.T) {
	prev := avail(99, 0, 0)
	if starved, _ := StarvedNow(prev, prev, 0); starved {
		t.Fatal("pct<=0 must disable the PSI threshold test")
	}
	cur := prev
	cur.NrThrottled = 1
	if starved, _ := StarvedNow(prev, cur, 0); !starved {
		t.Fatal("a throttle delta should still register with pct<=0")
	}
}

package iostorm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fungiblemoose/tickwarden/internal/hostagent"
)

// stormFleet is the canonical incident: CT 110 (torrentbox) floods the shared NVMe
// while CT 120 (java / Minecraft) starves, plus an idle 130. Returns a fresh slice
// each call so mutation tests can't leak between cases.
func stormFleet() []hostagent.CgroupLoad {
	return []hostagent.CgroupLoad{
		{CTID: 130, Name: "idle", CoresUsed: 0.01},
		{CTID: 120, Name: "java", CoresUsed: 0.8, IOPressure: 35.0, ThrottledFrac: 0.4},
		{CTID: 110, Name: "torrentbox", CoresUsed: 0.5, WriteMBps: 220.0, IOPressure: 1.0},
	}
}

func TestDetect_ClearStorm(t *testing.T) {
	storms := Detect(stormFleet())
	if len(storms) != 1 {
		t.Fatalf("expected exactly 1 storm, got %d: %+v", len(storms), storms)
	}
	s := storms[0]
	if s.Writer.CTID != 110 {
		t.Errorf("writer should be CT 110 (torrentbox), got CT %d (%s)", s.Writer.CTID, s.Writer.Name)
	}
	if s.Victim.CTID != 120 {
		t.Errorf("victim should be CT 120 (java), got CT %d (%s)", s.Victim.CTID, s.Victim.Name)
	}
	if s.SuggestedWbps != DefaultSuggestedWbps {
		t.Errorf("SuggestedWbps: got %d want %d", s.SuggestedWbps, DefaultSuggestedWbps)
	}
}

func TestDetect_ApplyCmdShape(t *testing.T) {
	s := Detect(stormFleet())[0]
	// Must target the WRITER's cgroup (the thing we throttle), not the victim's.
	wantPath := fmt.Sprintf("/sys/fs/cgroup/lxc/%d/io.max", s.Writer.CTID)
	if !strings.Contains(s.ApplyCmd, wantPath) {
		t.Errorf("ApplyCmd should write to writer cgroup %q:\n  %s", wantPath, s.ApplyCmd)
	}
	if !strings.Contains(s.ApplyCmd, DevicePlaceholder) {
		t.Errorf("ApplyCmd should carry the device placeholder %q:\n  %s", DevicePlaceholder, s.ApplyCmd)
	}
	if !strings.Contains(s.ApplyCmd, fmt.Sprintf("wbps=%d", DefaultSuggestedWbps)) {
		t.Errorf("ApplyCmd should set wbps=%d:\n  %s", DefaultSuggestedWbps, s.ApplyCmd)
	}
}

func TestDetect_NoStormWriterAlone(t *testing.T) {
	// Heavy writer, but nobody is suffering → not a storm (don't recommend a cap).
	loads := []hostagent.CgroupLoad{
		{CTID: 110, Name: "torrentbox", WriteMBps: 300.0, IOPressure: 1.0},
		{CTID: 130, Name: "idle", CoresUsed: 0.01},
	}
	if storms := Detect(loads); len(storms) != 0 {
		t.Fatalf("writer alone is not a storm, got %+v", storms)
	}
}

func TestDetect_NoStormVictimAlone(t *testing.T) {
	// Victim is badly pressured, but no container clears the writer threshold →
	// we can't name a culprit to throttle, so no storm.
	loads := []hostagent.CgroupLoad{
		{CTID: 120, Name: "java", IOPressure: 50.0, ThrottledFrac: 0.6},
		{CTID: 110, Name: "light", WriteMBps: 5.0},
	}
	if storms := Detect(loads); len(storms) != 0 {
		t.Fatalf("victim alone is not a storm, got %+v", storms)
	}
}

func TestDetect_WriterCannotBeOwnVictim(t *testing.T) {
	// A single container that is both a heavy writer AND pressured must NOT pair
	// with itself; with no other suffering container there is no storm.
	loads := []hostagent.CgroupLoad{
		{CTID: 110, Name: "torrentbox", WriteMBps: 250.0, IOPressure: 40.0, ThrottledFrac: 0.5},
		{CTID: 130, Name: "idle", CoresUsed: 0.01},
	}
	if storms := Detect(loads); len(storms) != 0 {
		t.Fatalf("writer cannot be its own victim, got %+v", storms)
	}
}

func TestDetect_ThrottleOnlyVictimQualifies(t *testing.T) {
	// Victim shows no io.pressure but is CPU-throttled past the threshold; the
	// alternate victim signal must still trigger a storm.
	loads := []hostagent.CgroupLoad{
		{CTID: 110, Name: "torrentbox", WriteMBps: 120.0},
		{CTID: 120, Name: "java", IOPressure: 0.0, ThrottledFrac: 0.20},
	}
	storms := Detect(loads)
	if len(storms) != 1 || storms[0].Victim.CTID != 120 {
		t.Fatalf("throttle-only victim should qualify, got %+v", storms)
	}
}

func TestDetect_WriterThresholdEdge(t *testing.T) {
	victim := hostagent.CgroupLoad{CTID: 120, Name: "java", IOPressure: VictimMinIOPressure + 5}

	// Exactly at the writer threshold qualifies (>=).
	at := Detect([]hostagent.CgroupLoad{
		victim, {CTID: 110, Name: "w", WriteMBps: WriterMinWriteMBps},
	})
	if len(at) != 1 {
		t.Fatalf("writer at exactly the threshold should storm, got %+v", at)
	}
	// A hair below does not.
	below := Detect([]hostagent.CgroupLoad{
		victim, {CTID: 110, Name: "w", WriteMBps: WriterMinWriteMBps - 0.01},
	})
	if len(below) != 0 {
		t.Fatalf("writer below the threshold must not storm, got %+v", below)
	}
}

func TestDetect_VictimThresholdEdges(t *testing.T) {
	writer := hostagent.CgroupLoad{CTID: 110, Name: "w", WriteMBps: 200.0}

	// io.pressure exactly at the threshold qualifies.
	atPSI := Detect([]hostagent.CgroupLoad{
		writer, {CTID: 120, Name: "v", IOPressure: VictimMinIOPressure},
	})
	if len(atPSI) != 1 {
		t.Fatalf("victim io.pressure at threshold should qualify, got %+v", atPSI)
	}
	// Both victim signals a hair below their thresholds → no storm.
	below := Detect([]hostagent.CgroupLoad{
		writer,
		{CTID: 120, Name: "v", IOPressure: VictimMinIOPressure - 0.01, ThrottledFrac: VictimMinThrottledFrac - 0.001},
	})
	if len(below) != 0 {
		t.Fatalf("victim below both thresholds must not storm, got %+v", below)
	}
	// Throttle exactly at threshold qualifies.
	atThrottle := Detect([]hostagent.CgroupLoad{
		writer, {CTID: 120, Name: "v", ThrottledFrac: VictimMinThrottledFrac},
	})
	if len(atThrottle) != 1 {
		t.Fatalf("victim throttle at threshold should qualify, got %+v", atThrottle)
	}
}

func TestDetect_PicksHeaviestWriterAndWorstVictim(t *testing.T) {
	// Two writers and two victims: the heaviest writer pairs with the worst victim.
	loads := []hostagent.CgroupLoad{
		{CTID: 111, Name: "small-writer", WriteMBps: 90.0},
		{CTID: 110, Name: "big-writer", WriteMBps: 400.0},
		{CTID: 121, Name: "mild-victim", IOPressure: 12.0},
		{CTID: 120, Name: "bad-victim", IOPressure: 60.0},
	}
	storms := Detect(loads)
	if len(storms) != 2 {
		t.Fatalf("expected one storm per writer (2), got %d: %+v", len(storms), storms)
	}
	// First storm = heaviest writer (110) with worst victim (120).
	if storms[0].Writer.CTID != 110 || storms[0].Victim.CTID != 120 {
		t.Errorf("first storm should be writer 110 / victim 120, got writer %d / victim %d",
			storms[0].Writer.CTID, storms[0].Victim.CTID)
	}
}

func TestDetect_Empty(t *testing.T) {
	if storms := Detect(nil); storms != nil {
		t.Fatalf("nil fleet should yield nil storms, got %+v", storms)
	}
	if storms := Detect([]hostagent.CgroupLoad{}); len(storms) != 0 {
		t.Fatalf("empty fleet should yield no storms, got %+v", storms)
	}
}

func TestDetect_DoesNotMutateInput(t *testing.T) {
	in := stormFleet()
	_ = Detect(in)
	want := stormFleet()
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("Detect mutated input at %d: got %+v want %+v", i, in[i], want[i])
		}
	}
}

func TestReport_StormGuidance(t *testing.T) {
	r := Report(Detect(stormFleet()))
	for _, want := range []string{
		"torrentbox", "CT 110", "java", "CT 120",
		"advise-only", DevicePlaceholder, "lsblk", "io.max", "150 MB/s",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
}

func TestReport_NoStorm(t *testing.T) {
	r := Report(nil)
	if !strings.Contains(r, "no I/O storm detected") {
		t.Errorf("empty report should state no storm:\n%s", r)
	}
}

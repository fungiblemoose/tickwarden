package observe

import "testing"

func TestDiff_FasterUnderConstantLoad(t *testing.T) {
	a := BenchStats{Samples: 30, MSPTMean: 10.0, MSPTP95: 14, TPSMean: 20, CPUPressureMean: 15}
	b := BenchStats{Samples: 30, MSPTMean: 6.0, MSPTP95: 8, TPSMean: 20, CPUPressureMean: 16}
	d := Diff(a, b)
	if !d.Trustworthy {
		t.Fatalf("similar pressure should be trustworthy, notes=%v", d.Notes)
	}
	if d.MSPTMeanDelta != -4.0 {
		t.Fatalf("expected -4.0 delta, got %v", d.MSPTMeanDelta)
	}
	if want := "B is faster"; d.Verdict[:11] != want {
		t.Fatalf("expected faster verdict, got %q", d.Verdict)
	}
}

func TestDiff_ConfoundedByHostLoad(t *testing.T) {
	// B looks faster, but the host was far less loaded during B — untrustworthy.
	a := BenchStats{Samples: 30, MSPTMean: 12.0, TPSMean: 20, CPUPressureMean: 40}
	b := BenchStats{Samples: 30, MSPTMean: 6.0, TPSMean: 20, CPUPressureMean: 8}
	d := Diff(a, b)
	if d.Trustworthy {
		t.Fatal("32pp CPU pressure difference must mark the diff untrustworthy")
	}
	if d.Verdict[:12] != "INCONCLUSIVE" {
		t.Fatalf("expected INCONCLUSIVE verdict, got %q", d.Verdict)
	}
}

func TestDiff_Noise(t *testing.T) {
	a := BenchStats{Samples: 30, MSPTMean: 6.00, TPSMean: 20, CPUPressureMean: 15}
	b := BenchStats{Samples: 30, MSPTMean: 6.10, TPSMean: 20, CPUPressureMean: 15}
	d := Diff(a, b)
	if d.Verdict != "no meaningful tick-time difference (within noise)" {
		t.Fatalf("0.1ms move should be noise, got %q", d.Verdict)
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentile(xs, 95); p < 9 {
		t.Fatalf("p95 of 1..10 should be near the top, got %v", p)
	}
	if percentile(nil, 95) != 0 {
		t.Fatal("p95 of empty should be 0")
	}
}

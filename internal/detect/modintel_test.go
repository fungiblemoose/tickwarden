package detect

import (
	"strings"
	"testing"
)

// modSet builds a ModSet directly from canonical ids, bypassing the filesystem
// scan. Advise is a pure function of the set, so this keeps the advice tests
// independent of ScanMods and its temp-dir plumbing (covered in mods_test.go).
func modSet(ids ...string) ModSet {
	m := ModSet{found: map[string]struct{}{}}
	for _, id := range ids {
		m.found[id] = struct{}{}
	}
	return m
}

// containsSubstr reports whether any line in lines contains sub (case-sensitive).
func containsSubstr(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestAdviseLightingConflict(t *testing.T) {
	// Both light engines installed: must be flagged as a conflict, and must NOT
	// also appear as a lighting suggestion (we already have one — two, in fact).
	a := Advise(modSet("starlight", "scalablelux"))
	if !containsSubstr(a.Conflicts, "starlight") || !containsSubstr(a.Conflicts, "scalablelux") {
		t.Fatalf("expected a starlight+scalablelux lighting conflict, got Conflicts=%v", a.Conflicts)
	}
	if containsSubstr(a.Suggestions, "no lighting mod") {
		t.Fatalf("should not suggest a lighting mod when two are installed; Suggestions=%v", a.Suggestions)
	}
}

func TestAdviseLightingSuggestedWhenAbsent(t *testing.T) {
	// No light engine at all: suggest adding one, with no conflict.
	a := Advise(modSet())
	if len(a.Conflicts) != 0 {
		t.Fatalf("empty set should have no conflicts, got %v", a.Conflicts)
	}
	if !containsSubstr(a.Suggestions, "no lighting mod") {
		t.Fatalf("empty set should suggest a lighting mod; Suggestions=%v", a.Suggestions)
	}
}

func TestAdviseSingleLightingEngineNoNoise(t *testing.T) {
	// Exactly one light engine: neither a conflict nor a lighting suggestion.
	for _, id := range []string{"starlight", "scalablelux"} {
		a := Advise(modSet(id))
		if len(a.Conflicts) != 0 {
			t.Fatalf("%s alone should not conflict, got %v", id, a.Conflicts)
		}
		if containsSubstr(a.Suggestions, "no lighting mod") {
			t.Fatalf("%s alone should not trigger a lighting suggestion; Suggestions=%v", id, a.Suggestions)
		}
	}
}

func TestAdviseEmptySetSuggestsCorePerfMods(t *testing.T) {
	// An empty set should surface every core perf-mod suggestion: lighting,
	// c2me, lithium, ferritecore, krypton, spark. These are the baseline stack.
	a := Advise(modSet())
	wantTokens := []string{
		"no lighting mod", // lighting
		"no c2me",         // chunk perf
		"no lithium",      // tick
		"no ferritecore",  // memory
		"no krypton",      // network
		"no spark",        // profiling
	}
	for _, tok := range wantTokens {
		if !containsSubstr(a.Suggestions, tok) {
			t.Fatalf("empty set should suggest %q; Suggestions=%v", tok, a.Suggestions)
		}
	}
	if len(a.Suggestions) != len(wantTokens) {
		t.Fatalf("empty set should yield exactly %d suggestions, got %d: %v",
			len(wantTokens), len(a.Suggestions), a.Suggestions)
	}
	if a.Empty() {
		t.Fatal("empty mod set should still produce advice (suggestions), Empty() should be false")
	}
}

func TestAdviseFullPerfSetHasNoSuggestions(t *testing.T) {
	// A complete, sane perf stack (one light engine + chunk/tick/memory/network
	// + profiler) should leave nothing to suggest and nothing to flag.
	a := Advise(modSet("starlight", "c2me", "lithium", "ferritecore", "krypton", "spark"))
	if len(a.Suggestions) != 0 {
		t.Fatalf("full perf set should yield no suggestions, got %v", a.Suggestions)
	}
	if len(a.Conflicts) != 0 {
		t.Fatalf("full perf set should yield no conflicts, got %v", a.Conflicts)
	}
}

func TestAdviseServerCoreNote(t *testing.T) {
	// servercore runs its own dynamic sim-distance controller — the operator
	// must be warned not to also enable tickwarden's apply mode.
	a := Advise(modSet("servercore"))
	if !containsSubstr(a.Notes, "servercore") || !containsSubstr(a.Notes, "-apply") {
		t.Fatalf("servercore should produce a controller-conflict note mentioning -apply; Notes=%v", a.Notes)
	}
	if containsSubstr(a.Conflicts, "servercore") {
		t.Fatalf("servercore alone is not a jar conflict; Conflicts=%v", a.Conflicts)
	}
}

func TestAdviseVMPNote(t *testing.T) {
	// vmp present: we add a context note rather than treating it as a win.
	a := Advise(modSet("vmp"))
	if !containsSubstr(a.Notes, "vmp") {
		t.Fatalf("vmp should produce a context note; Notes=%v", a.Notes)
	}
	// And it should not itself satisfy any perf-mod suggestion.
	if !containsSubstr(a.Suggestions, "no lithium") {
		t.Fatalf("vmp does not replace lithium; expected the lithium suggestion, Suggestions=%v", a.Suggestions)
	}
}

func TestAdviseForAddsVersionNote(t *testing.T) {
	base := Advise(modSet("starlight", "c2me", "lithium", "ferritecore", "krypton", "spark"))
	withVer := AdviseFor(modSet("starlight", "c2me", "lithium", "ferritecore", "krypton", "spark"), "1.21.4")
	if len(withVer.Notes) != len(base.Notes)+1 {
		t.Fatalf("AdviseFor should add exactly one version note; base=%d withVer=%d", len(base.Notes), len(withVer.Notes))
	}
	if !containsSubstr(withVer.Notes, "1.21.4") {
		t.Fatalf("version note should mention the target version; Notes=%v", withVer.Notes)
	}

	// Empty version string falls back to plain Advise.
	plain := AdviseFor(modSet(), "")
	if len(plain.Notes) != len(Advise(modSet()).Notes) {
		t.Fatal("AdviseFor with empty version should equal plain Advise")
	}
}

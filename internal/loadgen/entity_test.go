package loadgen

import (
	"errors"
	"strings"
	"testing"
)

// recorder is a fake Commander that logs commands and can fail on the Nth.
type recorder struct {
	cmds    []string
	failOn  int // 1-based; 0 = never
	failErr error
}

func (r *recorder) Command(cmd string) (string, error) {
	r.cmds = append(r.cmds, cmd)
	if r.failOn != 0 && len(r.cmds) == r.failOn {
		return "", r.failErr
	}
	return "", nil
}

func TestEntityLoad_Sequence(t *testing.T) {
	r := &recorder{}
	if err := EntityLoad(r, "minecraft:zombie", 0, 70, 0, 3, 0); err != nil {
		t.Fatal(err)
	}
	// gamerule off, forceload add, 3 tagged summons, kill, forceload remove, gamerule on.
	want := []string{
		"gamerule doMobSpawning false",
		"forceload add", "summon", "summon", "summon",
		"kill @e[tag=" + loadTag + "]",
		"forceload remove", "gamerule doMobSpawning true",
	}
	if len(r.cmds) != len(want) {
		t.Fatalf("expected %d commands, got %d: %v", len(want), len(r.cmds), r.cmds)
	}
	for i, w := range want {
		if !strings.HasPrefix(r.cmds[i], w) {
			t.Fatalf("command %d: want prefix %q, got %q", i, w, r.cmds[i])
		}
	}
	for i := 2; i <= 4; i++ {
		if !strings.Contains(r.cmds[i], "minecraft:zombie") || !strings.Contains(r.cmds[i], loadTag) {
			t.Fatalf("command %d should be a tagged zombie summon, got %q", i, r.cmds[i])
		}
	}
}

func TestEntityLoad_DefaultsAndClamp(t *testing.T) {
	r := &recorder{}
	if err := EntityLoad(r, "", 0, 70, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	// count clamped to 1: gamerule off + forceload + 1 summon + kill + remove + gamerule on = 6.
	if len(r.cmds) != 6 {
		t.Fatalf("count<1 should clamp to 1 summon, got %d cmds: %v", len(r.cmds), r.cmds)
	}
	if !strings.Contains(r.cmds[2], "minecraft:villager") {
		t.Fatalf("empty type should default to villager, got %q", r.cmds[2])
	}
}

func TestEntityLoad_AbortsCleanlyOnSummonError(t *testing.T) {
	// Commands: 1 gamerule-off, 2 forceload-add, 3 first-summon. Fail the first
	// summon; cleanup must still run and end by restoring the gamerule.
	r := &recorder{failOn: 3, failErr: errors.New("boom")}
	err := EntityLoad(r, "minecraft:villager", 0, 70, 0, 5, 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	last := r.cmds[len(r.cmds)-1]
	if last != "gamerule doMobSpawning true" {
		t.Fatalf("must restore spawning on abort; last cmd was %q", last)
	}
	joined := strings.Join(r.cmds, "\n")
	if !strings.Contains(joined, "forceload remove ") {
		t.Fatalf("must release forceload on abort; cmds: %v", r.cmds)
	}
}

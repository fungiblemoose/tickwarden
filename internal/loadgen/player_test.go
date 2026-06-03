package loadgen

import (
	"errors"
	"strings"
	"testing"
)

func TestPlayerLoad_SpawnsAndDespawns(t *testing.T) {
	r := &recorder{}
	if err := PlayerLoad(r, 0, 72, 0, 3, 0); err != nil {
		t.Fatal(err)
	}
	// 3 spawns + 3 despawns.
	if len(r.cmds) != 6 {
		t.Fatalf("expected 6 commands, got %d: %v", len(r.cmds), r.cmds)
	}
	for i := 0; i < 3; i++ {
		if !strings.HasPrefix(r.cmds[i], "player twbot") || !strings.Contains(r.cmds[i], "spawn at 0 72 0") {
			t.Fatalf("cmd %d should spawn a bot, got %q", i, r.cmds[i])
		}
	}
	for i := 3; i < 6; i++ {
		if !strings.HasPrefix(r.cmds[i], "player twbot") || !strings.HasSuffix(r.cmds[i], " kill") {
			t.Fatalf("cmd %d should despawn a bot, got %q", i, r.cmds[i])
		}
	}
}

func TestPlayerLoad_ClampsAndCleansUpOnError(t *testing.T) {
	if err := PlayerLoad(&recorder{}, 0, 72, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	// Fail the 2nd spawn; must still attempt to despawn (cleanup) and error.
	r := &recorder{failOn: 2, failErr: errors.New("no carpet")}
	if err := PlayerLoad(r, 0, 72, 0, 4, 0); err == nil {
		t.Fatal("expected an error when a spawn fails")
	}
	if !strings.Contains(strings.Join(r.cmds, "\n"), " kill") {
		t.Fatalf("must attempt despawn cleanup on error; cmds: %v", r.cmds)
	}
}

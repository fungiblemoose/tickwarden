package loadgen

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFlyLoad_TeleportsThroughTerrain(t *testing.T) {
	r := &recorder{}
	if err := FlyLoad(r, 500000, 100, 500000, 64, 3, 0); err != nil {
		t.Fatal(err)
	}
	// spawn, 3 teleports, despawn.
	if len(r.cmds) != 5 {
		t.Fatalf("expected 5 commands, got %d: %v", len(r.cmds), r.cmds)
	}
	if !strings.HasPrefix(r.cmds[0], "player twbot0 spawn") {
		t.Fatalf("first command should spawn the bot, got %q", r.cmds[0])
	}
	for i, x := range []int{500000, 500064, 500128} {
		want := fmt.Sprintf("tp twbot0 %d 100 500000", x)
		if r.cmds[1+i] != want {
			t.Fatalf("teleport %d: want %q, got %q", i, want, r.cmds[1+i])
		}
	}
	if r.cmds[4] != "player twbot0 kill" {
		t.Fatalf("last command should despawn, got %q", r.cmds[4])
	}
}

func TestFlyLoad_DespawnsOnTeleportError(t *testing.T) {
	// Fail the 2nd command (the first teleport); must still despawn.
	r := &recorder{failOn: 2, failErr: errors.New("boom")}
	if err := FlyLoad(r, 0, 100, 0, 64, 5, 0); err == nil {
		t.Fatal("expected an error")
	}
	if r.cmds[len(r.cmds)-1] != "player twbot0 kill" {
		t.Fatalf("must despawn on abort; last cmd was %q", r.cmds[len(r.cmds)-1])
	}
}

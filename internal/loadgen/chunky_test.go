package loadgen

import (
	"errors"
	"reflect"
	"testing"
)

// recordingCommander is a fake Commander that records every command in order
// and can be told to fail on the Nth call, letting us assert both the exact
// command sequence and the abort-on-error behavior without a socket.
type recordingCommander struct {
	got     []string
	failOn  int // 1-based index of the call that should error; 0 = never
	failErr error
}

func (r *recordingCommander) Command(cmd string) (string, error) {
	r.got = append(r.got, cmd)
	if r.failOn != 0 && len(r.got) == r.failOn {
		return "", r.failErr
	}
	return "ok", nil
}

func TestChunkyBurst_OrderAndBlockRadius(t *testing.T) {
	r := &recordingCommander{}
	// radiusChunks=8 must become 8*16=128 BLOCKS in the radius command.
	if err := ChunkyBurst(r, "minecraft:the_nether", 5000, -3000, 8, 0); err != nil {
		t.Fatalf("ChunkyBurst: %v", err)
	}
	want := []string{
		"chunky world minecraft:the_nether",
		"chunky center 5000 -3000",
		"chunky radius 128",
		"chunky start",
		"chunky cancel",
	}
	if !reflect.DeepEqual(r.got, want) {
		t.Fatalf("command sequence mismatch:\n got  %q\n want %q", r.got, want)
	}
}

func TestChunkyBurst_DefaultWorld(t *testing.T) {
	r := &recordingCommander{}
	if err := ChunkyBurst(r, "", 0, 0, 1, 0); err != nil {
		t.Fatalf("ChunkyBurst: %v", err)
	}
	if r.got[0] != "chunky world minecraft:overworld" {
		t.Fatalf("empty world should default to overworld, got %q", r.got[0])
	}
	if r.got[2] != "chunky radius 16" {
		t.Fatalf("radius 1 chunk should be 16 blocks, got %q", r.got[2])
	}
}

func TestChunkyBurst_AbortsOnError(t *testing.T) {
	sentinel := errors.New("rcon boom")
	// Fail on the 3rd command (chunky radius); start/cancel must NOT be sent.
	r := &recordingCommander{failOn: 3, failErr: sentinel}
	err := ChunkyBurst(r, "minecraft:overworld", 1, 2, 4, 0)
	if err == nil {
		t.Fatal("expected ChunkyBurst to return the command error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error chain should wrap sentinel, got %v", err)
	}
	want := []string{
		"chunky world minecraft:overworld",
		"chunky center 1 2",
		"chunky radius 64",
	}
	if !reflect.DeepEqual(r.got, want) {
		t.Fatalf("should abort after failing command:\n got  %q\n want %q", r.got, want)
	}
}

package pregen

import (
	"context"
	"errors"
	"testing"
	"time"
)

// recordingCmd is a fake loadgen.Commander that records every command issued,
// in order, so a test can assert the exact pause/continue sequence without an
// RCON socket or a live server. failOn (1-based) makes the Nth call return an
// error, exercising the send-failure path.
type recordingCmd struct {
	got     []string
	failOn  int
	failErr error
}

func (r *recordingCmd) Command(cmd string) (string, error) {
	r.got = append(r.got, cmd)
	if r.failOn != 0 && len(r.got) == r.failOn {
		return "", r.failErr
	}
	return "ok", nil
}

func (r *recordingCmd) last() string {
	if len(r.got) == 0 {
		return ""
	}
	return r.got[len(r.got)-1]
}

// fakeLoad is an injectable LoadSampler whose verdict the test controls.
type fakeLoad struct {
	ok     bool
	reason string
}

func (f fakeLoad) Headroom() (bool, string) { return f.ok, f.reason }

// fakePlayers is an injectable PlayerSource returning a fixed count or error.
type fakePlayers struct {
	n   int
	err error
}

func (f fakePlayers) Online() (int, error) { return f.n, f.err }

// idleController returns a Controller wired to "safe to run" signals: zero
// players, healthy TPS, and headroom. Tests then mutate the individual fakes.
func idleController(cmd *recordingCmd) (*Controller, *fakePlayers, *fakeLoad, *float64, *error) {
	players := &fakePlayers{n: 0}
	load := &fakeLoad{ok: true}
	tps := 20.0
	var tpsErr error
	c := &Controller{
		Cmd:      cmd,
		Load:     load,
		Players:  players,
		TPS:      func() (float64, error) { return tps, tpsErr },
		TPSFloor: 19.0,
		Poll:     time.Millisecond,
	}
	// Return pointers to the captured locals so callers can mutate the signals
	// the TPS closure reads on each call.
	return c, players, load, &tps, &tpsErr
}

func TestStep_IdleWithHeadroom_Continues(t *testing.T) {
	cmd := &recordingCmd{}
	c, _, _, _, _ := idleController(cmd)

	act, err := c.Step()
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if act != ActionContinue {
		t.Fatalf("idle+headroom should continue, got %q", act)
	}
	if cmd.last() != cmdContinue {
		t.Fatalf("expected %q command, got %q", cmdContinue, cmd.got)
	}
}

func TestStep_PlayerJoins_Pauses(t *testing.T) {
	cmd := &recordingCmd{}
	c, players, _, _, _ := idleController(cmd)

	// First step under idle conditions starts the task running.
	if act, err := c.Step(); err != nil || act != ActionContinue {
		t.Fatalf("first step: act=%q err=%v", act, err)
	}

	// A player joins: the very next step must pause.
	players.n = 1
	act, err := c.Step()
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if act != ActionPause {
		t.Fatalf("player online should pause, got %q", act)
	}
	if cmd.last() != cmdPause {
		t.Fatalf("expected %q command, got %q", cmdPause, cmd.got)
	}
}

func TestStep_TPSBelowFloor_Pauses(t *testing.T) {
	cmd := &recordingCmd{}
	c, _, _, tps, _ := idleController(cmd)

	if _, err := c.Step(); err != nil { // start running
		t.Fatalf("first step: %v", err)
	}
	*tps = 18.5 // below 19.0 floor
	act, err := c.Step()
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if act != ActionPause {
		t.Fatalf("low TPS should pause, got %q", act)
	}
}

func TestStep_NoHeadroom_Pauses(t *testing.T) {
	cmd := &recordingCmd{}
	c, _, load, _, _ := idleController(cmd)

	if _, err := c.Step(); err != nil { // start running
		t.Fatalf("first step: %v", err)
	}
	load.ok = false
	load.reason = "io pressure"
	act, err := c.Step()
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if act != ActionPause {
		t.Fatalf("no headroom should pause, got %q", act)
	}
}

func TestStep_NoDuplicateCommands_WhenStateUnchanged(t *testing.T) {
	cmd := &recordingCmd{}
	c, _, _, _, _ := idleController(cmd)

	// Three steps under stable idle conditions: only ONE continue should be sent.
	for i := 0; i < 3; i++ {
		act, err := c.Step()
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if i == 0 && act != ActionContinue {
			t.Fatalf("step 0 should continue, got %q", act)
		}
		if i > 0 && act != ActionHold {
			t.Fatalf("step %d should hold (no transition), got %q", i, act)
		}
	}
	if len(cmd.got) != 1 || cmd.got[0] != cmdContinue {
		t.Fatalf("expected exactly one %q command, got %q", cmdContinue, cmd.got)
	}
}

func TestStep_FirstStepEstablishesPauseExplicitly(t *testing.T) {
	cmd := &recordingCmd{}
	c, players, _, _, _ := idleController(cmd)
	players.n = 2 // not safe from the very start

	act, err := c.Step()
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	// Even though the zero-value running state is already "paused", the first
	// step must emit an explicit pause to establish a known state.
	if act != ActionPause {
		t.Fatalf("first step under load should explicitly pause, got %q", act)
	}
	if cmd.last() != cmdPause {
		t.Fatalf("expected %q, got %q", cmdPause, cmd.got)
	}
}

func TestStep_PlayerReadError_Pauses(t *testing.T) {
	cmd := &recordingCmd{}
	c, players, _, _, _ := idleController(cmd)

	if _, err := c.Step(); err != nil { // start running
		t.Fatalf("first step: %v", err)
	}
	players.err = errors.New("rcon timeout")
	act, err := c.Step()
	if err != nil {
		t.Fatalf("Step should not propagate a read error: %v", err)
	}
	if act != ActionPause {
		t.Fatalf("player read error must be treated as unsafe → pause, got %q", act)
	}
}

func TestStep_TPSReadError_Pauses(t *testing.T) {
	cmd := &recordingCmd{}
	c, _, _, _, tpsErr := idleController(cmd)

	if _, err := c.Step(); err != nil { // start running
		t.Fatalf("first step: %v", err)
	}
	*tpsErr = errors.New("spark unreachable")
	act, err := c.Step()
	if err != nil {
		t.Fatalf("Step should not propagate a read error: %v", err)
	}
	if act != ActionPause {
		t.Fatalf("TPS read error must be treated as unsafe → pause, got %q", act)
	}
}

func TestStep_CommandSendFailure_RetriesNextStep(t *testing.T) {
	// Fail the first command send. Step must surface the error AND leave the
	// tracked state unadvanced, so the next Step retries the SAME transition.
	sentinel := errors.New("rcon boom")
	cmd := &recordingCmd{failOn: 1, failErr: sentinel}
	c, _, _, _, _ := idleController(cmd)

	act, err := c.Step()
	if err == nil {
		t.Fatalf("expected error on failed command send")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error should wrap the sentinel, got %v", err)
	}
	if act != "" {
		t.Fatalf("failed send should return empty action, got %q", act)
	}

	// Retry: same continue transition, now succeeds.
	act, err = c.Step()
	if err != nil {
		t.Fatalf("retry Step: %v", err)
	}
	if act != ActionContinue {
		t.Fatalf("retry should re-attempt continue, got %q", act)
	}
	want := []string{cmdContinue, cmdContinue}
	if len(cmd.got) != 2 || cmd.got[0] != want[0] || cmd.got[1] != want[1] {
		t.Fatalf("expected two %q sends (fail then retry), got %q", cmdContinue, cmd.got)
	}
}

func TestRun_RequiresPositivePoll(t *testing.T) {
	c := &Controller{Cmd: &recordingCmd{}, Load: fakeLoad{ok: true}, Players: fakePlayers{}, TPS: func() (float64, error) { return 20, nil }}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("Run with zero Poll should error")
	}
}

func TestRun_PausesOnExit(t *testing.T) {
	cmd := &recordingCmd{}
	c, _, _, _, _ := idleController(cmd)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Give the immediate first Step time to run (it will issue continue).
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run should return context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// Regardless of intermediate state, the LAST command must be a pause:
	// leaving generation running after the controller dies is the exact failure
	// mode this package prevents.
	if cmd.last() != cmdPause {
		t.Fatalf("Run must pause Chunky on exit; last command was %q (all: %q)", cmd.last(), cmd.got)
	}
}

func TestRun_ContextDeadline_StillPauses(t *testing.T) {
	cmd := &recordingCmd{}
	c, players, _, _, _ := idleController(cmd)
	players.n = 5 // stays paused throughout

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if cmd.last() != cmdPause {
		t.Fatalf("expected pause on exit, got %q", cmd.got)
	}
}

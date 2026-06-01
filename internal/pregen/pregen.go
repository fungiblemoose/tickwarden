// Package pregen is a host-load-aware controller for the Chunky world
// pregenerator. Pregenerating a world up front makes later play cheap — chunks
// are already on disk, so no terrain GENERATION (the heaviest server-tick
// workload) happens while real players are online. But that up-front
// generation is itself brutally heavy, so running it blindly would starve the
// very players it's meant to help and trip the host pressure that
// internal/observe exists to catch.
//
// The controller resolves that tension by treating pregen as strictly
// opportunistic background work: it runs Chunky ONLY when the node has genuine
// headroom, and it PAUSES the instant anyone joins, TPS dips, or host pressure
// spikes — then RESUMES once the node is idle again. Chunky's own
// `chunky pause` / `chunky continue` console commands make this cheap: a paused
// task keeps its progress, so we can stop and start freely without losing work.
//
// Testability is by construction. Every input is an injected interface
// (LoadSampler, PlayerSource, a TPS func) and the Chunky control surface is the
// same loadgen.Commander used elsewhere, so the entire decision logic is
// exercised with in-memory fakes — no socket, no live server, no real clock in
// the per-step logic.
package pregen

import (
	"context"
	"fmt"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/loadgen"
)

// LoadSampler reports whether the node currently has headroom for heavy
// background work. ok==false means the node is under pressure or otherwise
// busy and pregen must back off; reason is a short human-readable explanation
// for logging. This is the seam where a host agent or cgroup-v2 PSI reading
// (internal/observe.ReadPressure) plugs in.
type LoadSampler interface {
	Headroom() (ok bool, reason string)
}

// PlayerSource reports the number of players currently online. Any non-zero
// count means real users are on the server and pregen must yield to them.
type PlayerSource interface {
	Online() (int, error)
}

// Chunky control commands. A paused task retains its progress, so toggling
// between these is free of lost work — which is what makes aggressive
// pause/resume the right strategy rather than start/cancel.
const (
	cmdContinue = "chunky continue"
	cmdPause    = "chunky pause"
)

// Controller decides, on each Step, whether the Chunky pregeneration task
// should be RUNNING or PAUSED given live player, TPS, and host-load signals,
// and issues the corresponding Chunky command only when that desired state
// changes (so it never spams a busy RCON channel).
type Controller struct {
	// Cmd is the Chunky control surface. The same loadgen.Commander that the
	// load generator uses to drive bursts; here we issue continue/pause.
	Cmd loadgen.Commander
	// Load reports host headroom. Required.
	Load LoadSampler
	// Players reports online player count. Required.
	Players PlayerSource
	// TPS reports current server ticks-per-second. Required.
	TPS func() (float64, error)
	// TPSFloor is the minimum healthy TPS; below it, pregen pauses. A Minecraft
	// server targets 20 TPS, so a floor around 19.0–19.5 leaves real headroom.
	TPSFloor float64
	// Poll is the interval between Step calls in Run.
	Poll time.Duration
	// OnStep, if set, is invoked after every Step with its result. It is the
	// only channel by which the otherwise side-effect-light loop reports
	// per-tick outcomes. Injecting logging (rather than calling a logger
	// directly) keeps the decision logic free of I/O and keeps tests silent.
	OnStep func(action string, err error)

	// running tracks the last state we COMMANDED, so we only send a command on
	// a transition. The zero value (false) means "we have not yet told Chunky
	// to run", which correctly matches a freshly added-but-paused Chunky task:
	// the first Step that finds headroom will issue the initial continue.
	running bool
	// started guards the very first Step so that even when the desired state is
	// "paused" we emit an explicit pause once, rather than assuming Chunky is
	// already idle. Establishing a known state up front is cheaper than
	// debugging a phantom-running task.
	started bool
}

// Action strings returned by Step. "hold" means no transition occurred and no
// command was sent.
const (
	ActionContinue = "continue"
	ActionPause    = "pause"
	ActionHold     = "hold"
)

// Step makes ONE pregen decision and, if the desired running state differs
// from the last commanded state, issues the matching Chunky command.
//
// Desired state is RUN only when ALL of these hold: zero players online, TPS at
// or above the floor, and the host reporting headroom. Otherwise the desired
// state is PAUSED.
//
// Conservative-on-uncertainty is the governing principle: a controller that
// runs heavy generation when it CAN'T confirm the node is idle is worse than
// useless — it can starve players or amplify host pressure precisely when
// signals are flaky. So any error reading players, TPS, or load is treated as
// "not safe" and forces the desired state to PAUSED. We would rather lose some
// background-generation throughput than ever generate on top of real load.
//
// The returned action is "continue" or "pause" when a command was sent, or
// "hold" when the desired state already matched the current state. When a
// command send fails, Step returns the underlying error AND does not advance
// the tracked state, so the next Step retries the same transition.
func (c *Controller) Step() (action string, err error) {
	desired, reason := c.desiredRun()

	// First Step always establishes a known state explicitly, even if that
	// state is the zero-value "paused" — see the started field doc.
	if c.started && desired == c.running {
		return ActionHold, nil
	}

	cmd := cmdPause
	act := ActionPause
	if desired {
		cmd = cmdContinue
		act = ActionContinue
	}

	if _, cerr := c.Cmd.Command(cmd); cerr != nil {
		// Leave running/started untouched so the next Step retries this same
		// transition rather than silently assuming it took effect.
		return "", fmt.Errorf("pregen: %q failed (wanted run=%v: %s): %w", cmd, desired, reason, cerr)
	}
	c.running = desired
	c.started = true
	return act, nil
}

// desiredRun computes whether pregen SHOULD be running right now, returning a
// short reason for logging/diagnostics. It reads players, then TPS, then load,
// short-circuiting on the first signal that says "not safe" so an early failing
// or unsafe signal isn't masked by a later healthy one. Every read error maps
// to "do not run".
func (c *Controller) desiredRun() (run bool, reason string) {
	n, err := c.Players.Online()
	if err != nil {
		return false, fmt.Sprintf("players read error: %v", err)
	}
	if n > 0 {
		return false, fmt.Sprintf("%d player(s) online", n)
	}

	tps, err := c.TPS()
	if err != nil {
		return false, fmt.Sprintf("tps read error: %v", err)
	}
	if tps < c.TPSFloor {
		return false, fmt.Sprintf("tps %.2f below floor %.2f", tps, c.TPSFloor)
	}

	ok, lreason := c.Load.Headroom()
	if !ok {
		if lreason == "" {
			lreason = "no headroom"
		}
		return false, "host load: " + lreason
	}

	return true, "idle with headroom"
}

// Run drives Step every Poll until ctx is cancelled. On exit it makes a
// best-effort attempt to PAUSE Chunky regardless of the current tracked state:
// leaving a heavy generation task running after the controller dies is exactly
// the unattended-load failure mode this package exists to prevent, so the
// shutdown pause is unconditional. Run returns ctx.Err() on normal cancellation
// (Step errors during the loop are non-fatal and are surfaced by the caller's
// own logging via OnStep, if set).
func (c *Controller) Run(ctx context.Context) error {
	if c.Poll <= 0 {
		return fmt.Errorf("pregen: Poll must be > 0")
	}
	defer c.pauseOnExit()

	t := time.NewTicker(c.Poll)
	defer t.Stop()

	// Make an immediate first decision rather than idling for one Poll, so the
	// controller establishes a known state the moment it starts.
	c.report(c.Step())

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			c.report(c.Step())
		}
	}
}

// report forwards a Step result to OnStep when configured.
func (c *Controller) report(action string, err error) {
	if c.OnStep != nil {
		c.OnStep(action, err)
	}
}

// pauseOnExit unconditionally tells Chunky to pause on shutdown. Errors are
// ignored: if the command channel is already gone there is nothing more we can
// do, and the alternative (propagating it past a cancelled context) buys
// nothing.
func (c *Controller) pauseOnExit() {
	_, _ = c.Cmd.Command(cmdPause)
	c.running = false
	c.started = true
}

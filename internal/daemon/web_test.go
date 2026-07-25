package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/observe"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestUI_PageAndStatus(t *testing.T) {
	store := NewStore(DefaultConfig().knobs())
	store.SetStatus(Status{OK: true, Snapshot: observe.Snapshot{TPS: 20, MSPT: 12, Sim: 10, View: 14}})
	srv := httptest.NewServer(uiHandler(store, "", testLogger()))
	defer srv.Close()

	page, err := http.Get(srv.URL + "/")
	if err != nil || page.StatusCode != 200 {
		t.Fatalf("GET / failed: %v status=%v", err, page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), "tickwarden") || !strings.Contains(string(body), "/api/status") {
		t.Fatal("control page should be the self-contained dashboard")
	}

	st, err := http.Get(srv.URL + "/api/status")
	if err != nil || st.StatusCode != 200 {
		t.Fatalf("GET /api/status failed: %v", err)
	}
	jb, _ := io.ReadAll(st.Body)
	st.Body.Close()
	for _, want := range []string{`"tps":20`, `"target_mspt":35`, `"min_sim":6`} {
		if !strings.Contains(string(jb), want) {
			t.Fatalf("status JSON missing %q: %s", want, jb)
		}
	}
}

func TestUI_ConfigPatchAndValidation(t *testing.T) {
	store := NewStore(DefaultConfig().knobs())
	srv := httptest.NewServer(uiHandler(store, "", testLogger()))
	defer srv.Close()

	// postConfig mutates through the hardened path: JSON Content-Type plus the
	// X-Tickwarden-Token CSRF header, which is required even on a tokenless
	// loopback bind.
	postConfig := func(body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/api/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tickwarden-Token", "loopback")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /api/config: %v", err)
		}
		return resp
	}

	// Partial patch: only target_mspt changes, everything else keeps its value.
	resp := postConfig(`{"target_mspt": 30}`)
	if resp.StatusCode != 200 {
		t.Fatalf("valid patch should succeed: status=%v", resp.StatusCode)
	}
	resp.Body.Close()
	if k := store.Knobs(); k.TargetMSPT != 30 || k.MinSim != 6 {
		t.Fatalf("patch should change only target_mspt, got %+v", k)
	}

	// A mutation without the CSRF header is refused even on loopback.
	noCSRF, _ := http.Post(srv.URL+"/api/config", "application/json", strings.NewReader(`{"target_mspt": 25}`))
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF header should 403, got %v", noCSRF.StatusCode)
	}
	noCSRF.Body.Close()

	// min_sim > max_sim must be rejected and leave the knobs untouched.
	resp = postConfig(`{"min_sim": 20, "max_sim": 8}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid bounds should 400, got %v", resp.StatusCode)
	}
	resp.Body.Close()
	if k := store.Knobs(); k.MinSim != 6 || k.MaxSim != 16 {
		t.Fatalf("rejected patch must not change knobs, got %+v", k)
	}

	// GET on the mutating endpoint is refused.
	resp, _ = http.Get(srv.URL + "/api/config")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/config should be 405, got %v", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUI_TokenAuth(t *testing.T) {
	store := NewStore(DefaultConfig().knobs())
	srv := httptest.NewServer(uiHandler(store, "s3cret", testLogger()))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/status")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token should be 401, got %v", resp.StatusCode)
	}
	resp.Body.Close()

	// The URL query string is no longer accepted — header only.
	resp, _ = http.Get(srv.URL + "/api/status?token=s3cret")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token must be rejected, got %v", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Header.Set("X-Tickwarden-Token", "s3cret")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("header token should pass, got %v", resp.StatusCode)
	}
	resp.Body.Close()
}

// A non-loopback bind without a token is a misconfiguration serveUI must refuse:
// /api/config changes a live game server.
func TestServeUI_RefusesNonLoopbackWithoutToken(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UIAddr = "0.0.0.0:0"
	err := serveUI(context.Background(), cfg, NewStore(cfg.knobs()), testLogger())
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected a token-refusal error, got %v", err)
	}

	// With a token it binds fine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.UIToken = "s3cret"
	if err := serveUI(ctx, cfg, NewStore(cfg.knobs()), testLogger()); err != nil {
		t.Fatalf("tokened non-loopback bind should work: %v", err)
	}
}

// A knob change through the store takes effect on the very next loop step —
// the whole point of deriving the adaptive config per step.
func TestStep_KnobChangeTakesEffect(t *testing.T) {
	fa := &fakeApply{}
	// MSPT 30 sits inside the default [28, 35] band → hold, no apply.
	snap := observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 30, Sim: 14, View: 18, TPS: 20}
	l := newLoop(Config{Apply: true}, snap, nil, fa.apply)

	l.step()
	if fa.count() != 0 {
		t.Fatalf("within-band step should hold, got %d applies", fa.count())
	}

	// Operator drops the target to 20 via the control plane: 30 is now over
	// budget and the next step must lower.
	k := l.store.Knobs()
	k.TargetMSPT = 20
	l.store.SetKnobs(k)

	l.step()
	if fa.count() != 1 || fa.lastSim >= 14 {
		t.Fatalf("knob change should lower on the next step, got %d applies (sim %d)", fa.count(), fa.lastSim)
	}
}

// The loop publishes its observations and decisions to the store for the
// control plane, collapsing repeated identical holds into one record.
func TestStep_PublishesStatusAndDedupesHolds(t *testing.T) {
	fa := &fakeApply{}
	snap := observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 30, Sim: 14, View: 18, TPS: 20}
	l := newLoop(Config{}, snap, nil, fa.apply)

	for i := 0; i < 5; i++ {
		l.step()
	}
	st := l.store.Status()
	if !st.OK || st.Snapshot.MSPT != 30 {
		t.Fatalf("status should carry the last snapshot, got %+v", st)
	}
	if n := len(l.store.Decisions()); n != 1 {
		t.Fatalf("5 identical holds should collapse to 1 record, got %d", n)
	}
}

// GC dominating the poll window → hold, not lower; once the collector goes
// quiet the same MSPT lowers normally.
func TestStep_GCStallHoldsThenRecovers(t *testing.T) {
	fa := &fakeApply{}
	snap := observe.Snapshot{Players: 4, PlayersPeak: 4, MSPT: 48, Sim: 14, View: 18, TPS: 19}
	l := newLoop(Config{Apply: true}, snap, nil, fa.apply)
	l.cfg.Interval = 10 * time.Second

	gcTime := uint64(1000)
	grow := uint64(3000) // 3s of GC per 10s window = 30%, over the 15% default
	l.deps.JVM = func() (observe.JVMStats, error) {
		gcTime += grow
		return observe.JVMStats{GCTimeMs: gcTime, GCCount: 1, HeapMax: 1, Available: true}, nil
	}

	// First step seeds the counters (prev == cur, delta zero) → normal lower.
	l.step()
	if fa.count() != 1 {
		t.Fatalf("first step has no GC delta yet and should lower, got %d applies", fa.count())
	}

	// Second step sees a 3s GC delta → 30%% of the window → hold.
	l.step()
	if fa.count() != 1 {
		t.Fatalf("GC-stalled step must hold, got %d applies", fa.count())
	}

	// Collector goes quiet → delta 0 → the over-budget MSPT lowers again.
	grow = 0
	l.step()
	if fa.count() != 2 {
		t.Fatalf("after GC settles the lower should apply, got %d applies", fa.count())
	}
}

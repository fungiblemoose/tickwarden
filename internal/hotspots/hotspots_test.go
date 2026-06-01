package hotspots

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleJSON = `[
  {"dimension":"minecraft:overworld","x":12,"z":-3,"block_entities":120,"entities":4},
  {"dimension":"minecraft:overworld","x":412,"z":-88,"block_entities":3900,"entities":12},
  {"dimension":"minecraft:the_nether","x":0,"z":0,"block_entities":3900,"entities":2},
  {"dimension":"minecraft:overworld","x":7,"z":7,"block_entities":15}
]`

func fakeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestFetchRanksByBlockEntities(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, sampleJSON)
	defer srv.Close()

	hs, err := Fetch(srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(hs) != 4 {
		t.Fatalf("want 4 hotspots, got %d", len(hs))
	}

	// Two chunks tie at 3900; tie broken by entities desc -> overworld(412,-88)
	// with 12 entities should come before the_nether(0,0) with 2.
	if hs[0].BlockEntities != 3900 {
		t.Errorf("top hotspot should have 3900 block entities, got %d", hs[0].BlockEntities)
	}
	if hs[0].Dimension != "minecraft:overworld" || hs[0].X != 412 || hs[0].Z != -88 {
		t.Errorf("top hotspot mis-ranked: %+v", hs[0])
	}
	if hs[1].Dimension != "minecraft:the_nether" || hs[1].Entities != 2 {
		t.Errorf("second hotspot should be the_nether tie: %+v", hs[1])
	}
	// Lowest count last; missing "entities" field defaults to 0.
	last := hs[len(hs)-1]
	if last.BlockEntities != 15 || last.Entities != 0 {
		t.Errorf("last hotspot wrong: %+v", last)
	}
}

func TestFetchNon200(t *testing.T) {
	srv := fakeServer(t, http.StatusInternalServerError, "boom")
	defer srv.Close()
	if _, err := Fetch(srv.URL); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestFetchBadJSON(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, "not json")
	defer srv.Close()
	if _, err := Fetch(srv.URL); err == nil {
		t.Fatal("expected decode error on malformed JSON")
	}
}

func TestFetchUnreachable(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, sampleJSON)
	srv.Close() // closed immediately -> connection refused
	if _, err := Fetch(srv.URL); err == nil {
		t.Fatal("expected error hitting closed server")
	}
}

func TestReportContent(t *testing.T) {
	hs := []Hotspot{
		{Dimension: "minecraft:overworld", X: 412, Z: -88, BlockEntities: 3900, Entities: 12},
		{Dimension: "minecraft:overworld", X: 7, Z: 7, BlockEntities: 15},
	}
	out := Report(hs)

	for _, want := range []string{
		"chunk (412,-88) in minecraft:overworld: 3900 block entities",
		"12 entities", // entities shown when > 0
		"chunk (7,7)", // second entry rendered
		"hopper",      // the explanatory hint
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Report output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "(7,7) in minecraft:overworld: 15 block entities, ") {
		t.Errorf("entities suffix should be omitted when 0:\n%s", out)
	}
}

func TestReportEmpty(t *testing.T) {
	out := Report(nil)
	if !strings.Contains(out, "No loaded chunks") {
		t.Errorf("empty report should explain the empty case:\n%s", out)
	}
	if !strings.Contains(out, "0.2.0") {
		t.Errorf("empty report should hint at companion version:\n%s", out)
	}
}

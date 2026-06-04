package observe

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParsePSI(t *testing.T) {
	tests := []struct {
		name             string
		in               string
		wantSome, wantFl float64
	}{
		{
			name:     "some and full",
			in:       "some avg10=12.34 avg60=1.00 avg300=0.00 total=999\nfull avg10=5.67 avg60=0.00 avg300=0.00 total=10\n",
			wantSome: 12.34,
			wantFl:   5.67,
		},
		{
			name:     "some only (cpu has no full)",
			in:       "some avg10=0.50 avg60=0.10 avg300=0.00 total=5\n",
			wantSome: 0.50,
			wantFl:   0,
		},
		{name: "empty", in: "", wantSome: 0, wantFl: 0},
		{
			name:     "missing avg10 key",
			in:       "some avg60=1.00 total=5\n",
			wantSome: 0,
			wantFl:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := parsePSI(tt.in)
			if p.SomeAvg10 != tt.wantSome || p.FullAvg10 != tt.wantFl {
				t.Fatalf("parsePSI = %+v, want some=%v full=%v", p, tt.wantSome, tt.wantFl)
			}
		})
	}
}

func TestParseCPUStat(t *testing.T) {
	tests := []struct {
		name             string
		in               string
		wantNr, wantUsec uint64
	}{
		{
			name:     "throttle counters present",
			in:       "usage_usec 123\nnr_periods 100\nnr_throttled 7\nthrottled_usec 4567\n",
			wantNr:   7,
			wantUsec: 4567,
		},
		{
			name:     "no throttling",
			in:       "usage_usec 1\nnr_periods 1\n",
			wantNr:   0,
			wantUsec: 0,
		},
		{name: "empty", in: "", wantNr: 0, wantUsec: 0},
		{
			name:     "malformed columns skipped",
			in:       "nr_throttled\nthrottled_usec 88\n",
			wantNr:   0,
			wantUsec: 88,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nr, usec := parseCPUStat(tt.in)
			if nr != tt.wantNr || usec != tt.wantUsec {
				t.Fatalf("parseCPUStat = (%d, %d), want (%d, %d)", nr, usec, tt.wantNr, tt.wantUsec)
			}
		})
	}
}

func TestParseKV(t *testing.T) {
	fields := []string{"some", "avg10=3.14", "avg60=0.00"}
	if got := parseKV(fields, "avg10"); got != 3.14 {
		t.Errorf("parseKV avg10 = %v", got)
	}
	if got := parseKV(fields, "avg300"); got != 0 {
		t.Errorf("parseKV missing key = %v, want 0", got)
	}
}

func TestCgroupRel(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{name: "unified", contents: "0::/lxc/101\n", want: "/lxc/101"},
		{name: "v1 ignored", contents: "5:cpu:/x\n0::/leaf\n", want: "/leaf"},
		{name: "fallback when absent", contents: "1:name=systemd:/y\n", want: "/"},
		{name: "empty fallback", contents: "", want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cgroupRel(tt.contents, "/"); got != tt.want {
				t.Fatalf("cgroupRel = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCgroupLevelsFrom(t *testing.T) {
	const base = "/sys/fs/cgroup"
	tests := []struct {
		name string
		rel  string
		want []string
	}{
		{
			name: "nested leaf walks to root",
			rel:  "/lxc/101",
			want: []string{
				filepath.Join(base, "/lxc/101"),
				filepath.Join(base, "/lxc"),
				base,
			},
		},
		{
			name: "root only",
			rel:  "/",
			want: []string{base},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cgroupLevelsFrom(base, tt.rel)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("cgroupLevelsFrom = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- HTTP Fetch* tests --------------------------------------------------

func newServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSnapshot(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		srv := newServer(t, 200, `{"tps":19.5,"mspt":52.1,"players":3,"players_peak":8,"view":10,"sim":8}`)
		got, err := FetchSnapshot(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		want := Snapshot{TPS: 19.5, MSPT: 52.1, Players: 3, PlayersPeak: 8, View: 10, Sim: 8}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		srv := newServer(t, 200, `{not json`)
		if _, err := FetchSnapshot(srv.URL); err == nil {
			t.Fatal("expected error on malformed json")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := newServer(t, 500, `{}`)
		if _, err := FetchSnapshot(srv.URL); err == nil {
			t.Fatal("expected error on 500")
		}
	})
	t.Run("connection error", func(t *testing.T) {
		if _, err := FetchSnapshot("http://127.0.0.1:0"); err == nil {
			t.Fatal("expected error on bad url")
		}
	})
}

func TestFetchPlayers(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		srv := newServer(t, 200, `{"players":5}`)
		got, err := FetchPlayers(srv.URL)
		if err != nil || got != 5 {
			t.Fatalf("got %d, err %v", got, err)
		}
	})
	t.Run("missing field", func(t *testing.T) {
		srv := newServer(t, 200, `{"tps":20}`)
		if _, err := FetchPlayers(srv.URL); err == nil {
			t.Fatal("expected error when players absent")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		srv := newServer(t, 200, `xxx`)
		if _, err := FetchPlayers(srv.URL); err == nil {
			t.Fatal("expected error on malformed json")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := newServer(t, 404, ``)
		if _, err := FetchPlayers(srv.URL); err == nil {
			t.Fatal("expected error on 404")
		}
	})
}

func TestFetchPlayerPeak(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		srv := newServer(t, 200, `{"players_peak":12}`)
		got, err := FetchPlayerPeak(srv.URL)
		if err != nil || got != 12 {
			t.Fatalf("got %d, err %v", got, err)
		}
	})
	t.Run("missing field", func(t *testing.T) {
		srv := newServer(t, 200, `{"players":1}`)
		if _, err := FetchPlayerPeak(srv.URL); err == nil {
			t.Fatal("expected error when players_peak absent")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		srv := newServer(t, 200, `nope`)
		if _, err := FetchPlayerPeak(srv.URL); err == nil {
			t.Fatal("expected error on malformed json")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := newServer(t, 503, ``)
		if _, err := FetchPlayerPeak(srv.URL); err == nil {
			t.Fatal("expected error on 503")
		}
	})
}

func TestSparkHTTPReader(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		srv := newServer(t, 200, `{"tps":20,"mspt":50}`)
		r := NewSparkHTTPReader(srv.URL)
		tps, mspt, err := r.Read()
		if err != nil || tps != 20 || mspt != 50 {
			t.Fatalf("got tps=%v mspt=%v err=%v", tps, mspt, err)
		}
		if r.Name() == "" {
			t.Fatal("Name should be non-empty")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := newServer(t, 500, ``)
		if _, _, err := NewSparkHTTPReader(srv.URL).Read(); err == nil {
			t.Fatal("expected error on 500")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		srv := newServer(t, 200, `{bad`)
		if _, _, err := NewSparkHTTPReader(srv.URL).Read(); err == nil {
			t.Fatal("expected error on malformed json")
		}
	})
}

func TestStubReader(t *testing.T) {
	var s StubReader
	tps, mspt, err := s.Read()
	if err != nil || tps != 20.0 || mspt != 50.0 {
		t.Fatalf("got tps=%v mspt=%v err=%v", tps, mspt, err)
	}
	if s.Name() != "stub" {
		t.Fatal("Name mismatch")
	}
}

func TestReadPressure_Smoke(t *testing.T) {
	// Runs against the real host. We only assert it doesn't panic and returns a
	// value; PSI may or may not be available depending on the platform.
	_ = ReadPressure()
}

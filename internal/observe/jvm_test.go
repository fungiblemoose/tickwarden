package observe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func jvmAt(gcTimeMs uint64) JVMStats {
	return JVMStats{GCTimeMs: gcTimeMs, GCCount: 1, HeapUsed: 1, HeapMax: 2, Available: true}
}

func TestGCStalledNow_OverThreshold(t *testing.T) {
	// 3s of GC inside a 10s window = 30%, over a 15% threshold.
	stalled, detail := GCStalledNow(jvmAt(1000), jvmAt(4000), 10*time.Second, 15)
	if !stalled {
		t.Fatal("30% GC share should stall")
	}
	if !strings.Contains(detail, "3000ms") || !strings.Contains(detail, "30%") {
		t.Fatalf("detail should carry the delta and fraction: %q", detail)
	}
}

func TestGCStalledNow_UnderThreshold(t *testing.T) {
	// 0.5s in 10s = 5% — normal collector housekeeping, not a stall.
	if stalled, _ := GCStalledNow(jvmAt(1000), jvmAt(1500), 10*time.Second, 15); stalled {
		t.Fatal("5% GC share must not stall")
	}
}

// A counter that went BACKWARDS means the JVM restarted: the delta is
// meaningless and must not stall.
func TestGCStalledNow_CounterResetIsNotAStall(t *testing.T) {
	if stalled, _ := GCStalledNow(jvmAt(900000), jvmAt(100), 10*time.Second, 15); stalled {
		t.Fatal("a JVM restart (counter reset) must not read as a stall")
	}
}

// First poll passes prev == cur: delta zero, never a stall. And an unavailable
// reading (companion without /jvm) never stalls either.
func TestGCStalledNow_FirstPollAndUnavailable(t *testing.T) {
	cur := jvmAt(50000)
	if stalled, _ := GCStalledNow(cur, cur, 10*time.Second, 15); stalled {
		t.Fatal("prev == cur must not stall")
	}
	if stalled, _ := GCStalledNow(JVMStats{}, JVMStats{GCTimeMs: 9000}, 10*time.Second, 15); stalled {
		t.Fatal("unavailable readings must not stall")
	}
}

func TestFetchJVM_DecodesAndMarksAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"heap_used":1073741824,"heap_committed":2147483648,"heap_max":4294967296,` +
			`"gc":[{"name":"G1 Young Generation","count":12,"time_ms":345}],"gc_count":12,"gc_time_ms":345}`))
	}))
	defer srv.Close()

	s, err := FetchJVM(srv.URL)
	if err != nil {
		t.Fatalf("FetchJVM: %v", err)
	}
	if !s.Available || s.HeapMax != 4294967296 || s.GCTimeMs != 345 || s.GCCount != 12 {
		t.Fatalf("bad decode: %+v", s)
	}
}

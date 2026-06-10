package daemon

import (
	"sync"
	"time"

	"github.com/fungiblemoose/tickwarden/internal/observe"
)

// Knobs are the tuning parameters an operator may change while the daemon is
// running — the subset of Config the web control plane exposes. The loop reads
// a fresh copy every step, so a change takes effect on the next decision
// without a restart. They are deliberately NOT persisted: tickwarden.toml
// remains the single durable source of settings, and a runtime tweak is an
// experiment until the operator writes it there.
type Knobs struct {
	TargetMSPT float64 `json:"target_mspt"`
	MinSim     int     `json:"min_sim"`
	MaxSim     int     `json:"max_sim"`
	MaxView    int     `json:"max_view"`
	StarvePSI  float64 `json:"starve_psi"`
	GCStallPct float64 `json:"gc_stall_pct"`
	Apply      bool    `json:"apply"`
}

// Status is the daemon's latest observation set, published by the loop after
// every step for the control plane to display. One struct rather than separate
// gauges so a reader always sees one coherent step, never half of two.
type Status struct {
	Time         time.Time        `json:"time"`
	OK           bool             `json:"ok"`  // last companion fetch succeeded
	Err          string           `json:"err,omitempty"`
	Snapshot     observe.Snapshot `json:"snapshot"`
	HostStarved  bool             `json:"host_starved"`
	StarveDetail string           `json:"starve_detail,omitempty"`
	GCStalled    bool             `json:"gc_stalled"`
	GCDetail     string           `json:"gc_detail,omitempty"`
	JVM          observe.JVMStats `json:"jvm"`
	JVMAvailable bool             `json:"jvm_available"`
}

// DecisionRecord is one controller decision, kept in the store's ring so the
// control plane can show what the daemon has been doing — previously this
// history existed only as journald lines.
type DecisionRecord struct {
	Time     time.Time `json:"time"`
	Action   string    `json:"action"`
	SimFrom  int       `json:"sim_from"`
	SimTo    int       `json:"sim_to"`
	ViewFrom int       `json:"view_from"`
	ViewTo   int       `json:"view_to"`
	Reason   string    `json:"reason"`
	Applied  bool      `json:"applied"`
}

// maxDecisions bounds the in-memory decision history. At the default 10s
// interval, 200 records is ~half an hour of full activity — and holds are only
// recorded when the reason changes, so in practice it covers far longer.
const maxDecisions = 200

// Store is the daemon's shared state: the live knobs the web control plane can
// change and the observations/decisions the loop records. All methods are
// safe for concurrent use (loop goroutine writes, HTTP handlers read/write).
type Store struct {
	mu        sync.Mutex
	knobs     Knobs
	status    Status
	decisions []DecisionRecord // newest first
}

// NewStore seeds a store with the daemon's starting knobs.
func NewStore(k Knobs) *Store {
	return &Store{knobs: k}
}

// Knobs returns a copy of the current knobs.
func (s *Store) Knobs() Knobs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.knobs
}

// SetKnobs replaces the knobs wholesale. Validation is the caller's job (the
// web handler validates; tests may set anything).
func (s *Store) SetKnobs(k Knobs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.knobs = k
}

// SetStatus publishes the latest loop observations.
func (s *Store) SetStatus(st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

// Status returns a copy of the latest observations.
func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// AddDecision prepends a decision record, trimming the ring to maxDecisions.
func (s *Store) AddDecision(d DecisionRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append([]DecisionRecord{d}, s.decisions...)
	if len(s.decisions) > maxDecisions {
		s.decisions = s.decisions[:maxDecisions]
	}
}

// Decisions returns a copy of the decision history, newest first.
func (s *Store) Decisions() []DecisionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DecisionRecord, len(s.decisions))
	copy(out, s.decisions)
	return out
}

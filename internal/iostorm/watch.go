package iostorm

import (
	"time"

	"github.com/fungiblemoose/tickwarden/internal/hostagent"
)

// Watch samples the live host n times (interval seconds apart, via the sampling
// window hostagent already enforces) and feeds each snapshot's detected storms to
// emit. It's a thin convenience for a `tickwarden iostorm --watch`-style loop: all
// detection logic lives in the pure Detect, so Watch carries none of its own — it
// only orchestrates repeated sampling. emit is called once per sample (including
// with a nil/empty slice when no storm is present that round) so the caller can
// render an all-clear or accumulate across rounds as it sees fit.
//
// n <= 0 means "loop forever"; callers wanting a bounded run pass a positive n.
// interval is the per-sample delta window in seconds, wired into hostagent.Config;
// any other Config fields (ScanRoot, etc.) the caller sets are preserved.
//
// A sampling error aborts the loop and is returned. We stop rather than continue on
// error because a failed cgroup scan usually means a misconfigured root or "not run
// on the host" — retrying every interval would just spam the same failure.
func Watch(cfg hostagent.Config, interval, n int, emit func([]Storm)) error {
	if interval > 0 {
		cfg.Interval = time.Duration(interval) * time.Second
	}
	for i := 0; n <= 0 || i < n; i++ {
		loads, err := hostagent.Sample(cfg)
		if err != nil {
			return err
		}
		emit(Detect(loads))
	}
	return nil
}

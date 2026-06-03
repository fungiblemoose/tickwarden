// Package optimize is the capstone: a single "run this once and it tells you
// everything" entry point that fuses the signals the other packages expose into
// one prioritized, human read-out.
//
// Each underlying package answers one question — detect: "what am I running
// on?"; detect.ScanMods/Advise: "what mods are installed and is the stack
// sane?"; tune: "what settings should I use, and why?"; observe: "am I being
// starved right now?". An operator reaching for tickwarden usually wants the
// JOIN of all four, not four separate commands. Analyze runs them in the right
// order (feeding mod detection into the tuner's per-core budget, and a measured
// peak into its player sizing) and Report() prints the result as one screen.
//
// This package is pure orchestration over the others: it adds no new detection
// or tuning logic of its own, so the reasons it prints are exactly the reasons
// those packages stand behind. Stdlib only, plus the three internal packages.
package optimize

import (
	"github.com/fungiblemoose/tickwarden/internal/detect"
	"github.com/fungiblemoose/tickwarden/internal/observe"
	"github.com/fungiblemoose/tickwarden/internal/tune"
)

// Options carries the workload assumptions and optional data sources the
// analysis can't read off the host itself.
type Options struct {
	// ModsDir, when set, is scanned for installed mods. The scan both feeds the
	// tuner's per-core budget (chunk-perf mods raise it) and produces curated
	// advice. When empty we scan nothing and make no claims about the mod stack —
	// see Analyze for why that matters.
	ModsDir string
	// Players is the expected PEAK concurrent count. 0 means "use the tune
	// package's default" (a small friends-server), which the tuner clamps no
	// lower than 1.
	Players int
	// PlayersURL, when set, reads the companion's MEASURED peak and overrides
	// Players. A fetch failure is non-fatal (see Analyze).
	PlayersURL string
	// Clustered says the playerbase congregates rather than scatters, which lets
	// the tuner afford a higher simulation distance. Default false is the
	// conservative (spread-out) assumption — see tune.Options.
	Clustered bool
}

// Result is everything Analyze gathered, kept structured so callers can render
// it as JSON or feed Plan into internal/apply, in addition to Report().
type Result struct {
	Profile         detect.Profile   `json:"profile"`
	Mods            detect.ModSet    `json:"-"` // ModSet has unexported state; surfaced via Report
	ModAdvice       detect.ModAdvice `json:"mod_advice"`
	Plan            tune.Plan        `json:"plan"`
	Pressure        observe.Pressure `json:"pressure"` // in-container headroom hint, best-effort
	Players         int              `json:"players"`
	PlayersMeasured bool             `json:"players_measured"`
	// modsScanned records whether ModsDir was set and scanned. It lets Report
	// distinguish "scanned a folder, found no mods" from "never looked", which
	// are very different things to tell an operator.
	modsScanned bool
}

// Analyze runs the full pipeline: Detect the host, optionally ScanMods (and
// Advise on them), optionally read the measured player peak, then Recommend a
// tuning plan, and snapshot in-container pressure.
//
// It returns an error only for genuine failures — today, a mods/ directory that
// exists but can't be read (ScanMods propagates that; treating an unreadable
// modded server as vanilla would hand back a dangerously small recommendation).
// A PlayersURL fetch failure is deliberately NON-fatal: a network blip on the
// companion endpoint shouldn't sink the whole capstone, so we fall back to the
// configured Players with PlayersMeasured=false, mirroring `tickwarden tune`.
func Analyze(opts Options) (Result, error) {
	var r Result

	r.Profile = detect.Detect()

	// Build the tuner options. When no mods dir is given we haven't observed the
	// mod stack at all, so we fall back to the tune package's default PerfMods
	// assumption (optimistic: it assumes chunk-perf mods are present and so uses
	// the higher per-core budget). This matches the common case the tool targets
	// and is documented in Report's mods section so the operator can correct it.
	topts := tune.DefaultOptions()
	topts.Clustered = opts.Clustered

	// Players: 0 means "tuner default" (DefaultOptions, not Recommend's clamp-to-1).
	if opts.Players > 0 {
		topts.Players = opts.Players
	}

	if opts.ModsDir != "" {
		mods, err := detect.ScanMods(opts.ModsDir)
		if err != nil {
			return r, err
		}
		r.Mods = mods
		r.ModAdvice = detect.Advise(mods)
		r.modsScanned = true
		// Now that we've actually looked, set PerfMods from what's installed
		// rather than the optimistic default.
		topts.PerfMods = mods.HasChunkPerfMods()
	}

	// A measured peak (from the companion) overrides the assumed/flag player
	// count — closing the loop from "size for a guess" to "size for reality".
	if opts.PlayersURL != "" {
		if peak, err := observe.FetchPlayerPeak(opts.PlayersURL); err == nil {
			if peak < 1 {
				peak = 1 // even an idle server wants headroom for joins
			}
			topts.Players = peak
			topts.PlayersAuto = true
			r.PlayersMeasured = true
		}
		// On error: fall back silently to topts.Players; Report's summary will
		// show the (unmeasured) count. The non-fatal choice is intentional.
	}

	r.Plan = tune.Recommend(r.Profile, topts)
	r.Players = topts.Players

	// Best-effort headroom snapshot. On non-Linux dev hosts or where PSI isn't
	// exposed this comes back Available=false, which Report handles.
	r.Pressure = observe.ReadPressure()

	return r, nil
}

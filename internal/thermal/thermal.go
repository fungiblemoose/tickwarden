// Package thermal surfaces a signal that no Minecraft profiler reports: the
// host CPU thermal- or frequency-throttling underneath the JVM.
//
// spark and friends see the world from inside the JVM — they can tell you a
// tick took 80ms, but not *why* the silicon was slow. On a passively-cooled
// mini-PC (the typical self-hosted box) the CPU can hit its thermal trip point,
// drop its clock to protect itself, and tank MSPT — and every JVM-level tool is
// blind to it because the OS hides the slowdown as "the CPU was just slower
// this tick." tickwarden reads the kernel's own thermal and cpufreq sysfs
// interfaces to name that cause:
//
//	/sys/class/thermal/thermal_zone*/{type,temp}   (temp in milli-°C)
//	/sys/devices/system/cpu/cpufreq/policy*/{scaling_cur_freq,cpuinfo_max_freq,scaling_max_freq}  (kHz)
//	/sys/class/hwmon/hwmon*/temp*_input            (fallback temp, milli-°C)
//
// All roots hang off Config.SysRoot so tests can point at a t.TempDir fixture
// instead of a live host.
package thermal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Throttling thresholds. They are consts (not knobs) because they encode a
// physical reality, not a preference, and because documenting the WHY beats a
// magic number buried in an if.
//
// TripC is the temperature at which we call it throttling on heat *alone*. Real
// trip points vary by CPU (Intel mobile ~100°C, many ARM SoCs ~85°C, the
// passively-cooled N100/N150 boxes this tool targets commonly throttle in the
// low-to-mid 80s), so 85 is a deliberately conservative floor: at 85°C a fanless
// box is already shedding clock.
//
// WarmC is the lower bar for the *combined* signal: a CPU that is merely warm
// AND visibly clocked down is throttling even below TripC. We require the temp
// conjunction so we never mistake normal idle power-scaling (low clock, cool
// chip) for throttling.
//
// CappedPctFloor is the achieved-clock percentage below which we consider the
// clock "meaningfully" capped. 90% gives ~10% headroom for ordinary frequency
// jitter before we cry throttling.
const (
	TripC          = 85.0
	WarmC          = 80.0
	CappedPctFloor = 90.0
)

// Zone is one named thermal sensor reading.
type Zone struct {
	Label   string  `json:"label"`
	Celsius float64 `json:"celsius"`
}

// Thermal is a snapshot of host temperature and CPU clock state.
//
// FreqCappedPct is, despite the name, the percent of the hardware ceiling the
// CPU is *currently achieving*: CurFreqMHz / MaxFreqMHz * 100. 100 means full
// speed; lower means capped. The field, the Throttling rule, and Verdict all
// share this one convention so the number you read is the number Verdict prints.
type Thermal struct {
	Zones         []Zone   `json:"zones"`
	MaxC          float64  `json:"max_c"`           // hottest zone, °C
	CurFreqMHz    float64  `json:"cur_freq_mhz"`    // highest current clock across policies
	MaxFreqMHz    float64  `json:"max_freq_mhz"`    // hardware ceiling (cpuinfo_max_freq)
	FreqCappedPct float64  `json:"freq_capped_pct"` // cur/max*100; 100=full, lower=capped
	Throttling    bool     `json:"throttling"`
	Notes         []string `json:"notes,omitempty"` // sensor-availability / static-cap diagnostics
}

// Config injects the sysfs root so tests can use fixtures. Zero value uses the
// real "/sys".
type Config struct {
	SysRoot string
}

func (c Config) root() string {
	if c.SysRoot == "" {
		return "/sys"
	}
	return c.SysRoot
}

// Read collects thermal zones and CPU frequency state and decides whether the
// host is throttling. Missing sysfs entries are tolerated — they become Notes,
// not errors — because VMs and trimmed kernels often expose only some of these.
// An error is reserved for the case where nothing thermal could be read at all.
func Read(cfg Config) (Thermal, error) {
	t := Thermal{}

	t.Zones = readZones(cfg)
	if len(t.Zones) == 0 {
		// Fall back to hwmon raw inputs when the thermal-zone class is absent
		// (common on bare hardware exposing only hwmon).
		t.Zones = readHwmon(cfg)
		if len(t.Zones) > 0 {
			t.Notes = append(t.Notes, "no thermal_zone* found; using hwmon temp inputs")
		}
	}
	for _, z := range t.Zones {
		if z.Celsius > t.MaxC {
			t.MaxC = z.Celsius
		}
	}
	if len(t.Zones) == 0 {
		t.Notes = append(t.Notes, "no temperature sensors readable (thermal_zone* and hwmon both absent)")
	}

	cur, hwMax, scalMax, freqOK := readFreq(cfg)
	if freqOK {
		t.CurFreqMHz = cur / 1000 // kHz → MHz
		t.MaxFreqMHz = hwMax / 1000
		if t.MaxFreqMHz > 0 {
			t.FreqCappedPct = t.CurFreqMHz / t.MaxFreqMHz * 100
		}
		// A scaling_max_freq below the hardware ceiling is a *static* cap (a
		// governor/policy limit or firmware throttle) that is real even with no
		// load — worth flagging regardless of temperature.
		if scalMax > 0 && hwMax > 0 && scalMax < hwMax {
			t.Notes = append(t.Notes,
				fmt.Sprintf("scaling_max_freq capped to %.0f MHz of %.0f MHz hardware max — a frequency limit is set",
					scalMax/1000, hwMax/1000))
		}
	} else {
		t.Notes = append(t.Notes, "no cpufreq policy* found (common in VMs) — clock state unknown")
	}

	t.Throttling = decideThrottling(t, freqOK, scalMax < hwMax && scalMax > 0)

	if len(t.Zones) == 0 && !freqOK {
		return t, fmt.Errorf("thermal: no thermal or cpufreq data under %s", cfg.root())
	}
	return t, nil
}

// decideThrottling encodes the two-clause rule. Clause 1 fires on heat alone at
// or above TripC. Clause 2 catches the case where the chip is only warm (≥WarmC)
// but is *visibly clocked down* — i.e. the frequency signal earns its keep by
// detecting throttling below the temperature floor. Either clause, or a static
// scaling cap with warmth, is enough.
//
// We never flag on low clock alone: a cool chip at low clock is normal idle
// power-scaling, not throttling.
func decideThrottling(t Thermal, freqOK, staticCap bool) bool {
	if t.MaxC >= TripC {
		return true
	}
	capped := freqOK && t.MaxFreqMHz > 0 && t.FreqCappedPct < CappedPctFloor
	if (capped || staticCap) && t.MaxC >= WarmC {
		return true
	}
	return false
}

// Verdict is the one-line human read-out, mirroring observe's "print the cause"
// philosophy.
func (t Thermal) Verdict() string {
	if t.Throttling {
		if t.FreqCappedPct > 0 {
			return fmt.Sprintf("CPU at %.0f°C, clocked to %.0f%% of max — likely thermal throttling; check cooling",
				t.MaxC, t.FreqCappedPct)
		}
		return fmt.Sprintf("CPU at %.0f°C — at/over the %.0f°C trip point, likely thermal throttling; check cooling",
			t.MaxC, TripC)
	}
	if t.MaxC > 0 {
		return fmt.Sprintf("thermal headroom OK — %.0f°C, %.0f%% of max clock", t.MaxC, t.FreqCappedPct)
	}
	return "thermal headroom OK (no temperature sensors readable)"
}

// readZones reads /sys/class/thermal/thermal_zone*/{type,temp}. temp is
// milli-°C. Zones with an unreadable temp are skipped (some firmware exposes a
// type but a write-only or absent temp).
func readZones(cfg Config) []Zone {
	dirs, _ := filepath.Glob(filepath.Join(cfg.root(), "class", "thermal", "thermal_zone*"))
	sort.Strings(dirs)
	var zones []Zone
	for _, d := range dirs {
		milli, ok := readMilliC(filepath.Join(d, "temp"))
		if !ok {
			continue
		}
		label := strings.TrimSpace(readString(filepath.Join(d, "type")))
		if label == "" {
			label = filepath.Base(d)
		}
		zones = append(zones, Zone{Label: label, Celsius: milli / 1000})
	}
	return zones
}

// readHwmon is the fallback temp source: /sys/class/hwmon/hwmon*/temp*_input
// (milli-°C). It prefers a sibling temp*_label, else the chip's name file, so
// the reading is still identifiable.
func readHwmon(cfg Config) []Zone {
	inputs, _ := filepath.Glob(filepath.Join(cfg.root(), "class", "hwmon", "hwmon*", "temp*_input"))
	sort.Strings(inputs)
	var zones []Zone
	for _, in := range inputs {
		milli, ok := readMilliC(in)
		if !ok {
			continue
		}
		dir := filepath.Dir(in)
		base := filepath.Base(in) // tempN_input
		label := readString(filepath.Join(dir, strings.TrimSuffix(base, "_input")+"_label"))
		label = strings.TrimSpace(label)
		if label == "" {
			label = strings.TrimSpace(readString(filepath.Join(dir, "name")))
		}
		if label == "" {
			label = base
		}
		zones = append(zones, Zone{Label: label, Celsius: milli / 1000})
	}
	return zones
}

// readFreq reads all cpufreq policies and reduces them to scalars:
//   - cur  = the highest scaling_cur_freq across policies (the busiest cluster)
//   - hwMax = cpuinfo_max_freq (the true hardware ceiling, NOT scaling_max_freq)
//   - scalMax = the lowest scaling_max_freq seen (the tightest active cap)
//
// We take cur as a max because a single hot/busy cluster is what dips MSPT;
// averaging would hide it on big.LITTLE layouts. All values are kHz.
func readFreq(cfg Config) (cur, hwMax, scalMax float64, ok bool) {
	dirs, _ := filepath.Glob(filepath.Join(cfg.root(), "devices", "system", "cpu", "cpufreq", "policy*"))
	for _, d := range dirs {
		if c, got := readKHz(filepath.Join(d, "scaling_cur_freq")); got {
			if c > cur {
				cur = c
			}
			ok = true
		}
		if m, got := readKHz(filepath.Join(d, "cpuinfo_max_freq")); got {
			if m > hwMax {
				hwMax = m
			}
			ok = true
		}
		if sm, got := readKHz(filepath.Join(d, "scaling_max_freq")); got {
			if scalMax == 0 || sm < scalMax {
				scalMax = sm
			}
			ok = true
		}
	}
	return cur, hwMax, scalMax, ok
}

func readMilliC(path string) (float64, bool) { return readFloat(path) }
func readKHz(path string) (float64, bool)    { return readFloat(path) }

func readFloat(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// Package config reads tickwarden's single, optional config file
// (tickwarden.toml) so users can set their server's shape once instead of
// repeating the same flags on every command.
//
// It parses a deliberately tiny subset of TOML by hand, using only the standard
// library — the same approach apply.LoadLocks takes for its [lock] section. No
// TOML dependency is pulled in. We recognize a fixed set of top-level keys (each
// with a known type) plus the existing [lock] section; everything else is
// tolerated, never fatal.
//
// Precedence is left to the caller: Config carries a Set map recording which
// keys were explicitly present in the file, so a caller can layer
// defaults < config < flags (see the package's integration notes / tests).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the parsed tickwarden.toml. Zero values are meaningful only in
// combination with Set: a field reads as its Go zero value unless its key
// appears in Set, which records the keys that were explicitly (and validly)
// present in the file.
type Config struct {
	CompanionURL string  // companion_url
	Players      int     // players
	TargetMSPT   float64 // target_mspt
	MinSim       int     // min_sim
	MaxSim       int     // max_sim
	Clustered    bool    // clustered
	Settled      bool    // settled
	ModsDir      string  // mods_dir

	// Set records which top-level keys were explicitly set in the file (by
	// their TOML key name, e.g. "companion_url"). Use it to decide whether a
	// config value should override a built-in default.
	Set map[string]bool

	// Lock mirrors the existing [lock] section: key -> true for every locked
	// server.properties key. Always non-nil. This duplicates
	// apply.LoadLocks' semantics on purpose — that package is out of scope to
	// edit, so config re-derives the same view rather than importing it.
	Lock map[string]bool

	// Warnings collects non-fatal problems: unknown top-level keys and values
	// that couldn't be parsed as their key's expected type. The corresponding
	// fields/Set entries are left untouched (zero / unset).
	Warnings []string
}

// IsSet reports whether the given TOML key was explicitly present (and valid)
// in the file. Convenience over poking Set directly.
func (c Config) IsSet(key string) bool { return c.Set[key] }

// kind is the expected type of a recognized top-level key.
type kind int

const (
	kindString kind = iota
	kindInt
	kindFloat
	kindBool
)

// knownKeys maps each recognized top-level key to its expected type.
var knownKeys = map[string]kind{
	"companion_url": kindString,
	"players":       kindInt,
	"target_mspt":   kindFloat,
	"min_sim":       kindInt,
	"max_sim":       kindInt,
	"clustered":     kindBool,
	"settled":       kindBool,
	"mods_dir":      kindString,
}

// Load reads and parses tickwarden.toml at path.
//
// A missing file is not an error: it returns a zero-ish Config (empty Set,
// empty Lock, no Warnings) and a nil error, so callers fall back to their own
// defaults. Unknown top-level keys and unparseable values are skipped and
// recorded in Warnings rather than failing the load.
func Load(path string) (Config, error) {
	cfg := Config{
		Set:  map[string]bool{},
		Lock: map[string]bool{},
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	defer f.Close()

	// section tracks which TOML zone we're in:
	//   "" (top level) -> recognized config keys
	//   "lock"         -> the [lock] section
	//   anything else  -> an unrelated section we ignore entirely
	section := ""

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[]")
			continue
		}

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		switch section {
		case "lock":
			if val == "true" {
				cfg.Lock[key] = true
			}
		case "":
			cfg.parseTopLevel(key, val)
		default:
			// Some other section ([foo]); not ours, ignore its keys.
		}
	}
	if err := sc.Err(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// parseTopLevel handles one top-level key=value, applying it to the right
// typed field or recording a warning. On a parse failure the field and Set
// entry are left untouched.
func (c *Config) parseTopLevel(key, val string) {
	k, known := knownKeys[key]
	if !known {
		c.Warnings = append(c.Warnings, fmt.Sprintf("unknown config key %q (ignored)", key))
		return
	}

	switch k {
	case kindString:
		s := unquote(val)
		switch key {
		case "companion_url":
			c.CompanionURL = s
		case "mods_dir":
			c.ModsDir = s
		}
		c.Set[key] = true

	case kindInt:
		n, err := strconv.Atoi(val)
		if err != nil {
			c.warnBadValue(key, val, "integer")
			return
		}
		switch key {
		case "players":
			c.Players = n
		case "min_sim":
			c.MinSim = n
		case "max_sim":
			c.MaxSim = n
		}
		c.Set[key] = true

	case kindFloat:
		x, err := strconv.ParseFloat(val, 64)
		if err != nil {
			c.warnBadValue(key, val, "number")
			return
		}
		if key == "target_mspt" {
			c.TargetMSPT = x
		}
		c.Set[key] = true

	case kindBool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			c.warnBadValue(key, val, "boolean")
			return
		}
		switch key {
		case "clustered":
			c.Clustered = b
		case "settled":
			c.Settled = b
		}
		c.Set[key] = true
	}
}

func (c *Config) warnBadValue(key, val, want string) {
	c.Warnings = append(c.Warnings,
		fmt.Sprintf("config key %q: %q is not a valid %s (ignored)", key, val, want))
}

// unquote strips a single pair of matching surrounding quotes (" or ') from a
// TOML string value. Unquoted values pass through unchanged.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

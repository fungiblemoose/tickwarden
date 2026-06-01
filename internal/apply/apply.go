// Package apply writes tune recommendations into the real config files —
// carefully. It only touches settings it can change safely and reversibly
// (today: server.properties keys), backs up before writing, never overwrites a
// value the user has locked, and defaults to showing a diff rather than acting.
//
// JVM flags and mod configs are deliberately out of scope for automatic writing
// for now: editing a systemd unit or a mod's TOML is format-specific and riskier
// to get wrong than it is valuable. Those recommendations are reported as
// manual. The guardrails (lock file, backup, dry-run) matter more than breadth.
package apply

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/fungiblemoose/tickwarden/internal/tune"
)

// Change is one server.properties edit tickwarden wants to make.
type Change struct {
	Key        string
	Old        string // current value; meaningful only if Present
	New        string
	Present    bool // was the key already in the file?
	Locked     bool // pinned by the user in tickwarden.toml — never written
	Confidence string
	Reason     string
}

// Differs reports whether applying this change would actually alter the file.
func (c Change) Differs() bool {
	return !c.Present || strings.TrimSpace(c.Old) != strings.TrimSpace(c.New)
}

// Plan is the set of server.properties changes for a tune plan.
type Plan struct {
	Path      string
	Changes   []Change // would change the file (and aren't locked)
	Locked    []Change // tickwarden disagrees but the user pinned the value
	Unchanged []Change // already match
}

// PlanProperties computes what `tune --apply` would do to a server.properties
// file, given a tune plan and a set of locked keys.
func PlanProperties(propsPath string, tp tune.Plan, locks map[string]bool) (*Plan, error) {
	current, err := readProps(propsPath)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Path: propsPath}
	for _, rec := range tp.Recs {
		if rec.Target != tune.TargetProperties {
			continue
		}
		old, present := current[rec.Key]
		ch := Change{
			Key:        rec.Key,
			Old:        old,
			New:        rec.Value,
			Present:    present,
			Locked:     locks[rec.Key],
			Confidence: string(rec.Confidence),
			Reason:     rec.Reason,
		}
		switch {
		case !ch.Differs():
			plan.Unchanged = append(plan.Unchanged, ch)
		case ch.Locked:
			plan.Locked = append(plan.Locked, ch)
		default:
			plan.Changes = append(plan.Changes, ch)
		}
	}
	return plan, nil
}

// Write applies the plan's (non-locked) changes to the file, after backing it
// up to <path>.bak. Returns the backup path. It preserves the file's existing
// line order, comments, and blank lines; changed keys are edited in place and
// any genuinely new keys are appended.
func (p *Plan) Write() (backup string, err error) {
	if len(p.Changes) == 0 {
		return "", nil
	}
	original, err := os.ReadFile(p.Path)
	if err != nil {
		return "", err
	}
	backup = p.Path + ".bak"
	if err := os.WriteFile(backup, original, 0o644); err != nil {
		return "", fmt.Errorf("writing backup: %w", err)
	}

	want := map[string]string{}
	for _, c := range p.Changes {
		want[c.Key] = c.New
	}

	var out strings.Builder
	written := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(original)))
	for sc.Scan() {
		line := sc.Text()
		if k, ok := propKey(line); ok {
			if v, found := want[k]; found {
				out.WriteString(k + "=" + v + "\n")
				written[k] = true
				continue
			}
		}
		out.WriteString(line + "\n")
	}
	// Append any new keys that weren't already present.
	for _, c := range p.Changes {
		if !written[c.Key] {
			out.WriteString(c.Key + "=" + c.New + "\n")
		}
	}
	if err := os.WriteFile(p.Path, []byte(out.String()), 0o644); err != nil {
		return backup, err
	}
	return backup, nil
}

// Revert restores <path> from <path>.bak.
func Revert(path string) error {
	backup := path + ".bak"
	data, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("no backup to revert from (%s): %w", backup, err)
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadLocks parses a minimal tickwarden.toml for locked keys. Supported form:
//
//	[lock]
//	view-distance = true
//	simulation-distance = true
//
// A missing file is not an error — it just means nothing is locked.
func LoadLocks(path string) (map[string]bool, error) {
	locks := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return locks, nil
		}
		return nil, err
	}
	defer f.Close()

	inLock := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inLock = line == "[lock]"
			continue
		}
		if !inLock {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(v) == "true" {
			locks[strings.TrimSpace(k)] = true
		}
	}
	return locks, sc.Err()
}

// readProps reads a properties file into a key→value map. A missing file is an
// error here — applying to a server with no server.properties is a mistake we
// want surfaced, not silently papered over.
func readProps(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, ok := propKey(sc.Text()); ok {
			_, v, _ := strings.Cut(sc.Text(), "=")
			m[k] = strings.TrimSpace(v)
		}
	}
	return m, sc.Err()
}

// propKey returns the key of a `key=value` properties line, or ok=false for
// comments, blanks, and non-property lines.
func propKey(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "!") {
		return "", false
	}
	k, _, ok := strings.Cut(t, "=")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(k), true
}

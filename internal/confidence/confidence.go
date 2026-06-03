// Package confidence defines the single Confidence type shared by every
// tickwarden recommender (tune, ostune, ...). Each finding carries one of these
// so the operator can tell a measured rule from folklore at a glance; the
// string values are printed verbatim in reports and serialized as the
// "confidence" JSON field, so they must stay stable.
package confidence

// Confidence flags how much to trust a recommendation or finding.
type Confidence string

const (
	Solid     Confidence = "solid"     // well-established, low risk
	Heuristic Confidence = "heuristic" // reasonable default, tune to taste
	Contested Confidence = "contested" // folklore; validate against your load
)

package release

import "strings"

// curationWarningTags is the exact curation-warning tag vocabulary, in
// canonical (lowercase) form and canonical order. SeaDex curators tag a
// listed release "Broken" or "Incomplete" to warn against grabbing it as-is,
// so every recommendation surface (the daemon's findings, the audit report's
// best/alt classification, the Torznab curation set and RSS journal) gates on
// these tags. Matching is exact and case-insensitive - never substring - so
// only the curators' own vocabulary trips the gate; do not extend the list
// speculatively.
var curationWarningTags = [...]string{"broken", "incomplete"}

// CurationWarnings returns the canonical curation-warning tags present in a
// release's SeaDex tag list: exact, case-insensitive matches against the
// curationWarningTags vocabulary, deduped, in canonical order. Only the
// canonical constants are returned - never raw upstream tag bytes - so
// callers can embed the result in reports and log attributes without
// re-sanitizing. Nil when the release carries no warning.
func CurationWarnings(tags []string) []string {
	var out []string
	for _, w := range curationWarningTags {
		for _, tag := range tags {
			if strings.EqualFold(strings.TrimSpace(tag), w) {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

// CurationWarned reports whether a release's SeaDex tag list carries a
// curation warning (see CurationWarnings). Such a release is never
// recommended: the daemon's compare pass, the audit report's best/alt
// classification, and the Torznab feed all exclude it, so a torrent SeaDex
// marks isBest but tags Broken cannot surface as something to grab.
func CurationWarned(tags []string) bool {
	for _, tag := range tags {
		t := strings.TrimSpace(tag)
		for _, w := range curationWarningTags {
			if strings.EqualFold(t, w) {
				return true
			}
		}
	}
	return false
}

package audit

import "strings"

// curationWarningTags is the exact curation-warning tag vocabulary, in
// canonical (lowercase) form and canonical order. SeaDex curators tag a listed
// release "Broken" or "Incomplete" to warn against grabbing it as-is, so this
// report ANNOTATES such a release with these tags. This list is DISPLAY
// vocabulary only: which tags remove a release from which recommendation
// surface is the operator's filters.exclude_tags policy (internal/tagfilter,
// empty by default), not this fixed pair. Matching is exact and
// case-insensitive - never substring - so only the curators' own vocabulary is
// reported; do not extend the list speculatively.
var curationWarningTags = [...]string{"broken", "incomplete"}

// curationWarnings returns the canonical curation-warning tags present in a
// release's SeaDex tag list: exact, case-insensitive matches against the
// curationWarningTags vocabulary, deduped, in canonical order. Only the
// canonical constants are returned - never raw upstream tag bytes - so the
// Notes column and the machine-readable Release.Warnings field embed the result
// without re-sanitizing. Nil when the release carries no warning.
//
// It never decides whether a release is FILTERED - internal/tagfilter answers
// that for all three surfaces from the operator's config - so a warned release
// that no exclude_tags entry names is annotated AND recommended.
func curationWarnings(tags []string) []string {
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

package audit

import "strings"

// curationWarningTags is the exact curation-warning tag vocabulary, in
// canonical (lowercase) form and canonical order. It is DISPLAY vocabulary:
// which tags remove a release from a surface is the operator's tag policy.
var curationWarningTags = [...]string{"broken", "incomplete"}

// curationWarnings returns the canonical curation-warning tags present in a
// release's SeaDex tag list: exact, case-insensitive matches against the
// curationWarningTags vocabulary, deduped, in canonical order. Only the
// canonical constants are returned, never raw upstream tag bytes. Nil when the
// release carries no warning.
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

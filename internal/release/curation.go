package release

import "strings"

// curationWarningTags is the exact curation-warning tag vocabulary, in
// canonical (lowercase) form and canonical order. SeaDex curators tag a
// listed release "Broken" or "Incomplete" to warn against grabbing it as-is,
// so the audit report ANNOTATES such a release with these tags. This list is
// DISPLAY vocabulary only: which tags remove a release from which
// recommendation surface is the operator's filters.exclude_tags policy
// (internal/tagfilter, empty by default), not this fixed pair. Matching is
// exact and case-insensitive - never substring - so only the curators' own
// vocabulary is reported; do not extend the list speculatively.
var curationWarningTags = [...]string{"broken", "incomplete"}

// CurationWarnings returns the canonical curation-warning tags present in a
// release's SeaDex tag list: exact, case-insensitive matches against the
// curationWarningTags vocabulary, deduped, in canonical order. Only the
// canonical constants are returned - never raw upstream tag bytes - so
// callers can embed the result in reports and log attributes without
// re-sanitizing. Nil when the release carries no warning.
//
// This is the DISPLAY half of the curation-warning story: the audit report's
// Notes column and its machine-readable Release.Warnings field. It never
// decides whether a release is filtered - internal/tagfilter answers that
// question for all three surfaces from the operator's config - so a warned
// release that no exclude_tags entry names is annotated AND recommended.
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

// curationWarned reports whether a release's SeaDex tag list carries a
// curation warning (see CurationWarnings).
//
// It says nothing about whether the release is excluded from anything: the
// three recommendation surfaces (the daemon's findings, the audit report, the
// Torznab feed) each ask the operator's filters.exclude_tags policy
// (internal/tagfilter) instead, which by default excludes NOTHING - so a
// release SeaDex tags Broken reaches all three.
//
// It is retained deliberately with NO production caller: it is the boolean
// reading of the display vocabulary above, kept as the one place that question
// is spelled should a display or diagnostic consumer need it, and it is
// exercised by this package's unit, property and fuzz tests, which cross-check
// it against CurationWarnings and so pin the vocabulary discipline
// (exact, case-insensitive, order-independent) the annotation depends on. It is
// UNEXPORTED for exactly that reason: an exported symbol whose only references
// are _test.go files is what CI's punused gate reports (EU1001), and an
// adjudication entry would claim a cross-package consumer that does not exist.
// Export it again in the same change that gives it one.
func curationWarned(tags []string) bool {
	return len(CurationWarnings(tags)) > 0
}

package release

import (
	"slices"
	"testing"
)

// curationWarned reports whether a tag list carries a curation warning. It is
// the boolean reading of CurationWarnings, and it lives HERE because production
// asks neither question this way: the audit report renders the LIST
// (release.CurationWarnings(t.Tags)), and whether a release is excluded from a
// surface is the operator's filters.exclude_tags policy in internal/tagfilter,
// which by default excludes nothing - so a release SeaDex tags Broken still
// reaches the findings, the report and the feed.
//
// Its job is to be this package's own oracle: the unit table below, the property
// test and the fuzz target all cross-check it against CurationWarnings, which is
// what pins the vocabulary discipline (exact, case-insensitive,
// order-independent) the annotation depends on. Keeping it in curation.go made it
// a production function with no production caller - the deadcode gate reports
// exactly that, and the previous doc block argued for retaining it "should a
// consumer need it", which is the speculative half. Give it a production caller
// and it moves back, in that same change.
func curationWarned(tags []string) bool {
	return len(CurationWarnings(tags)) > 0
}

// TestCurationWarned pins the gate's vocabulary discipline: exact,
// case-insensitive matches on the curators' own tags (broken/incomplete)
// trip it - whitespace-tolerant, never substring - so a tag like
// "semi-broken" or "incompletely" cannot hide a release.
func TestCurationWarned(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want bool
	}{
		{"broken lowercase", []string{"broken"}, true},
		{"broken canonical case", []string{"Broken"}, true},
		{"broken upper", []string{"BROKEN"}, true},
		{"incomplete mixed case", []string{"Incomplete"}, true},
		{"surrounding whitespace tolerated", []string{" Broken "}, true},
		{"warning beside normal tags", []string{"best", "dual", "Broken"}, true},
		{"no substring match on prefix", []string{"brokenish"}, false},
		{"no substring match on compound", []string{"semi-broken"}, false},
		{"no substring match on incompletely", []string{"incompletely"}, false},
		{"no phrase match", []string{"not incomplete"}, false},
		{"unrelated tags", []string{"best", "dual"}, false},
		{"empty tag", []string{""}, false},
		{"nil tags", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := curationWarned(tt.tags); got != tt.want {
				t.Errorf("curationWarned(%q) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

// TestCurationWarnings pins the annotation contract: only the canonical
// lowercase constants come back (never raw upstream tag bytes), deduped, in
// canonical order regardless of input order, and nil when no warning is
// present - so reports and logs can embed the result without re-sanitizing.
func TestCurationWarnings(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want []string
	}{
		{"both in canonical order regardless of input order", []string{"Incomplete", "BROKEN"}, []string{"broken", "incomplete"}},
		{"dedupes repeated spellings", []string{"Broken", " broken "}, []string{"broken"}},
		{"canonical constant not raw bytes", []string{" BrOkEn "}, []string{"broken"}},
		{"single incomplete", []string{"dual", "Incomplete"}, []string{"incomplete"}},
		{"none", []string{"best", "dual"}, nil},
		{"nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CurationWarnings(tt.tags); !slices.Equal(got, tt.want) {
				t.Errorf("CurationWarnings(%q) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

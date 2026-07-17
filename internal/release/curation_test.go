package release

import (
	"slices"
	"testing"
)

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
			if got := CurationWarned(tt.tags); got != tt.want {
				t.Errorf("CurationWarned(%q) = %v, want %v", tt.tags, got, tt.want)
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

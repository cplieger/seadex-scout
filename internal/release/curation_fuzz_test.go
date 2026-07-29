package release

import (
	"slices"
	"testing"
)

// FuzzCurationWarnings fuzzes the curation-warning gate over arbitrary
// untrusted SeaDex tag strings with bounded-output and cross-function
// invariants, never a reimplementation of the exact-match rule: the result is
// always one of the four canonical values (nil, [broken], [incomplete],
// [broken incomplete]) so raw upstream tag bytes can never leak into reports
// or log attributes; curationWarned always agrees with the annotation
// (nil-ness consistency); input tag order never changes the result;
// duplicating the tag list never changes the result (dedupe); and appending a
// canonical warning spelling always trips the gate.
func FuzzCurationWarnings(f *testing.F) {
	f.Add("broken", "incomplete", "best")
	f.Add("Broken", " BROKEN ", "dual")
	f.Add("semi-broken", "incompletely", "not incomplete")
	f.Add("", "  ", "Incomplete")
	f.Add("BrOkEn\u0130", "\u212Aincomplete", "broken\u017f")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		tags := []string{a, b, c}
		canonical := [][]string{nil, {"broken"}, {"incomplete"}, {"broken", "incomplete"}}

		warns := CurationWarnings(tags)
		if len(warns) == 0 && warns != nil {
			t.Errorf("CurationWarnings(%q) = non-nil empty slice, want nil when no warning is present", tags)
		}
		bounded := false
		for _, want := range canonical {
			if slices.Equal(warns, want) {
				bounded = true
				break
			}
		}
		if !bounded {
			t.Errorf("CurationWarnings(%q) = %q, want one of the four canonical values", tags, warns)
		}

		if warned := curationWarned(tags); warned != (warns != nil) {
			t.Errorf("curationWarned(%q) = %v, disagrees with CurationWarnings = %q", tags, warned, warns)
		}

		reversed := []string{c, b, a}
		if got := CurationWarnings(reversed); !slices.Equal(got, warns) {
			t.Errorf("input order changed the result: CurationWarnings(%q) = %q, want %q", reversed, got, warns)
		}

		doubled := []string{a, b, c, a, b, c}
		if got := CurationWarnings(doubled); !slices.Equal(got, warns) {
			t.Errorf("duplicated tags changed the result: CurationWarnings(%q) = %q, want %q", doubled, got, warns)
		}

		augmented := []string{a, b, c, " BrOkEn "}
		if !curationWarned(augmented) {
			t.Errorf("curationWarned(%q) = false after appending a canonical warning spelling", augmented)
		}
	})
}

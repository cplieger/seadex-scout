package release

import (
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// TestCurationWarningsProperties property-tests the curation-warning
// annotation over arbitrary tag lists. The output is bounded to the four
// canonical values (nil, [broken], [incomplete], [broken incomplete]) so raw
// upstream tag bytes can never leak; input tag order never changes the
// result; and appending a canonical warning tag in any casing always trips
// both CurationWarnings and the CurationWarned boolean helper (whose full
// delegated behavior curation_test.go's table pins).
func TestCurationWarningsProperties(t *testing.T) {
	tag := rapid.OneOf(
		rapid.SampledFrom([]string{
			"broken", "Broken", " BROKEN ", "incomplete", "Incomplete",
			"best", "dual", "semi-broken", "incompletely", "not incomplete", "",
		}),
		rapid.String(),
	)
	tagsGen := rapid.SliceOfN(tag, 0, 8)
	canonical := [][]string{nil, {"broken"}, {"incomplete"}, {"broken", "incomplete"}}

	rapid.Check(t, func(t *rapid.T) {
		tags := tagsGen.Draw(t, "tags")

		warns := CurationWarnings(tags)
		bounded := false
		for _, c := range canonical {
			if slices.Equal(warns, c) {
				bounded = true
				break
			}
		}
		if !bounded {
			t.Fatalf("CurationWarnings(%q) = %q, want one of the four canonical values (constants, deduped, canonical order)", tags, warns)
		}

		reversed := slices.Clone(tags)
		slices.Reverse(reversed)
		if got := CurationWarnings(reversed); !slices.Equal(got, warns) {
			t.Fatalf("input tag order changed the result: %q vs %q", got, warns)
		}

		augmented := append(slices.Clone(tags), " BrOkEn ")
		if !CurationWarned(augmented) {
			t.Fatalf("CurationWarned(%q) = false after appending a canonical warning tag", augmented)
		}
		if got := CurationWarnings(augmented); !slices.Contains(got, "broken") {
			t.Fatalf("CurationWarnings(%q) = %q, want to contain the canonical constant broken", augmented, got)
		}
	})
}

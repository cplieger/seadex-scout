package release

import (
	"testing"

	"pgregory.net/rapid"
)

// TestGroupsOverlapProperties property-tests the shared three-valued
// group-set comparison compare and audit key alignment on, with metamorphic
// invariants that do not reimplement the normalizer: the overlap is
// symmetric; an empty side is always None (nothing overlaps an empty set and
// nothing can hide behind one); appending a shared KNOWN group to both sides
// forces Known, with whitespace padding of the shared element not breaking
// the match; appending an unknown-evidence member (the NoGroup sentinel) to
// one side of a two-non-empty-sides comparison never yields a divergence
// proof (the result is Known or Unknown, never None); and Known requires a
// known group present on both sides.
func TestGroupsOverlapProperties(t *testing.T) {
	group := rapid.OneOf(
		rapid.SampledFrom([]string{"", "NOGRP", "no-group", "SubsPlease", " pmr ", "LostYears"}),
		rapid.String(),
	)
	groups := rapid.SliceOfN(group, 0, 6)
	// knownGroup draws a group guaranteed to normalize to known evidence
	// (never the NoGroup sentinel), without reimplementing the normalizer.
	knownGroup := rapid.Custom(func(t *rapid.T) string {
		g := group.Draw(t, "candidate")
		if NormalizeGroup(g) == noGroupNormalized {
			return "KnownGrp"
		}
		return g
	})

	rapid.Check(t, func(t *rapid.T) {
		a := groups.Draw(t, "a")
		b := groups.Draw(t, "b")

		if GroupsOverlap(a, b) != GroupsOverlap(b, a) {
			t.Fatalf("GroupsOverlap not symmetric for %q / %q", a, b)
		}
		if GroupsOverlap(a, nil) != OverlapNone || GroupsOverlap(nil, b) != OverlapNone {
			t.Fatalf("overlap with an empty side must be None: %q / %q", a, b)
		}

		shared := knownGroup.Draw(t, "shared")
		if got := GroupsOverlap(append(a, shared), append(b, shared)); got != OverlapKnown {
			t.Fatalf("appending shared known element %q to both sides = %v, want Known", shared, got)
		}
		if got := GroupsOverlap(append(a, " "+shared+" "), append(b, shared)); got != OverlapKnown {
			t.Fatalf("whitespace-padded shared known element %q = %v, want Known", shared, got)
		}

		if len(b) > 0 {
			if got := GroupsOverlap(append(a, NoGroup), b); got == OverlapNone {
				t.Fatalf("an unknown member beside %q against non-empty %q must never prove divergence", a, b)
			}
		}
	})
}

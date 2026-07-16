package release

import (
	"testing"

	"pgregory.net/rapid"
)

// TestGroupsIntersectProperties property-tests the shared group-overlap
// decision compare and audit key alignment on, with metamorphic invariants
// that do not reimplement the normalizer: intersection is symmetric, an empty
// side never intersects, appending a shared element to both sides forces
// true, and whitespace padding of the shared element does not break the match.
func TestGroupsIntersectProperties(t *testing.T) {
	group := rapid.OneOf(
		rapid.SampledFrom([]string{"", "NOGRP", "no-group", "SubsPlease", " pmr ", "LostYears"}),
		rapid.String(),
	)
	groups := rapid.SliceOfN(group, 0, 6)

	rapid.Check(t, func(t *rapid.T) {
		a := groups.Draw(t, "a")
		b := groups.Draw(t, "b")

		if GroupsIntersect(a, b) != GroupsIntersect(b, a) {
			t.Fatalf("GroupsIntersect not symmetric for %q / %q", a, b)
		}
		if GroupsIntersect(a, nil) || GroupsIntersect(nil, b) {
			t.Fatalf("intersection with an empty side must be false: %q / %q", a, b)
		}

		shared := group.Draw(t, "shared")
		if !GroupsIntersect(append(a, shared), append(b, shared)) {
			t.Fatalf("appending shared element %q to both sides must intersect", shared)
		}
		if !GroupsIntersect(append(a, " "+shared+" "), append(b, shared)) {
			t.Fatalf("whitespace-padded shared element %q must still intersect", shared)
		}
	})
}

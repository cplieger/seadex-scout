package release

import "testing"

// TestGroupNoGroupFallback covers the NoGroup fallback: a release with no group
// classifies and normalizes to NoGroup on both sides, so a group-less library
// file and a group-less SeaDex release (or SeaDex's own literal "NOGRP") compare
// as the same group rather than being skipped.
func TestGroupNoGroupFallback(t *testing.T) {
	if got := Classify(&Input{Group: ""}).Group; got != NoGroup {
		t.Errorf("Classify empty group = %q, want %q", got, NoGroup)
	}
	if got := Classify(&Input{Group: "   "}).Group; got != NoGroup {
		t.Errorf("Classify blank group = %q, want %q", got, NoGroup)
	}
	if got := Classify(&Input{Group: "SubsPlease"}).Group; got != "SubsPlease" {
		t.Errorf("Classify must keep a real group, got %q", got)
	}
	if NormalizeGroup("") != NormalizeGroup(NoGroup) {
		t.Errorf("NormalizeGroup(empty)=%q must equal NormalizeGroup(NoGroup)=%q",
			NormalizeGroup(""), NormalizeGroup(NoGroup))
	}
	// A group-less library value and a group-less SeaDex value must match.
	if NormalizeGroup("") != NormalizeGroup(Classify(&Input{Group: ""}).Group) {
		t.Error("group-less library and SeaDex releases must normalize equal")
	}
}

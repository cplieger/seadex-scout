package keyenc

import (
	"strings"
	"testing"
)

// TestBoundedPartThreshold pins BoundedPart's size-bound boundary: a
// component at MaxComponentBytes keeps the escaped legacy form (persisted
// dedupe keys from earlier versions stay valid), one byte over reduces to the
// deterministic fixed-size hashed identity, and distinct oversized components
// keep distinct identities.
func TestBoundedPartThreshold(t *testing.T) {
	atLimit := strings.Repeat("x", MaxComponentBytes)
	if got := BoundedPart(atLimit); got != atLimit {
		t.Errorf("BoundedPart at the limit = %d bytes starting %.16q, want the escaped legacy form", len(got), got)
	}
	overLimit := atLimit + "x"
	got := BoundedPart(overLimit)
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("BoundedPart over the limit = %d bytes starting %.16q, want the hashed identity", len(got), got)
	}
	if other := BoundedPart(atLimit + "y"); other == got {
		t.Error("distinct oversized components must not share a hashed identity")
	}
	if again := BoundedPart(overLimit); again != got {
		t.Errorf("BoundedPart must be deterministic: %q vs %q", got, again)
	}
}

// TestBoundedJoinPartsThresholdOnRawSize pins that the size bound checks RAW
// component sizes, not the escaped join: an honest delimiter-heavy set whose
// escaped form is ~2x the bound still keeps its exact escaped representation
// (a persisted key never flips shape because escaping grew), while one raw
// byte over the bound reduces to the hashed identity.
func TestBoundedJoinPartsThresholdOnRawSize(t *testing.T) {
	half := MaxComponentBytes / 2
	parts := []string{strings.Repeat(",", half), strings.Repeat("|", half)}
	got := BoundedJoinParts(parts)
	if strings.HasPrefix(got, "sha256:") {
		t.Error("a raw size at the bound must keep the escaped join even when escaping doubles it")
	}
	if got != escapeJoinParts(parts) {
		t.Error("a within-bound set must be byte-identical to its escaped join")
	}
	over := []string{strings.Repeat(",", half), strings.Repeat("|", half+1)}
	if !strings.HasPrefix(BoundedJoinParts(over), "sha256:") {
		t.Error("one raw byte over the bound must reduce to the hashed identity")
	}
}

// TestDomainSeparatesRawAndHashed pins the injectivity of the bounded
// encoding ACROSS the size boundary: a small upstream-controlled component
// that literally spells a hashed identity ("sha256:<hex>") must not collide
// byte-for-byte with the hashed identity of a different, oversized component
// set - the raw and hashed output domains stay disjoint, so two distinct keys
// can never share an encoding through the prefix.
func TestDomainSeparatesRawAndHashed(t *testing.T) {
	oversized := []string{strings.Repeat("x", MaxComponentBytes+1)}
	forged := hashKeyParts(oversized)
	if got := BoundedPart(forged); got == forged {
		t.Errorf("BoundedPart(%q) returned the raw hashed-identity spelling; raw and hashed domains must be disjoint", forged)
	}
	if got := BoundedJoinParts([]string{forged}); got == forged {
		t.Errorf("BoundedJoinParts([%q]) returned the raw hashed-identity spelling; raw and hashed domains must be disjoint", forged)
	}
	// Honest legacy components (no hashed-identity prefix) keep their raw
	// escaped representation, so persisted dedupe state stays valid.
	if got := BoundedPart("PMR"); got != "PMR" {
		t.Errorf("BoundedPart(\"PMR\") = %q, want the legacy raw form", got)
	}
}

// TestSingletonEmptyDoesNotAliasZeroComponents pins injectivity at the
// degenerate end of the component space: escaping preserves element
// boundaries only when the join emits at least one byte, so the singleton
// empty sequence must not share the zero-component sequence's empty encoding
// (an empty untrusted component would otherwise merge two distinct keys). The
// zero-component encoding itself stays the legacy empty string.
func TestSingletonEmptyDoesNotAliasZeroComponents(t *testing.T) {
	none := BoundedJoinParts(nil)
	if none != "" {
		t.Errorf("BoundedJoinParts(nil) = %q, want the legacy empty encoding", none)
	}
	single := BoundedJoinParts([]string{""})
	if single == none {
		t.Errorf("BoundedJoinParts([]string{%q}) = %q, want an encoding distinct from the zero-component one", "", single)
	}
	if !strings.HasPrefix(single, hashedPrefix) {
		t.Errorf("BoundedJoinParts([]string{%q}) = %q, want the hashed identity", "", single)
	}
	if BoundedJoinParts([]string{"", ""}) == single {
		t.Error("the one- and two-empty-component sequences must not share an encoding")
	}
}

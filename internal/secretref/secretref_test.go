package secretref

import "testing"

// TestUnexpandedCoversBothSpellings pins the reason this package exists: the two
// consumer packages disagreed about the brace-less form, and that disagreement
// let an unbraced $NAME passkey reach the AnimeBytes download-link builder as a
// usable credential. Both spellings must read the same here.
func TestUnexpandedCoversBothSpellings(t *testing.T) {
	for name, tc := range map[string]struct {
		val  string
		want bool
	}{
		"braced allowlisted ref":     {"${SEADEX_SCOUT_AB_PASSKEY}", true},
		"braced non-allowlisted ref": {"${AB_PASSKEY}", true},
		"braced empty name":          {"${}", true},
		"brace-less shell ref":       {"$SEADEX_SCOUT_AB_PASSKEY", true},
		"brace-less short name":      {"$X", true},
		"ref embedded in a value":    {"prefix-${AB_PASSKEY}-suffix", true},
		// A real credential must never trip this: a stray '$' followed by
		// lower-case or a digit is not a shell reference by this convention.
		"hex secret":               {"0f1e2d3c4b5a69788796a5b4c3d2e1f0", false},
		"base64-ish secret":        {"aGVsbG8gd29ybGQgc2VjcmV0Cg==", false},
		"dollar then lower-case":   {"abc$def", false},
		"dollar then digit":        {"abc$1", false},
		"bare dollar":              {"$", false},
		"empty":                    {"", false},
		"password-like with punct": {"p@ssw0rd!#%^&*()", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Unexpanded(tc.val); got != tc.want {
				t.Errorf("Unexpanded(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestUnusableIsAbsentOrUnexpanded pins the fail-closed gate: the two cases a
// consumer must treat identically are an absent secret and a placeholder in
// EITHER spelling.
func TestUnusableIsAbsentOrUnexpanded(t *testing.T) {
	for name, tc := range map[string]struct {
		val  string
		want bool
	}{
		"absent":                {"", true},
		"braced ref":            {"${SEADEX_SCOUT_AB_PASSKEY}", true},
		"brace-less ref":        {"$SEADEX_SCOUT_AB_PASSKEY", true},
		"a real 32-char secret": {"0f1e2d3c4b5a69788796a5b4c3d2e1f0", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Unusable(tc.val); got != tc.want {
				t.Errorf("Unusable(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

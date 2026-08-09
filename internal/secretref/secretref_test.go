package secretref

import "testing"

// TestUnexpandedCoversEveryRefSpelling pins the reason this package exists: the
// two consumer packages disagreed about the brace-less form, and that
// disagreement let an unbraced $NAME passkey reach the AnimeBytes download-link
// builder as a usable credential. Every spelling an operator can leave behind -
// braced, unterminated-braced, and brace-less - must read the same here, because
// a reference left behind is a paste rather than a parse.
func TestUnexpandedCoversEveryRefSpelling(t *testing.T) {
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
		// The dropped-brace paste. It matched NEITHER arm while the closing brace
		// was required - the braced arm needs the '}' and the brace-less arm needs
		// an upper-case letter or '_' straight after the '$', which '{' is not - so
		// every fail-closed gate reading this package failed OPEN on it.
		"unterminated braced ref":            {"${SEADEX_SCOUT_AB_PASSKEY", true},
		"unterminated braced empty name":     {"${", true},
		"unterminated ref embedded in value": {"prefix-${AB_PASSKEY", true},
		"unterminated lower-case name":       {"${ab_passkey", true},
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

// TestUnusableIsAbsentOrUnexpanded pins the fail-closed gate: the cases a
// consumer must treat identically are an absent secret and a placeholder in ANY
// spelling. The unterminated form is the one that used to read as usable, which
// is the whole point of this table.
func TestUnusableIsAbsentOrUnexpanded(t *testing.T) {
	for name, tc := range map[string]struct {
		val  string
		want bool
	}{
		"absent":                  {"", true},
		"braced ref":              {"${SEADEX_SCOUT_AB_PASSKEY}", true},
		"brace-less ref":          {"$SEADEX_SCOUT_AB_PASSKEY", true},
		"unterminated braced ref": {"${SEADEX_SCOUT_AB_PASSKEY", true},
		"a real 32-char secret":   {"0f1e2d3c4b5a69788796a5b4c3d2e1f0", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Unusable(tc.val); got != tc.want {
				t.Errorf("Unusable(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

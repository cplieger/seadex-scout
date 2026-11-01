package mediatype

import "testing"

// TestNormalize pins the canonical form both halves of the type contract
// compare in: upper-cased and trimmed, and nothing else (no underscore folding,
// no inner-space collapse), because a stored mapping Record.Type is compared to
// it by exact key.
func TestNormalize(t *testing.T) {
	tests := map[string]string{
		"":          "",
		"movie":     movie,
		"  MOVIE  ": movie,
		"\tova\n":   ova,
		"tv_short":  tvShort,
		"TV SHORT":  "TV SHORT", // a space is NOT an underscore: still unknown
		"not_a_fmt": "NOT_A_FMT",
	}
	for in, want := range tests {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKnownCoversEveryTokenAndNothingElse is the invariant the shared leaf
// exists for: the acceptance half (anilist's knownFormat) and the
// classification half (IsMovie, which picks the title fallback's arr) must
// range over ONE token set. Every exported token is accepted, and an unrecognized or
// non-canonical value is not - so a token added to the constants without being
// added to the set fails here instead of silently desynchronizing the halves.
func TestKnownCoversEveryTokenAndNothingElse(t *testing.T) {
	for _, tok := range []string{tv, tvShort, movie, special, ova, ona, music} {
		if !Known(tok) {
			t.Errorf("Known(%q) = false, want true for a declared token", tok)
		}
	}
	if len(known) != 7 {
		t.Errorf("known holds %d tokens, want the 7 declared constants", len(known))
	}
	for _, tok := range []string{"", "movie", " MOVIE", "NOT_A_FORMAT", "MANGA", "NOVEL", "ONE_SHOT"} {
		if Known(tok) {
			t.Errorf("Known(%q) = true, want false", tok)
		}
	}
}

// TestClassification pins the film predicate over the token set: exactly
// MOVIE is a film, and an unknown or empty token is not.
func TestClassification(t *testing.T) {
	tests := []struct {
		token string
		movie bool
	}{
		{movie, true},
		{tv, false},
		{tvShort, false},
		{ova, false},
		{ona, false},
		{special, false},
		{music, false},
		{"", false},
		{"NOT_A_FORMAT", false},
		{"movie", false}, // non-canonical: callers Normalize first
	}
	for _, tt := range tests {
		if got := IsMovie(tt.token); got != tt.movie {
			t.Errorf("IsMovie(%q) = %v, want %v", tt.token, got, tt.movie)
		}
	}
}

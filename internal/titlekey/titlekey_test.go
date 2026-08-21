package titlekey

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"punctuation and case stripped", "Frieren: Beyond Journey's End", "frierenbeyondjourneysend"},
		{"digits kept", "Sousou no Frieren (2023)", "sousounofrieren2023"},
		{"separators collapsed", "Re:ZERO -Starting Life in Another World-", "rezerostartinglifeinanotherworld"},
		{"em dash stripped", "86\u2014Eighty Six\u2014", "86eightysix"},
		{"CJK-only strips to empty", "\u846c\u9001\u306e\u30d5\u30ea\u30fc\u30ec\u30f3", ""},
		{"punctuation-only strips to empty", "!!!---...", ""},
		{"whitespace-only strips to empty", " \t ", ""},
		{"empty stays empty", "", ""},
		{"dotted capital I lowercases into the key", "\u0130stanbul", "istanbul"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Normalize(tt.title); got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

// TestContainsKey pins the containment rule the indexer's title harvest reads:
// a key matches only as an EXACT run of the candidate's own alphanumeric
// tokens, at every key length. Normalize strips every separator, so a plain
// normalized-substring test carries no boundary evidence at any length - "x"
// is satisfied by "Remux", and a real title-length key just as blindly
// ("gate" inside "Propagate", "bleach" inside "Unbleached", "zero" inside
// "ReZero"). Both directions are pinned here: a token run matches across the
// decoration a release name carries, and a key buried inside a longer token
// does not.
func TestContainsKey(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		want      string
		match     bool
	}{
		{"long key matches across decoration", "[Grp] Sousou no Frieren - S01 (BD 1080p)", "sousounofrieren", true},
		{"long key absent", "[Grp] Some Other Show - S01 (BD 1080p)", "sousounofrieren", false},
		{"long key spanning separators", "[Grp] 86 - Eighty Six - S01", "86eightysix", true},
		{"short key as its own token", "[Grp] X - S01 (BD 1080p)", "x", true},
		{"short key inside release metadata is refused", "[Grp] Show - S01 (BD Remux 1080p x265)", "x", false},
		{"short key across adjacent tokens", "[Grp] A B - S01", "ab", true},
		{"short key needs an exact token run", "[Grp] Abc - S01", "ab", false},
		{"empty candidate never matches a short key", "", "x", false},
		{"title-length key inside a longer word is refused", "[Grp] Unbleached Cotton - S01 (BD 1080p)", "bleach", false},
		{"title-length key as a word suffix is refused", "[Grp] Propagate - S01 (BD 1080p)", "gate", false},
		{"title-length key inside a CamelCase token is refused", "[Grp] ReZero - S01 (BD 1080p)", "zero", false},
		{"key straddling token boundaries is refused", "[Grp] Sousou no Frieren - S01", "ousounofrier", false},
		// The split class is the same [0-9a-z] alphabet Normalize keeps, and its
		// range ends are where a rune silently leaves that alphabet: a rune read
		// as a separator splits one token in two, so a key that really is a run
		// of the candidate's tokens stops matching. ("a" is pinned by the
		// adjacent-token case above.)
		{"a leading-range digit continues a token", "[Grp] Mobile Suit Gundam 00 - S01", "gundam00", true},
		{"a trailing-range digit continues a token", "[Grp] Show 99 - S01 (BD 1080p)", "show99", true},
		{"the last letter continues a token", "[Grp] Re Zero - S01 (BD 1080p)", "rezero", true},
		{"Unicode capital in the candidate lowercases into a token", "[Grp] \u0130stanbul - S01 (BD 1080p)", "istanbul", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsKey(tt.candidate, tt.want); got != tt.match {
				t.Errorf("ContainsKey(%q, %q) = %v, want %v", tt.candidate, tt.want, got, tt.match)
			}
		})
	}
}

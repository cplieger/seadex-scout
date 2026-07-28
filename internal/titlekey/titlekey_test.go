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

// TestContainsKey pins the two arms of the containment test the indexer's title
// harvest reads: a key of real length keeps the punctuation-tolerant normalized
// substring, while a SHORT key needs boundary evidence - an exact match against
// a run of the candidate's own alphanumeric tokens - so ordinary release
// metadata ("Remux", "x265") cannot satisfy it.
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsKey(tt.candidate, tt.want); got != tt.match {
				t.Errorf("ContainsKey(%q, %q) = %v, want %v", tt.candidate, tt.want, got, tt.match)
			}
		})
	}
}

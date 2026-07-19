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

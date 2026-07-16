package match

import "testing"

// FuzzNormalizeTitle fuzzes the title normalizer that keys the AniList title
// fallback index over untrusted AniList and arr titles. Invariants: the output
// contains only [a-z0-9] (the index-key charset, so two titles differing only
// in punctuation/case/whitespace collide as intended and nothing else leaks
// into a key), and normalization is idempotent (re-normalizing a key never
// changes it, so index build and lookup agree).
func FuzzNormalizeTitle(f *testing.F) {
	f.Add("Frieren: Beyond Journey's End")
	f.Add("Sousou no Frieren (2023)")
	f.Add("Re:ZERO -Starting Life in Another World-")
	f.Add("\u846c\u9001\u306e\u30d5\u30ea\u30fc\u30ec\u30f3") // CJK-only strips to empty
	f.Add("  ")
	f.Add("\u0130stanbul") // dotted capital I: ToLower yields i + combining dot
	f.Fuzz(func(t *testing.T, title string) {
		got := normalizeTitle(title)
		for _, r := range got {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				t.Errorf("normalizeTitle(%q) = %q contains %q outside [a-z0-9]", title, got, r)
			}
		}
		if again := normalizeTitle(got); again != got {
			t.Errorf("normalizeTitle not idempotent: %q -> %q -> %q", title, got, again)
		}
	})
}

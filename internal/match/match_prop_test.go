package match

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestNormalizeTitleCaseAndPunctuationProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base := rapid.StringMatching(`[A-Za-z0-9]{1,64}`).Draw(t, "base")
		want := normalizeTitle(base)
		if want == "" {
			t.Fatalf("normalizeTitle(%q) = empty, want a non-empty key", base)
		}
		decorated := " \t-._:" + strings.ToUpper(base) + "[]()!? "
		if got := normalizeTitle(decorated); got != want {
			t.Fatalf("normalizeTitle(%q) = %q, want %q from undecorated %q", decorated, got, want, base)
		}
	})
}

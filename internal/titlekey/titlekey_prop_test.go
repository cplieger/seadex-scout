package titlekey

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestNormalizeCaseAndPunctuationProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base := rapid.StringMatching(`[A-Za-z0-9]{1,64}`).Draw(t, "base")
		want := Normalize(base)
		if want == "" {
			t.Fatalf("Normalize(%q) = empty, want a non-empty key", base)
		}
		decorated := " \t-._:" + strings.ToUpper(base) + "[]()!? "
		if got := Normalize(decorated); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q from undecorated %q", decorated, got, want, base)
		}
	})
}

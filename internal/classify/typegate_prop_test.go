package classify

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestTypeGateASCIICaseInsensitiveProperty pins the file-eligibility type
// gate's case contract over generated marker-bearing names: swapping the ASCII
// case of every letter must change neither IsMediaFile, IsCreditlessExtra, nor
// ContentMediaFile. The creditless marker spells out 30 explicit case classes
// (a global (?i) is unusable - Go regexp's SimpleFold diverges from
// strings.ToLower on U+0130 and U+017F), and the unit table only exercises the
// all-upper spelling of each token, so a single dropped alternative - [Nn]
// typed as [N] - would leave a lowercase "nced" extra voting as content
// evidence. Only ASCII letters are swapped: a non-ASCII fold can legitimately
// change the [^[:alnum:]] boundary (U+212A lowercases onto ASCII 'k', turning a
// delimiter into a word character), so it is outside the invariant.
func TestTypeGateASCIICaseInsensitiveProperty(t *testing.T) {
	tokenGen := rapid.SampledFrom([]string{
		"ncop", "NCOP", "NcOp", "nced", "NCED", "creditless", "CREDITLESS",
		"CRED\u0130TLESS", "01", "v2", "V2", "Show", "1080p", "x265",
		"[", "]", "_", "-", " ", ".", ".mkv", ".MKV", ".ass", ".WEBM",
	})
	rapid.Check(t, func(t *rapid.T) {
		name := strings.Join(rapid.SliceOfN(tokenGen, 0, 6).Draw(t, "tokens"), "")
		swapped := swapASCIICase(name)
		if got, want := IsMediaFile(swapped), IsMediaFile(name); got != want {
			t.Errorf("IsMediaFile(%q) = %v, but IsMediaFile(%q) = %v", swapped, got, name, want)
		}
		if got, want := IsCreditlessExtra(swapped), IsCreditlessExtra(name); got != want {
			t.Errorf("IsCreditlessExtra(%q) = %v, but IsCreditlessExtra(%q) = %v", swapped, got, name, want)
		}
		if got, want := ContentMediaFile(swapped), ContentMediaFile(name); got != want {
			t.Errorf("ContentMediaFile(%q) = %v, but ContentMediaFile(%q) = %v", swapped, got, name, want)
		}
	})
}

// swapASCIICase flips the case of every ASCII letter and leaves every other
// rune untouched: the transformation the type gate must be blind to.
func swapASCIICase(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return r
	}, s)
}

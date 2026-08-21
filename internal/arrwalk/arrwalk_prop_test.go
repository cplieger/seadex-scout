package arrwalk

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestIsDualAudioPropTokenSetSemantics pins isDualAudio's contract that the
// result depends only on the SET of case-normalized language tokens: it is
// invariant under token order, separator choice ('/' vs ','), duplicate
// tokens, and appended whitespace-only tokens, and the same language repeated
// in different letter case is never dual audio.
func TestIsDualAudioPropTokenSetSemantics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		langs := rapid.SliceOfN(
			rapid.SampledFrom([]string{"Japanese", "English", "jpn", "eng", "Commentary", "ger"}), 1, 4,
		).Draw(t, "langs")
		sep1 := rapid.SampledFrom([]string{"/", ",", " / ", " , "}).Draw(t, "sep1")
		sep2 := rapid.SampledFrom([]string{"/", ",", " / ", " , "}).Draw(t, "sep2")
		base := strings.Join(langs, sep1)
		got := isDualAudio(base)

		reversed := make([]string, 0, len(langs))
		for _, l := range slices.Backward(langs) {
			reversed = append(reversed, l)
		}
		if r := isDualAudio(strings.Join(reversed, sep2)); r != got {
			t.Fatalf("isDualAudio(%q reversed w/ %q) = %v, want %v (order/separator invariance)", base, sep2, r, got)
		}
		if r := isDualAudio(base + sep1 + langs[0]); r != got {
			t.Fatalf("isDualAudio(%q + dup token) = %v, want %v (duplicate invariance)", base, r, got)
		}
		if r := isDualAudio(base + sep1 + "   "); r != got {
			t.Fatalf("isDualAudio(%q + blank token) = %v, want %v (blank tokens ignored)", base, r, got)
		}
		// Case-normalization oracle: one language repeated in different letter
		// case is a single distinct language, never dual audio.
		caseDup := langs[0] + sep1 + strings.ToUpper(langs[0])
		if isDualAudio(caseDup) {
			t.Fatalf("isDualAudio(%q) = true, want false (case-insensitive duplicate is one language)", caseDup)
		}
	})
}

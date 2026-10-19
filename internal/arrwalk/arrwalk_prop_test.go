package arrwalk

import (
	"slices"
	"testing"

	"pgregory.net/rapid"
)

// TestIsDualAudioPropTokenSetSemantics pins that isDualAudio depends only on the
// set of languages: it is invariant under track order and
// duplicate tracks, one language repeated is never dual audio, and adding a
// second language makes it dual audio.
func TestIsDualAudioPropTokenSetSemantics(t *testing.T) {
	pool := []string{"jpn", "eng", "ger", "fre", "spa"}
	rapid.Check(t, func(t *rapid.T) {
		langs := rapid.SliceOfN(rapid.SampledFrom(pool), 1, 6).Draw(t, "langs")
		got := isDualAudio(langs)

		reversed := slices.Clone(langs)
		slices.Reverse(reversed)
		if r := isDualAudio(reversed); r != got {
			t.Fatalf("isDualAudio(%q) = %v, want %v as for %q (order invariance)", reversed, r, got, langs)
		}
		dup := rapid.IntRange(0, len(langs)-1).Draw(t, "dup")
		if r := isDualAudio(slices.Insert(slices.Clone(langs), dup, langs[dup])); r != got {
			t.Fatalf("isDualAudio(%q with track %d repeated) = %v, want %v (duplicate invariance)", langs, dup, r, got)
		}

		same := slices.Repeat(langs[:1], rapid.IntRange(1, 6).Draw(t, "n"))
		if isDualAudio(same) {
			t.Fatalf("isDualAudio(%q) = true, want false (one language)", same)
		}
		other := rapid.SampledFrom(pool).Filter(func(l string) bool { return l != langs[0] }).Draw(t, "other")
		at := rapid.IntRange(0, len(same)).Draw(t, "at")
		if withOther := slices.Insert(same, at, other); !isDualAudio(withOther) {
			t.Fatalf("isDualAudio(%q) = false, want true (two languages)", withOther)
		}
	})
}

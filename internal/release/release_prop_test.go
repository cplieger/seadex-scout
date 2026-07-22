package release

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestGroupsOverlapProperties property-tests the shared three-valued
// group-set comparison compare and audit key alignment on, with metamorphic
// invariants that do not reimplement the normalizer: the overlap is
// symmetric; an empty side is always None (nothing overlaps an empty set and
// nothing can hide behind one); appending a shared KNOWN group to both sides
// forces Known, with whitespace padding of the shared element not breaking
// the match; appending an unknown-evidence member (the NoGroup sentinel) to
// one side of a two-non-empty-sides comparison never yields a divergence
// proof (the result is Known or Unknown, never None); and Known requires a
// known group present on both sides.
func TestGroupsOverlapProperties(t *testing.T) {
	group := rapid.OneOf(
		rapid.SampledFrom([]string{"", "NOGRP", "no-group", "SubsPlease", " pmr ", "LostYears"}),
		rapid.String(),
	)
	groups := rapid.SliceOfN(group, 0, 6)
	// knownGroup draws a group guaranteed to normalize to known evidence
	// (never the NoGroup sentinel), without reimplementing the normalizer.
	knownGroup := rapid.Custom(func(t *rapid.T) string {
		g := group.Draw(t, "candidate")
		if NormalizeGroup(g) == noGroupNormalized {
			return "KnownGrp"
		}
		return g
	})

	rapid.Check(t, func(t *rapid.T) {
		a := groups.Draw(t, "a")
		b := groups.Draw(t, "b")

		if GroupsOverlap(a, b) != GroupsOverlap(b, a) {
			t.Fatalf("GroupsOverlap not symmetric for %q / %q", a, b)
		}
		if GroupsOverlap(a, nil) != OverlapNone || GroupsOverlap(nil, b) != OverlapNone {
			t.Fatalf("overlap with an empty side must be None: %q / %q", a, b)
		}

		shared := knownGroup.Draw(t, "shared")
		if got := GroupsOverlap(append(a, shared), append(b, shared)); got != OverlapKnown {
			t.Fatalf("appending shared known element %q to both sides = %v, want Known", shared, got)
		}
		if got := GroupsOverlap(append(a, " "+shared+" "), append(b, shared)); got != OverlapKnown {
			t.Fatalf("whitespace-padded shared known element %q = %v, want Known", shared, got)
		}

		if len(b) > 0 {
			if got := GroupsOverlap(append(a, NoGroup), b); got == OverlapNone {
				t.Fatalf("an unknown member beside %q against non-empty %q must never prove divergence", a, b)
			}
		}
	})
}

// TestClassifyPlantedMarkerProperties property-tests Classify with an
// oracle-style planted-marker construction (never a reimplementation of the
// tokenizer): a name is BUILT from a marker-free title vocabulary and one or
// more known marker tokens joined by a random scene delimiter, so the
// expected classification is known by construction. It pins, over random
// composition: a marker-free name classifies unknown with no codec or
// resolution; a planted remux token always classifies remux; a planted
// encoder marker always classifies encode; remux wins when both are planted
// in the same name; a planted resolution is extracted lowercased and ranks
// positive; markers planted in a LATER Names element still classify; and the
// group fallback and structured-only dual-audio contracts hold throughout.
func TestClassifyPlantedMarkerProperties(t *testing.T) {
	// Title words are marker-free by construction: no substring of any codec
	// text token (avc/x264/h264/x265/h265/hevc, matched unbounded), no
	// bounded marker token (remux/premux/encode(d)/bdrip/crf/kbps/mbps), and
	// no digits (resolutions and bitrates need them).
	word := rapid.SampledFrom([]string{"Show", "Title", "Alpha", "Beta", "Gamma", "Sword", "Girl", "Piece"})
	delim := rapid.SampledFrom([]string{" ", ".", "_", "-"})
	remuxTok := rapid.SampledFrom([]string{"remux", "REMUX", "BDRemux", "BD-Remux", "bd_remux", "PREMUX", "Remuxed"})
	encodeTok := rapid.SampledFrom([]string{"x265", "X264", "HEVC", "avc", "BDRip", "encoded", "ENCODE", "CRF18", "crf.18", "4500kbps", "12 mbps"})
	resTok := rapid.SampledFrom([]string{"2160p", "1440p", "1080P", "720p", "480p"})

	classifyName := func(names ...string) Release {
		return Classify(&Input{Names: names})
	}

	rapid.Check(t, func(t *rapid.T) {
		d := delim.Draw(t, "delim")
		base := strings.Join(rapid.SliceOfN(word, 1, 4).Draw(t, "words"), d)

		if got := classifyName(base); got.Kind != KindUnknown || got.Codec != "" || got.Resolution != "" {
			t.Fatalf("marker-free name %q classified %q/%q/%q, want unknown kind, empty codec and resolution", base, got.Kind, got.Codec, got.Resolution)
		}

		remux := remuxTok.Draw(t, "remux")
		if got := classifyName(base + d + remux); got.Kind != KindRemux {
			t.Fatalf("planted remux token %q in %q classified %q, want remux", remux, base+d+remux, got.Kind)
		}

		encode := encodeTok.Draw(t, "encode")
		if got := classifyName(base + d + encode); got.Kind != KindEncode {
			t.Fatalf("planted encoder marker %q in %q classified %q, want encode", encode, base+d+encode, got.Kind)
		}

		both := base + d + remux + d + encode
		if got := classifyName(both); got.Kind != KindRemux {
			t.Fatalf("remux must win over an encoder marker in the same name: %q classified %q", both, got.Kind)
		}

		res := resTok.Draw(t, "res")
		withRes := base + d + res
		got := classifyName(withRes)
		if want := strings.ToLower(res); got.Resolution != want {
			t.Fatalf("planted resolution %q in %q extracted as %q, want %q", res, withRes, got.Resolution, want)
		}
		if ResolutionRank(got.Resolution) <= 0 {
			t.Fatalf("extracted resolution %q ranks %d, want > 0", got.Resolution, ResolutionRank(got.Resolution))
		}

		later := classifyName(base, base+d+remux+d+res)
		if later.Kind != KindRemux || later.Resolution != strings.ToLower(res) {
			t.Fatalf("markers in a later Names element classified %q/%q, want remux/%q", later.Kind, later.Resolution, strings.ToLower(res))
		}

		if got := classifyName(both); got.Group != NoGroup {
			t.Fatalf("group fallback broken: Group=%q, want %q", got.Group, NoGroup)
		}

		dualAudioText := base + d + "Dual Audio"
		if got := classifyName(dualAudioText); got.DualAudio {
			t.Fatalf("text-only dual-audio marker in %q set DualAudio=true, want structured metadata only", dualAudioText)
		}
		if got := Classify(&Input{Names: []string{dualAudioText}, DualAudio: true}); !got.DualAudio {
			t.Fatal("structured DualAudio=true was not preserved")
		}
	})
}

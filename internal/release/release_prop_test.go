package release

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestGroupsOverlapProperties property-tests the shared three-valued group-set comparison
// compare and audit key alignment on, with metamorphic invariants that do not reimplement
// the normalizer: symmetry; an empty side is always None (nothing overlaps an empty set and
// nothing can hide behind one); appending a shared KNOWN group to both sides forces Known
// even with whitespace padding; appending the NoGroup sentinel to both sides forces Known;
// and appending it to one side never yields a divergence proof.
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
		if GroupsOverlap(a, nil) != overlapNone || GroupsOverlap(nil, b) != overlapNone {
			t.Fatalf("overlap with an empty side must be None: %q / %q", a, b)
		}

		shared := knownGroup.Draw(t, "shared")
		if got := GroupsOverlap(append(a, shared), append(b, shared)); got != OverlapKnown {
			t.Fatalf("appending shared known element %q to both sides = %v, want Known", shared, got)
		}
		if got := GroupsOverlap(append(a, " "+shared+" "), append(b, shared)); got != OverlapKnown {
			t.Fatalf("whitespace-padded shared known element %q = %v, want Known", shared, got)
		}

		if got := GroupsOverlap(append(a, NoGroup), append(b, "no-group")); got != OverlapKnown {
			t.Fatalf("the NoGroup sentinel on both sides of %q / %q = %v, want Known", a, b, got)
		}

		if len(b) > 0 {
			if got := GroupsOverlap(append(a, NoGroup), b); got == overlapNone {
				t.Fatalf("an unknown member beside %q against non-empty %q must never prove divergence", a, b)
			}
		}
	})
}

// TestNormalizeGroupProperties property-tests the group identity both sides
// compare on. Idempotence over arbitrary text: a value already normalized (the
// library side persists normalized groups and compare normalizes them again)
// must not move. And a metamorphic pair over built names that start and end
// with a letter or digit: wrapping the name in edge delimiters, appending a
// whitespace-separated parenthetical qualifier, or both, never changes its
// identity, while a qualifier glued to the name without whitespace is kept.
func TestNormalizeGroupProperties(t *testing.T) {
	alnum := rapid.SampledFrom([]rune("aZ09ūσ³"))
	inner := rapid.SampledFrom([]rune("aZ09ū-_. &+@'|"))
	name := rapid.Custom(func(t *rapid.T) string {
		body := rapid.SliceOfN(inner, 0, 6).Draw(t, "inner")
		return string(alnum.Draw(t, "first")) + string(body) + string(alnum.Draw(t, "last"))
	})
	edge := rapid.StringOfN(rapid.SampledFrom([]rune(" ._-")), 1, 3, -1)
	qualifier := rapid.StringOfN(rapid.SampledFrom([]rune("a4k HDR")), 0, 8, -1)

	rapid.Check(t, func(t *rapid.T) {
		raw := rapid.String().Draw(t, "raw")
		if once := NormalizeGroup(raw); NormalizeGroup(once) != once {
			t.Fatalf("NormalizeGroup not idempotent: %q -> %q -> %q", raw, once, NormalizeGroup(once))
		}

		n := name.Draw(t, "name")
		want := NormalizeGroup(n)
		q := " (" + qualifier.Draw(t, "qualifier") + ")"
		decorated := []string{
			edge.Draw(t, "lead") + n + edge.Draw(t, "trail"),
			n + q,
			edge.Draw(t, "lead2") + n + q + edge.Draw(t, "trail2"),
		}
		for _, d := range decorated {
			if got := NormalizeGroup(d); got != want {
				t.Fatalf("NormalizeGroup(%q) = %q, want %q (the identity of %q)", d, got, want, n)
			}
		}
		if glued := n + "(x)"; NormalizeGroup(glued) == want {
			t.Fatalf("NormalizeGroup(%q) = %q, the identity of %q; a qualifier needs whitespace before it", glued, want, n)
		}
	})
}

// TestClassifyPlantedMarkerProperties property-tests Classify with an oracle-style
// planted-marker construction, never a reimplementation of the tokenizer: a name is BUILT
// from a marker-free title vocabulary plus one or more known marker tokens joined by a
// random scene delimiter, so the expected classification is known by construction. Each
// per-assertion message carries its own claim (remux beats encode, a marker in a LATER
// Names element still classifies, the group fallback and dual-audio contracts hold).
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

// TestClassifyToLowerFaithfulness pins the in-place matcher's central design claim: every
// text-derived field (Kind, Reason, Codec, Resolution) must classify raw evidence exactly
// as it classifies the strings.ToLower image, which is what the shared case classes
// (nametoken.Literal) and word-alphabet edges (nametoken.NonWordEdge) guarantee. A (?i)
// rewrite (SimpleFold matches U+017F but misses U+0130), a dropped U+0130/U+212A class
// member or a non-fold-invariant edge class breaks the equivalence on some generated input,
// so the marker machinery is pinned structurally rather than only by example rows. Group
// and Tracker are excluded: they pass raw casing through by contract.
func TestClassifyToLowerFaithfulness(t *testing.T) {
	piece := rapid.OneOf(
		rapid.SampledFrom([]string{
			"REMUX", "BDRemux", "PREMUX", "Remuxed", "BDRip", "BDR\u0130P", "encode", "ENCODED",
			"x265", "HEVC", "AVC", "h.264", "H.265", "CRF18", "crf 20", "4500 kbps", "4500 \u212abps",
			"1080p", "2160P", "21080p", "\u0130", "\u017f", "\u212a", "_", ".", "-", " ", "Show",
		}),
		rapid.String(),
	)
	pieces := rapid.SliceOfN(piece, 1, 6)
	rapid.Check(t, func(t *rapid.T) {
		name := strings.Join(pieces.Draw(t, "name"), "")
		notes := strings.Join(pieces.Draw(t, "notes"), "")
		raw := Classify(&Input{Names: []string{name}, Notes: notes})
		lowered := Classify(&Input{Names: []string{strings.ToLower(name)}, Notes: strings.ToLower(notes)})
		if raw.Kind != lowered.Kind || raw.Reason != lowered.Reason ||
			raw.Codec != lowered.Codec || raw.Resolution != lowered.Resolution {
			t.Fatalf("raw text and its ToLower image classify differently:\nname %q notes %q\nraw     %q/%q/%q/%q\nlowered %q/%q/%q/%q",
				name, notes,
				raw.Kind, raw.Reason, raw.Codec, raw.Resolution,
				lowered.Kind, lowered.Reason, lowered.Codec, lowered.Resolution)
		}
	})
}

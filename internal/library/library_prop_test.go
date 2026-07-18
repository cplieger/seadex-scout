package library

import (
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// genPropItem generates a library Item over a deliberately small key space so
// generated snapshots overlap on arr:id keys and all three diff outcomes
// (added, removed, changed) are reachable, plus occasional Sonarr Failed
// placeholders so the diff's per-key partial suppression is property-covered.
func genPropItem(t *rapid.T) Item {
	arr := rapid.SampledFrom([]string{ArrSonarr, ArrRadarr}).Draw(t, "arr")
	id := rapid.IntRange(1, 6).Draw(t, "id")
	if arr == ArrSonarr && rapid.Bool().Draw(t, "failed") {
		return Item{Arr: arr, ArrID: id, Failed: true}
	}
	groups := rapid.SliceOfN(rapid.SampledFrom([]string{"pmr", "lostyears", "nogrp", "seed"}), 0, 3).Draw(t, "groups")
	var sg map[int][]string
	if len(groups) > 0 && rapid.Bool().Draw(t, "hasSeasons") {
		sg = map[int][]string{rapid.IntRange(0, 3).Draw(t, "season"): groups}
	}
	return Item{
		Arr:          arr,
		ArrID:        id,
		Groups:       groups,
		SeasonGroups: sg,
		HasFile:      len(groups) > 0,
	}
}

// genPropSnapshot generates a snapshot of 0-8 items, setting Partial whenever
// a Sonarr Failed placeholder was generated, so generated snapshots honor the
// producer invariant (Walk publishes Partial=true exactly when a failed
// series' placeholder is present in Items).
func genPropSnapshot(t *rapid.T, label string) *Snapshot {
	n := rapid.IntRange(0, 8).Draw(t, label+"N")
	items := make([]Item, 0, n)
	partial := false
	for range n {
		it := genPropItem(t)
		partial = partial || it.Failed
		items = append(items, it)
	}
	return &Snapshot{Items: items, Partial: partial}
}

// TestDiffSnapshotsPropIdentity pins the diff's reflexivity: diffing any
// snapshot against itself must report nothing added, removed, or changed.
func TestDiffSnapshotsPropIdentity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genPropSnapshot(t, "s")
		if d := DiffSnapshots(s, s); d != (Diff{}) {
			t.Fatalf("DiffSnapshots(s, s) = %+v, want zero Diff", d)
		}
	})
}

// TestDiffSnapshotsPropSymmetry pins the diff's direction symmetry for
// complete snapshots: an addition one way is a removal the other way, and
// Changed is direction-independent (sameItem is symmetric).
func TestDiffSnapshotsPropSymmetry(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prev := genPropSnapshot(t, "prev")
		cur := genPropSnapshot(t, "cur")
		fwd := DiffSnapshots(prev, cur)
		rev := DiffSnapshots(cur, prev)
		if fwd.Added != rev.Removed || fwd.Removed != rev.Added || fwd.Changed != rev.Changed {
			t.Fatalf("DiffSnapshots not symmetric: forward %+v, reverse %+v", fwd, rev)
		}
	})
}

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

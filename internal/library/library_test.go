package library

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
)

// diffItem builds a minimal comparable Item for the DiffSnapshots tests.
func diffItem(arr string, id int, groups ...string) Item {
	return Item{Arr: arr, ArrID: id, Groups: groups, HasFile: len(groups) > 0}
}

// placeholder builds a degraded Item: arr identity only, no file data, exactly
// the shape internal/arrwalk publishes for a failed episode fetch or a movie
// Radarr reports a file for without sending its payload.
func placeholder(arr string, id int) Item {
	return Item{Arr: arr, ArrID: id, Failed: true}
}

// TestItemComparable pins the model's placeholder predicate, the one rule every
// consumer that partitions a snapshot routes through: a placeholder's file data
// is missing rather than empty, so it is not comparable, while an ordinary
// fileless item is (its no-file state is real).
func TestItemComparable(t *testing.T) {
	fileless := Item{Arr: ArrRadarr, ArrID: 1}
	if !fileless.Comparable() {
		t.Error("a genuinely fileless item must be comparable: its no-file state is real")
	}
	degraded := placeholder(ArrRadarr, 1)
	if degraded.Comparable() {
		t.Error("a placeholder must not be comparable: its file data is missing, not empty")
	}
}

func TestDiffSnapshots(t *testing.T) {
	prev := &Snapshot{Items: []Item{
		diffItem(ArrSonarr, 1, "pmr"),
		diffItem(ArrSonarr, 2, "grp"),
		diffItem(ArrRadarr, 3, "movgrp"),
	}}
	cur := &Snapshot{Items: []Item{
		diffItem(ArrSonarr, 1, "pmr"),       // unchanged
		diffItem(ArrSonarr, 2, "lostyears"), // changed group set
		diffItem(ArrSonarr, 4, "newgrp"),    // added
		// Radarr id 3 removed
	}}
	d := DiffSnapshots(prev, cur)
	if d.Added != 1 || d.Removed != 1 || d.Changed != 1 {
		t.Errorf("diff = %+v, want Added=1 Removed=1 Changed=1", d)
	}
}

// TestDiffSnapshotsPartialAware pins the per-key partial suppression on the
// diff: only a key that is a placeholder (in cur for removals, in prev
// for additions) is suppressed, while an item genuinely absent from a Partial
// snapshot still diffs - a published partial walk keeps every failed series
// as a placeholder, so absence means the arr no longer lists it. The blanket
// "partial suppresses every Sonarr addition/removal" behavior is retired: it
// permanently masked real removals and additions once partial walks started
// retaining placeholders.
func TestDiffSnapshotsPartialAware(t *testing.T) {
	t.Run("failed placeholder in cur suppresses only its own removal", func(t *testing.T) {
		// Series A's episode fetch failed this walk (a placeholder);
		// series B is truly gone from Sonarr. B reports removed even though
		// cur is Partial; A does not, and a change on a clean item counts.
		prev := &Snapshot{Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),  // A: fetch failed this walk
			diffItem(ArrSonarr, 2, "grp"),  // B: genuinely removed
			diffItem(ArrSonarr, 3, "seed"), // C: group changed
		}}
		cur := &Snapshot{Partial: true, Items: []Item{
			placeholder(ArrSonarr, 1),
			diffItem(ArrSonarr, 3, "lostyears"),
		}}
		d := DiffSnapshots(prev, cur)
		if d.Removed != 1 || d.Changed != 1 || d.Added != 0 {
			t.Errorf("diff = %+v, want Removed=1 (only the truly gone series) Changed=1 Added=0", d)
		}
	})
	t.Run("failed placeholder in prev suppresses only its own addition", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),
			placeholder(ArrSonarr, 2), // recovers this walk
		}}
		cur := &Snapshot{Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),
			diffItem(ArrSonarr, 2, "grp"),    // recovered, not an arrival
			diffItem(ArrSonarr, 4, "newgrp"), // genuinely added
		}}
		d := DiffSnapshots(prev, cur)
		if d.Added != 1 || d.Removed != 0 || d.Changed != 0 {
			t.Errorf("diff = %+v, want Added=1 (only the genuinely new series) with the recovery suppressed", d)
		}
	})
	t.Run("clean radarr transitions count during a sonarr partial", func(t *testing.T) {
		// Partial is set only by Sonarr episode-fetch failures, and a Radarr
		// item is a placeholder only for its own missing-file-payload
		// degradation, so an ordinary movie's presence change always counts.
		prev := &Snapshot{Items: []Item{
			diffItem(ArrSonarr, 1, "pmr"),
			diffItem(ArrRadarr, 3, "movgrp"), // genuinely removed
		}}
		cur := &Snapshot{Partial: true, Items: []Item{
			placeholder(ArrSonarr, 1),
			diffItem(ArrRadarr, 4, "newmov"), // genuinely added
		}}
		d := DiffSnapshots(prev, cur)
		if d.Added != 1 || d.Removed != 1 || d.Changed != 0 {
			t.Errorf("diff = %+v, want Added=1 Removed=1 (radarr transitions) with the sonarr failure suppressed", d)
		}
	})
}

func TestDiffSnapshotsDetectsFingerprintChangeWithSameGroup(t *testing.T) {
	x264 := release.Classify(&release.Input{
		Names: []string{"[PMR] Example [1080p][x264]"}, Group: "pmr", VideoCodec: "AVC",
	})
	x265 := release.Classify(&release.Input{
		Names: []string{"[PMR] Example [1080p][x265]"}, Group: "pmr", VideoCodec: "HEVC",
	})
	prev := &Snapshot{Items: []Item{{
		Arr:     ArrSonarr,
		ArrID:   1,
		Groups:  []string{"pmr"},
		Current: x264,
		HasFile: true,
	}}}
	cur := &Snapshot{Items: []Item{{
		Arr:     ArrSonarr,
		ArrID:   1,
		Groups:  []string{"pmr"},
		Current: x265,
		HasFile: true,
	}}}

	d := DiffSnapshots(prev, cur)
	if d.Added != 0 || d.Removed != 0 || d.Changed != 1 {
		t.Errorf("diff = %+v, want exactly one changed item for same-group fingerprint drift", d)
	}
}

// TestDiffSnapshotsDetectsSeasonGroupAttributionChange pins the third leg of
// the documented Changed contract: an item whose overall group set and
// fingerprint are unchanged but whose per-season group attribution moved
// (the groups swapped seasons) must still count as Changed.
func TestDiffSnapshotsDetectsSeasonGroupAttributionChange(t *testing.T) {
	prev := &Snapshot{Items: []Item{{
		Arr:          ArrSonarr,
		ArrID:        1,
		Groups:       []string{"lostyears", "pmr"},
		SeasonGroups: map[int][]string{1: {"pmr"}, 2: {"lostyears"}},
		HasFile:      true,
	}}}
	cur := &Snapshot{Items: []Item{{
		Arr:          ArrSonarr,
		ArrID:        1,
		Groups:       []string{"lostyears", "pmr"},
		SeasonGroups: map[int][]string{1: {"lostyears"}, 2: {"pmr"}},
		HasFile:      true,
	}}}
	d := DiffSnapshots(prev, cur)
	if d.Added != 0 || d.Removed != 0 || d.Changed != 1 {
		t.Errorf("diff = %+v, want exactly one changed item for a season-attribution-only change", d)
	}
}

// TestDiffSnapshotsKeysByArrAndID pins the documented "keyed by arr + id"
// contract: a Sonarr item and a Radarr item sharing the same numeric arr id
// are distinct entries, so removing only the Radarr one counts exactly one
// removal and no change on the same-id Sonarr item.
func TestDiffSnapshotsKeysByArrAndID(t *testing.T) {
	prev := &Snapshot{Items: []Item{
		{Arr: ArrSonarr, ArrID: 1, Groups: []string{"pmr"}, HasFile: true},
		{Arr: ArrRadarr, ArrID: 1, Groups: []string{"movgrp"}, HasFile: true},
	}}
	cur := &Snapshot{Items: []Item{
		{Arr: ArrSonarr, ArrID: 1, Groups: []string{"pmr"}, HasFile: true},
	}}
	d := DiffSnapshots(prev, cur)
	if d.Added != 0 || d.Removed != 1 || d.Changed != 0 {
		t.Errorf("diff = %+v, want Removed=1 Changed=0 (arr-qualified keys keep same-id items distinct)", d)
	}
}

// TestDiffSnapshotsSkipsFailedPlaceholders pins the placeholder keys' exclusion
// from comparison: a placeholder carries no comparable file state, so
// its key is never Changed, its own removal is suppressed while it is a
// placeholder in cur, and its recovery is not an addition when it was one in
// prev. The Radarr row covers the missing-file-payload degradation, which is a
// placeholder inside a COMPLETE walk (no Partial).
func TestDiffSnapshotsSkipsFailedPlaceholders(t *testing.T) {
	t.Run("failed placeholder in cur is not a change or removal", func(t *testing.T) {
		prev := &Snapshot{Items: []Item{diffItem(ArrSonarr, 1, "pmr")}}
		cur := &Snapshot{Partial: true, Items: []Item{placeholder(ArrSonarr, 1)}}
		if d := DiffSnapshots(prev, cur); d != (Diff{}) {
			t.Errorf("diff = %+v, want zero Diff (a placeholder carries no comparable state)", d)
		}
	})
	t.Run("radarr placeholder in a complete walk is not a change or removal", func(t *testing.T) {
		prev := &Snapshot{Items: []Item{diffItem(ArrRadarr, 9, "movgrp")}}
		cur := &Snapshot{Items: []Item{placeholder(ArrRadarr, 9)}}
		if d := DiffSnapshots(prev, cur); d != (Diff{}) {
			t.Errorf("diff = %+v, want zero Diff (a movie with no file payload is a placeholder, not a removal)", d)
		}
	})
	t.Run("failed placeholder in prev is not an addition when the series returns", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{placeholder(ArrSonarr, 1)}}
		cur := &Snapshot{Items: []Item{diffItem(ArrSonarr, 1, "pmr")}}
		if d := DiffSnapshots(prev, cur); d != (Diff{}) {
			t.Errorf("diff = %+v, want zero Diff (a returning series after a failed walk is not added)", d)
		}
	})
	t.Run("failed placeholder gone from cur is a removal", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{placeholder(ArrSonarr, 1)}}
		cur := &Snapshot{}
		if d := DiffSnapshots(prev, cur); d != (Diff{Removed: 1}) {
			t.Errorf("diff = %+v, want Removed=1 (a placeholder still carries arr presence)", d)
		}
	})
	t.Run("key debuting as a failed placeholder is an addition", func(t *testing.T) {
		prev := &Snapshot{}
		cur := &Snapshot{Partial: true, Items: []Item{placeholder(ArrSonarr, 2)}}
		if d := DiffSnapshots(prev, cur); d != (Diff{Added: 1}) {
			t.Errorf("diff = %+v, want Added=1 (a new series whose first fetch failed is still an arrival)", d)
		}
	})
	t.Run("failed placeholder on both sides is no transition", func(t *testing.T) {
		prev := &Snapshot{Partial: true, Items: []Item{placeholder(ArrSonarr, 3)}}
		cur := &Snapshot{Partial: true, Items: []Item{placeholder(ArrSonarr, 3)}}
		if d := DiffSnapshots(prev, cur); d != (Diff{}) {
			t.Errorf("diff = %+v, want zero Diff (a placeholder on both sides is no transition)", d)
		}
	})
}

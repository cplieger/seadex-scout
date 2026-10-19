package library

import (
	"encoding/json"
	"maps"
	"slices"
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

func TestDiffSnapshotsDetectsRevisionOnlyChange(t *testing.T) {
	none1 := release.Revision{Version: 1, Marker: release.RevisionNone}
	v2 := release.Revision{Version: 2, Marker: release.RevisionVersion}
	base := func() Item {
		return Item{
			Arr: ArrSonarr, ArrID: 1, Groups: []string{"g"}, HasFile: true,
			SeasonGroups:    map[int][]string{1: {"g"}},
			SeasonRevisions: map[int]map[string]release.Revision{1: {"g": none1}},
			Revisions:       map[string]release.Revision{"g": none1},
		}
	}
	seasonChanged := base()
	seasonChanged.SeasonRevisions = map[int]map[string]release.Revision{1: {"g": v2}}
	itemChanged := base()
	itemChanged.Revisions = map[string]release.Revision{"g": v2}
	tests := []struct {
		desc string
		cur  Item
		want int
	}{
		{"season revision re-grabbed", seasonChanged, 1},
		{"item revision re-grabbed", itemChanged, 1},
		{"unchanged", base(), 0},
	}
	for _, tc := range tests {
		d := DiffSnapshots(&Snapshot{Items: []Item{base()}}, &Snapshot{Items: []Item{tc.cur}})
		if d.Changed != tc.want || d.Added != 0 || d.Removed != 0 {
			t.Errorf("DiffSnapshots [%s] = %+v, want Changed=%d only", tc.desc, d, tc.want)
		}
	}
}

func TestLegacyItemDecodesWithoutRevisions(t *testing.T) {
	var it Item
	if err := json.Unmarshal([]byte(`{"arr":"sonarr","arr_id":1,"title":"T","season_groups":{"1":["g"]},"groups":["g"],"has_file":true,"current":{"group":"G"}}`), &it); err != nil {
		t.Fatalf("decoding an item written before revisions existed: %v", err)
	}
	if it.SeasonRevisions != nil || it.Revisions != nil {
		t.Errorf("legacy item decoded revisions %+v / %+v, want both nil (every reading unknown)", it.SeasonRevisions, it.Revisions)
	}
	if !slices.Equal(it.Groups, []string{"g"}) || !slices.Equal(it.SeasonGroups[1], []string{"g"}) || it.Current.Group != "G" {
		t.Errorf("legacy item siblings = groups %v, season groups %v, current %q; want them intact", it.Groups, it.SeasonGroups, it.Current.Group)
	}
}

func TestItemRevisionsRoundTrip(t *testing.T) {
	repack := release.Revision{Version: 3, Marker: release.RevisionRepack}
	in := Item{
		Arr: ArrSonarr, ArrID: 1, Title: "T",
		SeasonRevisions: map[int]map[string]release.Revision{0: {"udf": repack}},
		Revisions:       map[string]release.Revision{"udf": repack},
		Current:         release.Release{Group: "UDF", Revision: repack},
	}
	b, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out Item
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}
	if out.SeasonRevisions[0]["udf"] != repack || out.Revisions["udf"] != repack || out.Current.Revision != repack {
		t.Errorf("round trip of %s = %+v / %+v / %+v, want %+v everywhere", b, out.SeasonRevisions, out.Revisions, out.Current.Revision, repack)
	}
}

func TestDiffSnapshotsDetectsSeasonZeroEpisodeChange(t *testing.T) {
	base := func() Item {
		return Item{
			Arr: ArrSonarr, ArrID: 1, Groups: []string{"g"}, HasFile: true,
			SeasonGroups: map[int][]string{0: {"g"}},
			Specials:     map[int]ListedEpisode{5: {Group: "g", HasFile: true}, 6: {}},
		}
	}
	moved := base()
	moved.Specials = map[int]ListedEpisode{5: {}, 6: {Group: "g", HasFile: true}}
	unread := base()
	unread.Specials = nil
	for _, tc := range []struct {
		desc string
		cur  Item
		want int
	}{
		{"a file moved to another special", moved, 1},
		{"the specials became unknown", unread, 1},
		{"unchanged", base(), 0},
	} {
		d := DiffSnapshots(&Snapshot{Items: []Item{base()}}, &Snapshot{Items: []Item{tc.cur}})
		if d.Changed != tc.want || d.Added != 0 || d.Removed != 0 {
			t.Errorf("DiffSnapshots [%s] = %+v, want Changed=%d only", tc.desc, d, tc.want)
		}
	}
}

func TestDiffSnapshotsDetectsAListedEpisodeChange(t *testing.T) {
	base := func() Item {
		return Item{
			Arr: ArrSonarr, ArrID: 1, Groups: []string{"g"}, HasFile: true, SeasonGroups: map[int][]string{1: {"g"}},
			Listed: map[int]map[int]ListedEpisode{1: {1: {Group: "g", HasFile: true}, 2: {}}},
		}
	}
	added := base()
	added.Listed[1][3] = ListedEpisode{}
	unread := base()
	unread.Listed = nil
	for _, tc := range []struct {
		desc string
		cur  Item
		want int
	}{
		{"Sonarr lists another episode", added, 1},
		{"the listing became unknown", unread, 1},
		{"unchanged", base(), 0},
	} {
		d := DiffSnapshots(&Snapshot{Items: []Item{base()}}, &Snapshot{Items: []Item{tc.cur}})
		if d.Changed != tc.want || d.Added != 0 || d.Removed != 0 {
			t.Errorf("DiffSnapshots [%s] = %+v, want Changed=%d only", tc.desc, d, tc.want)
		}
	}
}

func TestItemSpecialsRoundTripAndLegacyReadsUnknown(t *testing.T) {
	in := Item{Arr: ArrSonarr, ArrID: 1, Specials: map[int]ListedEpisode{
		9:  {Group: "mtbb", Revision: release.Revision{Version: 2, Marker: release.RevisionVersion}, HasFile: true},
		10: {},
	}}
	b, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out Item
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}
	if !maps.Equal(out.Specials, in.Specials) {
		t.Errorf("round trip of %s = %+v, want %+v", b, out.Specials, in.Specials)
	}
	var legacy Item
	if err := json.Unmarshal([]byte(`{"arr":"sonarr","arr_id":1,"title":"T","season_groups":{"0":["g"]},"has_file":true,"current":{}}`), &legacy); err != nil {
		t.Fatalf("decoding an item written before season-0 episodes were read: %v", err)
	}
	if legacy.Specials != nil {
		t.Errorf("legacy item Specials = %+v, want nil (unknown)", legacy.Specials)
	}
}

// A series whose episode list names a season-0 file its file list lacks holds
// unknown specials though no season-0 group is known, and the
// state survives the persisted snapshot and counts as a change.
func TestItemSpecialsFileUnknownWithoutSeasonZeroGroup(t *testing.T) {
	in := Item{Arr: ArrSonarr, ArrID: 1, SeasonGroups: map[int][]string{1: {"g"}}, SpecialsFileUnknown: true}
	if !in.SpecialsUnknown() {
		t.Error("SpecialsUnknown() = false for SpecialsFileUnknown with no season-0 group, want true")
	}
	b, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out Item
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}
	if !out.SpecialsUnknown() {
		t.Errorf("round trip of %s reads specials known, want unknown", b)
	}
	known := in
	known.SpecialsFileUnknown = false
	if known.SpecialsUnknown() {
		t.Error("SpecialsUnknown() = true for a series with no season-0 file, want false")
	}
	if d := DiffSnapshots(&Snapshot{Items: []Item{known}}, &Snapshot{Items: []Item{in}}); d.Changed != 1 {
		t.Errorf("DiffSnapshots(specials became unknown) = %+v, want Changed=1", d)
	}
}

// TestDiffSnapshotsDetectsAReplacedFile pins that a file replaced by another of
// the same group, which changes no group or fingerprint, still reads as a change.
func TestDiffSnapshotsDetectsAReplacedFile(t *testing.T) {
	item := func(file int, size int64) Item {
		return Item{
			Arr: ArrSonarr, ArrID: 1, Groups: []string{"pmr"}, HasFile: true,
			FileBytes:      map[int]int64{file: size},
			SeasonEpisodes: map[int]map[int]Episode{1: {1: {File: file}}},
		}
	}
	tests := map[string]Item{"new file id": item(2, 100), "new size": item(1, 200)}
	for name, cur := range tests {
		t.Run(name, func(t *testing.T) {
			d := DiffSnapshots(&Snapshot{Items: []Item{item(1, 100)}}, &Snapshot{Items: []Item{cur}})
			if d.Changed != 1 {
				t.Errorf("diff = %+v, want one changed item", d)
			}
		})
	}
}

// TestItemOrderSnapshotShape pins the persisted Order shape: a series in a
// non-aired order round-trips its Order, and an official series (Order nil)
// encodes byte-identically to the snapshot before Order existed (the expected
// bytes were recorded from that code).
func TestItemOrderSnapshotShape(t *testing.T) {
	t.Run("official series unchanged", func(t *testing.T) {
		it := Item{
			Arr: ArrSonarr, ArrID: 7, Title: "Code Geass", TvdbID: 79525, HasFile: true,
			SeasonGroups: map[int][]string{1: {"sam"}}, Groups: []string{"sam"},
		}
		b, err := json.Marshal(&it)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		const want = `{"season_groups":{"1":["sam"]},"arr":"sonarr","title":"Code Geass","groups":["sam"],"current":{},"arr_id":7,"tvdb_id":79525,"has_file":true}`
		if string(b) != want {
			t.Errorf("official item JSON = %s, want %s", b, want)
		}
	})
	t.Run("non-aired order round trip", func(t *testing.T) {
		in := Item{Arr: ArrSonarr, ArrID: 2189, Title: "Monogatari", Order: &Order{Type: "alternate", Name: "Book Order"}}
		b, err := json.Marshal(&in)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("json.Unmarshal(%s): %v", b, err)
		}
		if got, want := string(raw["order"]), `{"type":"alternate","name":"Book Order"}`; got != want {
			t.Errorf("order member = %s, want %s", got, want)
		}
		var out Item
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("json.Unmarshal(%s): %v", b, err)
		}
		if out.Order == nil || *out.Order != *in.Order {
			t.Errorf("round trip Order = %+v, want %+v", out.Order, in.Order)
		}
	})
	// arrapi reports an empty seasonType as sonarr.Order{}, "an unknown order,
	// never aired": it must survive a restart as an order, or the series would
	// compare in aired order after the snapshot reloads.
	t.Run("unknown order round trip", func(t *testing.T) {
		in := Item{Arr: ArrSonarr, ArrID: 2203, Title: "Unknown", Order: &Order{}}
		b, err := json.Marshal(&in)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("json.Unmarshal(%s): %v", b, err)
		}
		if got, want := string(raw["order"]), `{"type":""}`; got != want {
			t.Errorf("order member = %s, want %s", got, want)
		}
		var out Item
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("json.Unmarshal(%s): %v", b, err)
		}
		if out.Order == nil || *out.Order != (Order{}) {
			t.Errorf("round trip Order = %+v, want &Order{}", out.Order)
		}
	})
}

// TestOrderLabel pins the order naming rule: the order's name when Sonarr
// lists one, else its type code, else a phrase for the unknown order arrapi
// reports for an empty seasonType.
func TestOrderLabel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order Order
		want  string
	}{
		{name: "named", order: Order{Type: "alternate", Name: "Book Order"}, want: "Book Order"},
		{name: "unnamed", order: Order{Type: "alternate"}, want: "alternate"},
		{name: "unknown", order: Order{}, want: "an unknown order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.order.Label(); got != tc.want {
				t.Errorf("Order%+v.Label() = %q, want %q", tc.order, got, tc.want)
			}
		})
	}
}

// TestDiffSnapshotsDetectsAnOrderChange pins that a series switching between
// aired and another order reads as a change: it decides whether it compares.
func TestDiffSnapshotsDetectsAnOrderChange(t *testing.T) {
	aired := diffItem(ArrSonarr, 1, "g")
	dvd := aired
	dvd.Order = &Order{Type: "dvd", Name: "DVD Order"}
	if d := DiffSnapshots(&Snapshot{Items: []Item{aired}}, &Snapshot{Items: []Item{dvd}}); d.Changed != 1 {
		t.Errorf("DiffSnapshots(aired to DVD order) = %+v, want Changed=1", d)
	}
	same := dvd
	same.Order = &Order{Type: "dvd", Name: "DVD Order"}
	if d := DiffSnapshots(&Snapshot{Items: []Item{dvd}}, &Snapshot{Items: []Item{same}}); d.Changed != 0 {
		t.Errorf("DiffSnapshots(equal orders in distinct pointers) = %+v, want Changed=0", d)
	}
}

// TestItemAniListIDsSnapshotShape pins the persisted AniList ids
// (anilist_ids, omitted when empty, so a Sonarr 4 series' snapshot is
// unchanged, as TestItemOrderSnapshotShape's official case shows) and that a
// change to them reads as a change: they decide whether a season compares.
func TestItemAniListIDsSnapshotShape(t *testing.T) {
	in := Item{Arr: ArrSonarr, ArrID: 33, Title: "Code Geass", AniListIDs: []int{1575, 2904}}
	b, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}
	if got, want := string(raw["anilist_ids"]), `[1575,2904]`; got != want {
		t.Errorf("anilist_ids member = %s, want %s", got, want)
	}
	var out Item
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", b, err)
	}
	if !slices.Equal(out.AniListIDs, in.AniListIDs) {
		t.Errorf("round trip AniListIDs = %v, want %v", out.AniListIDs, in.AniListIDs)
	}

	before := diffItem(ArrSonarr, 1, "g")
	after := before
	after.AniListIDs = []int{1575}
	if d := DiffSnapshots(&Snapshot{Items: []Item{before}}, &Snapshot{Items: []Item{after}}); d.Changed != 1 {
		t.Errorf("DiffSnapshots(AniList ids added) = %+v, want Changed=1", d)
	}
	same := after
	same.AniListIDs = []int{1575}
	if d := DiffSnapshots(&Snapshot{Items: []Item{after}}, &Snapshot{Items: []Item{same}}); d.Changed != 0 {
		t.Errorf("DiffSnapshots(equal AniList ids) = %+v, want Changed=0", d)
	}
}

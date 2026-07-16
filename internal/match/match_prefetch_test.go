package match

import (
	"context"
	"testing"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestPrefetchNegativelyMemoizesOnCompleteBatch pins the prefetch negative-memo
// branch: when the batch completes (no error) but omits a requested id, AniList
// has no such media, so prefetch must memoize the negative and the per-entry
// pass must NOT issue a second single Fetch for it. batchCountingAniList is
// defined in match_test.go.
func TestPrefetchNegativelyMemoizesOnCompleteBatch(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 77, Type: "MOVIE"}})
	fake := &batchCountingAniList{media: map[int]anilist.Media{}}
	m := NewMatcher(fake, nil)

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 77}}, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1", fake.batchCalls)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("single Fetch calls = %d, want 0 (a completed batch memoizes the negative)", fake.fetchCalls)
	}
	if ent, ok := res.Memo.Entries[77]; !ok || !ent.NotFound {
		t.Errorf("memo[77] = %+v (present=%v), want a NotFound negative entry", ent, ok)
	}
	if res.Degraded {
		t.Error("Degraded = true, want false: a definitive not-found is not a degraded cycle")
	}
	if len(res.Matches) != 1 || res.Matches[0].Source != SourceUnmapped {
		t.Errorf("match = %+v, want a single unmapped entry", res.Matches)
	}
}

// TestMatchNoRecordEntryRidesBatchPrefetch pins that an entry with NO Fribb
// record (the other batch trigger, beside the id-less record
// TestMatchBatchesAniListLookups pins) is resolved through the batch prefetch:
// one FetchMany pre-warms the memo and the per-entry pass makes zero single
// Fetch calls while still title-matching the entry to its library item.
func TestMatchNoRecordEntryRidesBatchPrefetch(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 5, Title: "Clannad", TvdbID: 700, Year: 2007},
	}}
	idx := mapping.NewIndex(nil) // no Fribb record at all: the no-record trigger
	fake := &batchCountingAniList{media: map[int]anilist.Media{
		600: {Titles: []string{"Clannad"}, Format: "TV", Year: 2007},
	}}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 600}}, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1 (a no-record entry must ride the batch prefetch)", fake.batchCalls)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("single Fetch calls = %d, want 0 (the batch pre-warms the memo)", fake.fetchCalls)
	}
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
		t.Errorf("matches = %+v, want one title match to the Sonarr series", res.Matches)
	}
}

package match

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// batchRecordErrAniList fails every batch record-locally: FetchMany returns
// per-id verdicts marking every id UNVERIFIED plus anilist.ErrBatchRecord (the
// completed chunks held only malformed records), while single Fetch still
// resolves from the canned map.
type batchRecordErrAniList struct {
	batchCountingAniList
}

func (b *batchRecordErrAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	b.batchCalls++
	b.batchSizes = append(b.batchSizes, len(ids))
	media := map[int]anilist.Media{}
	return anilist.BatchResult{
			Media:    media,
			Verdicts: batchVerdictsAbsentAs(ids, media, anilist.VerdictUnverified),
		},
		fmt.Errorf("%w media record 0 missing id", anilist.ErrBatchRecord)
}

// TestPrefetchEmptyRecordLocalBatchFallsBackPerID pins the outage
// classification boundary: an all-unverified batch result plus ErrBatchRecord is
// a record-local failure (the chunks completed; every record was malformed),
// NOT a total AniList outage, so prefetch must leave the pending ids uncached
// for the documented per-id Fetch fallback instead of failing them fast - the
// entry still title-matches through the single Fetch.
func TestPrefetchEmptyRecordLocalBatchFallsBackPerID(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 5, Title: "Clannad", TvdbID: 700, Year: 2007},
	}}
	idx := mapping.NewIndex(nil)
	fake := &batchRecordErrAniList{batchCountingAniList{media: map[int]anilist.Media{
		600: {Titles: []string{"Clannad"}, Format: "TV", Year: 2007},
	}}}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 600}}, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1", fake.batchCalls)
	}
	if fake.fetchCalls != 1 {
		t.Errorf("single Fetch calls = %d, want 1 (record-local empty batch must fall back per id, not fail fast)", fake.fetchCalls)
	}
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
		t.Errorf("matches = %+v, want one title match via the per-id fallback", res.Matches)
	}
}

// scopedBatchRecordAniList models a CHUNK-SCOPED record-local batch failure:
// FetchMany answers id 11, marks id 22 UNVERIFIED (its chunk answered
// untrustworthily), and marks id 33 ABSENT from a chunk that completed cleanly.
type scopedBatchRecordAniList struct {
	fetchedIDs []int
	batchCalls int
}

func (s *scopedBatchRecordAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	s.fetchedIDs = append(s.fetchedIDs, id)
	return anilist.Media{}, anilist.ErrNotFound
}

func (s *scopedBatchRecordAniList) FetchMany(_ context.Context, _ []int) (anilist.BatchResult, error) {
	s.batchCalls++
	return anilist.BatchResult{
			Media: map[int]anilist.Media{11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020}},
			Verdicts: map[int]anilist.Verdict{
				11: anilist.VerdictFound,
				22: anilist.VerdictUnverified,
				33: anilist.VerdictAbsent,
			},
		},
		&anilist.BatchRecordError{
			Err: fmt.Errorf("%w: media record 0 missing id", anilist.ErrBatchRecord),
		}
}

// TestPrefetchScopesNegativeMemoToVerifiedChunks pins the chunk-scoping half of
// the prefetch negative-memo rule: VerdictUnverified names the ids whose chunk
// is untrustworthy, so those stay uncached for the per-id retry while every
// OTHER requested-but-absent id was definitively answered by a clean chunk
// (VerdictAbsent) and IS memoized negatively. Both directions matter:
// memoizing an unverified id would cache a malformed record as not-found for a
// whole TTL, and refusing to memoize the verified ones dumps the entire pending
// set into rate-limited per-id fetches.
func TestPrefetchScopesNegativeMemoToVerifiedChunks(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // id-less: pending, answered by the batch
		{AniListID: 22, Type: "MOVIE"}, // id-less: pending, its chunk is unverified
		{AniListID: 33, Type: "MOVIE"}, // id-less: pending, absent from a CLEAN chunk
	})
	fake := &scopedBatchRecordAniList{}

	res := NewMatcher(fake, nil).Match(context.Background(),
		[]seadex.Entry{{AniListID: 11}, {AniListID: 22}, {AniListID: 33}},
		&library.Snapshot{}, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1", fake.batchCalls)
	}
	if len(fake.fetchedIDs) != 1 || fake.fetchedIDs[0] != 22 {
		t.Errorf("per-id Fetch ids = %v, want exactly [22]: only the untrustworthy chunk's id may be retried", fake.fetchedIDs)
	}
	if ent, ok := res.Memo.Entries[11]; !ok || ent.NotFound {
		t.Errorf("memo[11] = %+v (present=%v), want the batch-answered positive", ent, ok)
	}
	if ent, ok := res.Memo.Entries[33]; !ok || !ent.NotFound {
		t.Errorf("memo[33] = %+v (present=%v), want a negative: a clean chunk definitively answered it", ent, ok)
	}
	if ent, ok := res.Memo.Entries[22]; !ok || !ent.NotFound {
		t.Errorf("memo[22] = %+v (present=%v), want the per-id retry's definitive not-found", ent, ok)
	}
	if res.Degraded {
		t.Error("Degraded = true, want false: every id was definitively answered")
	}
}

// abortingBatchAniList models an ABORTED batch: its first FetchMany answers
// every id except an abandoned tail - the chunk that aborted and the ones after
// it, which the client never requested - marked VerdictUnrequested. A second
// call answers everything it is asked, so a re-batch of the tail succeeds.
type abortingBatchAniList struct {
	media      map[int]anilist.Media
	abandoned  []int
	batchSizes []int
	fetchCalls int
	batchCalls int
}

func (a *abortingBatchAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	a.fetchCalls++
	if m, ok := a.media[id]; ok {
		return m, nil
	}
	return anilist.Media{}, anilist.ErrNotFound
}

func (a *abortingBatchAniList) FetchMany(_ context.Context, ids []int) (anilist.BatchResult, error) {
	a.batchCalls++
	a.batchSizes = append(a.batchSizes, len(ids))
	aborted := a.batchCalls == 1
	out := make(map[int]anilist.Media, len(ids))
	for _, id := range ids {
		if aborted && slices.Contains(a.abandoned, id) {
			continue // its chunk was never requested
		}
		if m, ok := a.media[id]; ok {
			out[id] = m
		}
	}
	if aborted {
		verdicts := batchVerdicts(ids, out)
		for _, id := range a.abandoned {
			if _, requested := verdicts[id]; requested {
				verdicts[id] = anilist.VerdictUnrequested
			}
		}
		return anilist.BatchResult{Media: out, Verdicts: verdicts}, &anilist.BatchRecordError{
			Err: errors.New("anilist: 503 service unavailable"),
		}
	}
	return anilist.BatchResult{Media: out, Verdicts: batchVerdicts(ids, out)}, nil
}

// TestPrefetchReBatchesUnrequestedIDs pins the abort-recovery half of prefetch:
// the ids an aborted batch abandoned WITHOUT requesting are re-batched, not
// dropped to one rate-limited per-id Fetch each. Against a briefly-flaky
// upstream those per-id fetches succeed, so transientFailureCap never trips to
// stop them - an abort in an early chunk used to turn a handful of batched
// requests into one request per remaining id, the request storm batching exists
// to remove.
func TestPrefetchReBatchesUnrequestedIDs(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // id-less: answered by the first pass
		{AniListID: 22, Type: "MOVIE"}, // id-less: abandoned, must be re-batched
		{AniListID: 33, Type: "MOVIE"}, // id-less: abandoned, must be re-batched
	})
	fake := &abortingBatchAniList{
		media: map[int]anilist.Media{
			11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020},
			22: {Titles: []string{"Movie B"}, Format: "MOVIE", Year: 2021},
			33: {Titles: []string{"Movie C"}, Format: "MOVIE", Year: 2022},
		},
		abandoned: []int{22, 33},
	}

	res := NewMatcher(fake, nil).Match(context.Background(),
		[]seadex.Entry{{AniListID: 11}, {AniListID: 22}, {AniListID: 33}},
		&library.Snapshot{}, idx, Memo{})

	if fake.batchCalls != 2 {
		t.Errorf("batch calls = %d, want 2 (the abandoned tail is re-batched once)", fake.batchCalls)
	}
	if !slices.Equal(fake.batchSizes, []int{3, 2}) {
		t.Errorf("batch sizes = %v, want [3 2] (the second pass asks only the abandoned ids)", fake.batchSizes)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("single Fetch calls = %d, want 0 (a re-batch must replace the per-id fallback)", fake.fetchCalls)
	}
	for _, id := range []int{11, 22, 33} {
		ent, ok := res.Memo.Entries[id]
		if !ok || ent.NotFound || len(ent.Titles) == 0 {
			t.Errorf("memo[%d] = %+v (present=%v), want the batch-answered positive", id, ent, ok)
		}
	}
	if res.Degraded {
		t.Error("Degraded = true, want false: every id was answered, just across two passes")
	}
}

// TestPrefetchDoesNotReBatchRecordLocalIDs pins the other side of that rule: an
// id whose chunk DID answer but answered untrustworthily is NOT re-batched - the
// same poisoned record would come back - so it keeps the per-id Fetch that
// isolates it. scopedBatchRecordAniList names id 22 unverified with no
// unrequested tail.
func TestPrefetchDoesNotReBatchRecordLocalIDs(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"},
		{AniListID: 22, Type: "MOVIE"},
		{AniListID: 33, Type: "MOVIE"},
	})
	fake := &scopedBatchRecordAniList{}

	NewMatcher(fake, nil).Match(context.Background(),
		[]seadex.Entry{{AniListID: 11}, {AniListID: 22}, {AniListID: 33}},
		&library.Snapshot{}, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1 (a record-local chunk is not worth re-asking)", fake.batchCalls)
	}
	if len(fake.fetchedIDs) != 1 || fake.fetchedIDs[0] != 22 {
		t.Errorf("per-id Fetch ids = %v, want [22]: the untrustworthy chunk's id keeps the per-id retry", fake.fetchedIDs)
	}
}

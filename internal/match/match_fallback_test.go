package match

import (
	"context"
	"errors"
	"testing"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// partialBatchAniList models a PARTIAL batch failure (a later chunk failed):
// FetchMany answers the requested ids present in batchMedia but still returns
// an error, so unreturned ids stay uncached and fall through to the single
// Fetch, which answers from fetchMedia or a definitive not-found.
type partialBatchAniList struct {
	batchMedia map[int]anilist.Media
	fetchMedia map[int]anilist.Media
	fetchCalls int
	batchCalls int
}

func (b *partialBatchAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	b.fetchCalls++
	if m, ok := b.fetchMedia[id]; ok {
		return m, nil
	}
	return anilist.Media{}, anilist.ErrNotFound
}

func (b *partialBatchAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	b.batchCalls++
	out := make(map[int]anilist.Media)
	for _, id := range ids {
		if m, ok := b.batchMedia[id]; ok {
			out[id] = m
		}
	}
	return out, errors.New("anilist 500")
}

// TestMatchMemoizesNotFoundAfterFailedBatch pins the fallback chain behind a
// PARTIALLY failed prefetch batch (id 66 returned, the error hitting id 77's
// chunk): the unreturned id is left uncached (never negatively memoized on a
// batch ERROR), the per-entry pass retries it with a single Fetch, and a
// definitive ErrNotFound from that retry is memoized negatively WITHOUT
// flagging the cycle degraded (a not-found is an answer, not an outage).
func TestMatchMemoizesNotFoundAfterFailedBatch(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 66, Type: "MOVIE"}, // id-less: needs the title fallback
		{AniListID: 77, Type: "MOVIE"}, // id-less: needs the title fallback
	})
	fake := &partialBatchAniList{batchMedia: map[int]anilist.Media{
		66: {Titles: []string{"Returned"}, Format: "MOVIE", Year: 2020},
	}}
	m := NewMatcher(fake, nil)

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 66}, {AniListID: 77}}, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1", fake.batchCalls)
	}
	if fake.fetchCalls != 1 {
		t.Errorf("single Fetch calls = %d, want 1 (a partial batch leaves the unreturned id for the per-entry retry)", fake.fetchCalls)
	}
	if ent, ok := res.Memo.Entries[77]; !ok || !ent.NotFound {
		t.Errorf("memo[77] = %+v (present=%v), want a NotFound negative entry", ent, ok)
	}
	if res.Degraded {
		t.Error("Degraded = true, want false: a definitive not-found after a partial batch is not an outage")
	}
	if len(res.Matches) != 2 || res.Matches[1].Source != SourceUnmapped {
		t.Errorf("matches = %+v, want two entries with the retried one unmapped", res.Matches)
	}
}

// TestPendingAniListIDsDedupesAndSkipsInvalid pins the batch worklist guards:
// a duplicate AniList id is requested once, a non-positive id is never
// requested, and an already-memoized id (positive or negative) is skipped.
func TestPendingAniListIDsDedupesAndSkipsInvalid(t *testing.T) {
	idx := mapping.NewIndex(nil)
	lib := buildLibIndex(&library.Snapshot{})
	memo := Memo{Entries: map[int]MemoEntry{88: {NotFound: true}}}
	entries := []seadex.Entry{
		{AniListID: 77},
		{AniListID: 77}, // duplicate: requested once
		{AniListID: 0},  // non-positive: never requested
		{AniListID: 88}, // memoized: skipped
	}

	got := pendingAniListIDs(entries, idx, lib, &memo)

	if len(got) != 1 || got[0] != 77 {
		t.Errorf("pendingAniListIDs = %v, want [77]", got)
	}
}

// TestMatchSingleFetchRecoversAfterFailedBatch pins the batch-fallback recovery
// chain: a PARTIALLY failed prefetch batch (id 22 returned, the error hitting
// id 11's chunk) leaves the unreturned id uncached, the per-entry pass retries
// it with a single Fetch, and a SUCCESSFUL retry memoizes the positive result
// and still title-matches the entry, without degrading the cycle.
func TestMatchSingleFetchRecoversAfterFailedBatch(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Movie A", Year: 2020},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // id-less: needs the title fallback
		{AniListID: 22, Type: "MOVIE"}, // id-less: needs the title fallback
	})
	fake := &partialBatchAniList{
		batchMedia: map[int]anilist.Media{
			22: {Titles: []string{"Returned"}, Format: "MOVIE", Year: 2021},
		},
		fetchMedia: map[int]anilist.Media{
			11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020},
		},
	}

	res := NewMatcher(fake, nil).Match(context.Background(), []seadex.Entry{{AniListID: 11}, {AniListID: 22}}, snap, idx, Memo{})

	if fake.batchCalls != 1 || fake.fetchCalls != 1 {
		t.Errorf("calls = batch %d / fetch %d, want 1 / 1 (partial batch falls back to one single Fetch)", fake.batchCalls, fake.fetchCalls)
	}
	ent, ok := res.Memo.Entries[11]
	if !ok || ent.NotFound || len(ent.Titles) == 0 || ent.Titles[0] != "Movie A" {
		t.Errorf("memo[11] = %+v (present=%v), want the positive Movie A entry memoized", ent, ok)
	}
	if res.Degraded {
		t.Error("Degraded = true, want false: the single-Fetch retry succeeded")
	}
	if len(res.Matches) != 2 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
		t.Errorf("matches = %+v, want a title match to the Radarr movie for the retried entry", res.Matches)
	}
}

// totalOutageAniList fails every batch entirely (zero chunks succeed) and
// counts single Fetch calls, modelling a full AniList outage on a cold cycle.
type totalOutageAniList struct {
	fetchCalls int
	batchCalls int
}

func (o *totalOutageAniList) Fetch(context.Context, int) (anilist.Media, error) {
	o.fetchCalls++
	return anilist.Media{}, errors.New("anilist 500")
}

func (o *totalOutageAniList) FetchMany(context.Context, []int) (map[int]anilist.Media, error) {
	o.batchCalls++
	return nil, errors.New("anilist 500")
}

// TestMatchTotalBatchOutageSkipsPerIDFallback pins the fast-degrade contract:
// when the batch prefetch fails ENTIRELY (zero chunks succeeded - an AniList
// outage), the per-entry pass must NOT regress to one doomed per-id request
// per pending id; the pending ids fail immediately through the existing
// degradation accounting (Degraded set, entries unmapped, nothing memoized so
// next cycle retries). Partial-failure fallback semantics are pinned by
// TestMatchMemoizesNotFoundAfterFailedBatch above.
func TestMatchTotalBatchOutageSkipsPerIDFallback(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // id-less: needs the title fallback
		{AniListID: 22, Type: "MOVIE"}, // id-less: needs the title fallback
	})
	fake := &totalOutageAniList{}

	res := NewMatcher(fake, nil).Match(context.Background(),
		[]seadex.Entry{{AniListID: 11}, {AniListID: 22}}, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1", fake.batchCalls)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("single Fetch calls = %d, want 0 (a total outage must not produce a per-id request tail)", fake.fetchCalls)
	}
	if !res.Degraded {
		t.Error("Degraded = false, want true: pending ids failed against the outage")
	}
	if len(res.Matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(res.Matches))
	}
	for i := range res.Matches {
		if res.Matches[i].InLibrary() || res.Matches[i].Source != SourceUnmapped {
			t.Errorf("match %d = %+v, want unmapped", i, res.Matches[i])
		}
	}
	if len(res.Memo.Entries) != 0 {
		t.Errorf("memo = %+v, want empty (outage-failed ids must be retried next cycle)", res.Memo.Entries)
	}
}

// midBatchOutageAniList models an AniList outage that begins AFTER the first
// prefetch chunk succeeds: FetchMany returns the first requested id's media
// together with an error (a PARTIAL batch failure), and every subsequent
// single Fetch fails transiently.
type midBatchOutageAniList struct {
	fetchCalls int
	batchCalls int
}

func (o *midBatchOutageAniList) Fetch(context.Context, int) (anilist.Media, error) {
	o.fetchCalls++
	return anilist.Media{}, errors.New("anilist 500")
}

func (o *midBatchOutageAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	o.batchCalls++
	return map[int]anilist.Media{ids[0]: {Titles: []string{"Returned"}, Format: "TV"}}, errors.New("anilist 500")
}

// TestMatchMidBatchOutageTripsFastFailBreaker pins the consecutive-failure
// breaker: an outage that begins mid-batch looks like a PARTIAL failure to
// prefetch, so the per-entry pass starts retrying uncached ids one by one -
// but after transientFailureCap consecutive transient failures the breaker
// trips and every remaining uncached id fails fast (no further per-id
// requests: no unbounded futile tail), through the existing degradation
// accounting (Degraded set, failed ids un-memoized so next cycle retries).
func TestMatchMidBatchOutageTripsFastFailBreaker(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex(nil) // no records: every entry needs the AniList lookup
	fake := &midBatchOutageAniList{}
	entries := []seadex.Entry{
		{AniListID: 10}, // returned by the partial batch: memoized, no per-id retry
		{AniListID: 20}, // transient failure 1
		{AniListID: 30}, // transient failure 2
		{AniListID: 40}, // transient failure 3: trips the breaker
		{AniListID: 50}, // breaker tripped: fails fast, no request
		{AniListID: 60}, // breaker tripped: fails fast, no request
	}

	res := NewMatcher(fake, nil).Match(context.Background(), entries, snap, idx, Memo{})

	if fake.batchCalls != 1 {
		t.Errorf("batch calls = %d, want 1", fake.batchCalls)
	}
	if fake.fetchCalls != transientFailureCap {
		t.Errorf("single Fetch calls = %d, want %d (the breaker must stop the futile per-id tail)",
			fake.fetchCalls, transientFailureCap)
	}
	if !res.Degraded {
		t.Error("Degraded = false, want true: needed lookups failed against the outage")
	}
	if len(res.Memo.Entries) != 1 {
		t.Errorf("memo = %+v, want only the batch-returned id memoized (failed ids retried next cycle)", res.Memo.Entries)
	}
	if ent, ok := res.Memo.Entries[10]; !ok || ent.NotFound {
		t.Errorf("memo[10] = %+v (present=%v), want the positive batch-returned entry", ent, ok)
	}
	for i := range res.Matches {
		if res.Matches[i].InLibrary() {
			t.Errorf("match %d = %+v, want unmapped (empty library)", i, res.Matches[i])
		}
	}
}

// recoveringAniList models an upstream that recovers mid-outage: the batch
// prefetch is PARTIAL (first id returned + error), per-id Fetch fails
// transiently for every id except 40, which succeeds.
type recoveringAniList struct{ fetchCalls int }

func (a *recoveringAniList) Fetch(_ context.Context, id int) (anilist.Media, error) {
	a.fetchCalls++
	if id == 40 {
		return anilist.Media{Titles: []string{"Recovered"}, Format: "TV"}, nil
	}
	return anilist.Media{}, errors.New("anilist 500")
}

func (*recoveringAniList) FetchMany(_ context.Context, ids []int) (map[int]anilist.Media, error) {
	return map[int]anilist.Media{ids[0]: {Titles: []string{"Returned"}, Format: "TV"}}, errors.New("anilist 500")
}

// TestMatchSuccessfulLookupResetsFailureBreaker pins recordSuccess's streak
// reset: a successful per-id lookup after two transient failures must reset
// the consecutive-failure breaker, so a recovered upstream does not trip it
// early. With the reset removed, id 40's success leaves the streak at 2, id 50
// trips the breaker (streak 3), and id 60 fails fast - 4 Fetch calls instead
// of the 5 a resetting breaker allows.
func TestMatchSuccessfulLookupResetsFailureBreaker(t *testing.T) {
	fake := &recoveringAniList{}
	entries := []seadex.Entry{{AniListID: 10}, {AniListID: 20}, {AniListID: 30}, {AniListID: 40}, {AniListID: 50}, {AniListID: 60}}

	res := NewMatcher(fake, nil).Match(context.Background(), entries, &library.Snapshot{}, mapping.NewIndex(nil), Memo{})

	if fake.fetchCalls != 5 {
		t.Errorf("single Fetch calls = %d, want 5: success after two failures must reset the breaker streak", fake.fetchCalls)
	}
	if _, ok := res.Memo.Entries[40]; !ok {
		t.Error("successful recovery was not memoized")
	}
	if !res.Degraded {
		t.Error("Degraded = false, want true because transient failures occurred")
	}
}

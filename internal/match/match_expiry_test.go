package match

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// memoTestClock is the fixed instant the expiry tests run at; entries are
// stamped and compared against it through the Matcher's injected clock.
var memoTestClock = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// expiryMatcher builds a Matcher over client with a fixed clock and a scripted
// jitter sequence: the i-th rand draw returns draws[i%len(draws)], so every
// stamped expiry is exact and deterministic — no sleeps, no real randomness.
func expiryMatcher(client AniListClient, draws ...float64) *Matcher {
	m := NewMatcher(client, nil)
	m.now = func() time.Time { return memoTestClock }
	i := 0
	m.rand = func() float64 {
		v := draws[i%len(draws)]
		i++
		return v
	}
	return m
}

// TestMemoStampsJitteredExpiryOnNewEntries pins the write-side policy: every
// entry a Match pass writes — batch-prefetched positives AND not-found
// negatives — gets its own uniform random expiry in [now+memoTTLMin,
// now+memoTTLMax), each from a separate jitter draw, so entries written by the
// same batch still expire staggered.
func TestMemoStampsJitteredExpiryOnNewEntries(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // id-less: needs the lookup; the batch returns it
		{AniListID: 22, Type: "MOVIE"}, // id-less: the batch omits it -> negative
	})
	fake := &batchCountingAniList{media: map[int]anilist.Media{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020},
	}}
	m := expiryMatcher(fake, 0, 0.5)

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 11}, {AniListID: 22}}, snap, idx, Memo{})

	// Prefetch stamps in pending-id (entry) order: id 11 draws 0 (the window
	// floor), id 22 draws 0.5 (the 14-day mean).
	if got, want := res.Memo.Entries[11].Expiry, memoTestClock.Add(memoTTLMin); !got.Equal(want) {
		t.Errorf("memo[11].Expiry = %s, want the window floor %s", got, want)
	}
	if got, want := res.Memo.Entries[22].Expiry, memoTestClock.Add(memoTTLMin+(memoTTLMax-memoTTLMin)/2); !got.Equal(want) {
		t.Errorf("memo[22].Expiry = %s, want the 14-day mean %s", got, want)
	}
	if !res.Memo.Entries[22].NotFound {
		t.Error("memo[22].NotFound = false, want true (negatives carry the same expiry policy)")
	}
}

// TestMemoStampsExpiryOnSingleFetchWrites pins the per-id write sites (the
// paths behind a partially failed batch): both the positive single-Fetch
// renewal and the definitive not-found negative are stamped with a jittered
// expiry, so no write site can produce an immortal (zero-expiry) entry.
func TestMemoStampsExpiryOnSingleFetchWrites(t *testing.T) {
	snap := &library.Snapshot{}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // partial batch returns it
		{AniListID: 22, Type: "MOVIE"}, // batch error hits it: single Fetch -> positive
		{AniListID: 33, Type: "MOVIE"}, // batch error hits it: single Fetch -> not-found
	})
	fake := &partialBatchAniList{
		batchMedia: map[int]anilist.Media{11: {Titles: []string{"Returned"}, Format: "MOVIE"}},
		fetchMedia: map[int]anilist.Media{22: {Titles: []string{"Recovered"}, Format: "MOVIE"}},
	}
	m := expiryMatcher(fake, 0.5)

	res := m.Match(context.Background(),
		[]seadex.Entry{{AniListID: 11}, {AniListID: 22}, {AniListID: 33}}, snap, idx, Memo{})

	for _, id := range []int{11, 22, 33} {
		ent, ok := res.Memo.Entries[id]
		if !ok {
			t.Errorf("memo[%d] missing, want a stamped entry", id)
			continue
		}
		if want := memoTestClock.Add(memoTTLMin + (memoTTLMax-memoTTLMin)/2); !ent.Expiry.Equal(want) {
			t.Errorf("memo[%d].Expiry = %s, want %s (every write site stamps)", id, ent.Expiry, want)
		}
	}
	if !res.Memo.Entries[33].NotFound {
		t.Error("memo[33].NotFound = false, want the single-Fetch negative memoized")
	}
}

// TestMemoExpiredEntryRefetchedAndRestamped pins lazy expiry end to end: an
// expired entry — negative or positive — is a lookup miss, so the id re-enters
// the batched prefetch (zero per-id requests), the fresh AniList answer
// replaces the stale one, and the entry is re-stamped with a fresh jittered
// expiry. This is the exact staleness the TTL exists to fix: a show created on
// AniList after the negative was cached (id 11), and an English title added
// after the positive was cached (id 22).
func TestMemoExpiredEntryRefetchedAndRestamped(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Found Later", TmdbID: 100, Year: 2020},
		{Arr: library.ArrRadarr, ArrID: 2, Title: "New Title", TmdbID: 200, Year: 2021},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 11, Type: "MOVIE"}, // id-less: needs the lookup
		{AniListID: 22, Type: "MOVIE"}, // id-less: needs the lookup
	})
	fake := &batchCountingAniList{media: map[int]anilist.Media{
		11: {Titles: []string{"Found Later"}, Format: "MOVIE", Year: 2020},
		22: {Titles: []string{"New Title"}, Format: "MOVIE", Year: 2021},
	}}
	m := expiryMatcher(fake, 0, 0.5)
	memo := Memo{Entries: map[int]MemoEntry{
		// Stale negative: the show did not exist on AniList when cached.
		11: {NotFound: true, Expiry: memoTestClock.Add(-time.Minute)},
		// Stale positive whose expiry is EXACTLY now: the boundary is expired.
		22: {Titles: []string{"Old Title"}, Format: "MOVIE", Year: 2021, Expiry: memoTestClock},
	}}

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 11}, {AniListID: 22}}, snap, idx, memo)

	if fake.batchCalls != 1 || fake.fetchCalls != 0 {
		t.Errorf("calls = batch %d / fetch %d, want 1 / 0 (expired entries renew through the batch prefetch)", fake.batchCalls, fake.fetchCalls)
	}
	ent11 := res.Memo.Entries[11]
	if ent11.NotFound || len(ent11.Titles) != 1 || ent11.Titles[0] != "Found Later" {
		t.Errorf("memo[11] = %+v, want the expired negative replaced by the fresh positive", ent11)
	}
	if want := memoTestClock.Add(memoTTLMin); !ent11.Expiry.Equal(want) {
		t.Errorf("memo[11].Expiry = %s, want re-stamped %s", ent11.Expiry, want)
	}
	ent22 := res.Memo.Entries[22]
	if len(ent22.Titles) != 1 || ent22.Titles[0] != "New Title" {
		t.Errorf("memo[22].Titles = %v, want the fresh AniList title", ent22.Titles)
	}
	if want := memoTestClock.Add(memoTTLMin + (memoTTLMax-memoTTLMin)/2); !ent22.Expiry.Equal(want) {
		t.Errorf("memo[22].Expiry = %s, want re-stamped %s", ent22.Expiry, want)
	}
	for i := range res.Matches {
		if !res.Matches[i].InLibrary() || res.Matches[i].Source != SourceTitle {
			t.Errorf("match %d = %+v, want a title match through the renewed entry", i, res.Matches[i])
		}
	}
}

// TestMemoUnexpiredEntryServedWithoutRefetch pins the hit side of the TTL: a
// live (unexpired) entry answers from the memo with zero AniList requests —
// neither a batch nor a per-id fetch — and keeps its original expiry (reads
// never re-stamp, so an entry cannot live forever by being used).
func TestMemoUnexpiredEntryServedWithoutRefetch(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Movie A", TmdbID: 100, Year: 2020},
	}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 11, Type: "MOVIE"}})
	fake := &countingAniList{}
	m := expiryMatcher(fake, 0.5)
	expiry := memoTestClock.Add(time.Minute) // one minute of life left: still a hit
	memo := Memo{Entries: map[int]MemoEntry{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020, Expiry: expiry},
	}}

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 11}}, snap, idx, memo)

	if fake.calls != 0 {
		t.Errorf("AniList calls = %d, want 0 (a live entry is served from the memo)", fake.calls)
	}
	ent := res.Memo.Entries[11]
	if !ent.Expiry.Equal(expiry) {
		t.Errorf("memo[11].Expiry = %s, want the original %s (reads never re-stamp)", ent.Expiry, expiry)
	}
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
		t.Errorf("matches = %+v, want the memoized title match", res.Matches)
	}
}

// TestMemoPruneDropsExpiredUnrenewedKeepsLive pins the save-side hygiene: an
// already-expired entry this pass neither consulted nor renewed is dropped
// from the returned memo (it is a miss either way; next cycle's batch
// re-fetches it if it is ever needed again), while a live unconsulted entry
// survives untouched. Pruning itself spends no AniList requests.
func TestMemoPruneDropsExpiredUnrenewedKeepsLive(t *testing.T) {
	fake := &countingAniList{}
	m := expiryMatcher(fake, 0.5)
	live := memoTestClock.Add(48 * time.Hour)
	memo := Memo{Entries: map[int]MemoEntry{
		901: {NotFound: true, Expiry: memoTestClock.Add(-time.Hour)},                     // expired, unconsulted: pruned
		902: {Titles: []string{"Kept"}, Format: "TV", Year: 2020, Expiry: live},          // live, unconsulted: kept
		903: {Titles: []string{"Gone"}, Format: "TV", Year: 2021, Expiry: memoTestClock}, // boundary: expired, pruned
	}}

	res := m.Match(context.Background(), nil, &library.Snapshot{}, mapping.NewIndex(nil), memo)

	if _, ok := res.Memo.Entries[901]; ok {
		t.Error("expired unrenewed entry 901 survived the pass, want it pruned from the persisted memo")
	}
	if _, ok := res.Memo.Entries[903]; ok {
		t.Error("boundary-expired entry 903 survived the pass, want it pruned")
	}
	ent, ok := res.Memo.Entries[902]
	if !ok || !ent.Expiry.Equal(live) {
		t.Errorf("live entry 902 = %+v (present=%v), want kept with its expiry untouched", ent, ok)
	}
	if fake.calls != 0 {
		t.Errorf("AniList calls = %d, want 0 (pruning never fetches)", fake.calls)
	}
}

// TestMemoLegacyEntriesMigratedWithSpread pins the migration: entries
// persisted before the expiry policy (zero Expiry) are stamped on first load
// with per-entry draws from the wider [memoMigrationMin, memoTTLMax) window —
// distinct expiries so the backlog's first renewal spreads out — and a
// consulted legacy entry stays a memo HIT (zero AniList requests): migration
// must not turn the whole backlog into a day-one re-fetch stampede. The
// stamps land in the returned memo, so one successful cycle persists them.
func TestMemoLegacyEntriesMigratedWithSpread(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Movie A", TmdbID: 100, Year: 2020},
	}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 11, Type: "MOVIE"}})
	fake := &countingAniList{}
	m := expiryMatcher(fake, 0, 0.5, 0.25)
	memo := Memo{Entries: map[int]MemoEntry{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020}, // legacy, consulted this pass
		12: {NotFound: true},                                           // legacy negative, unconsulted
		13: {Titles: []string{"Other"}, Format: "TV", Year: 2019},      // legacy positive, unconsulted
	}}

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 11}}, snap, idx, memo)

	if fake.calls != 0 {
		t.Errorf("AniList calls = %d, want 0: migration must not re-fetch the legacy backlog", fake.calls)
	}
	if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
		t.Errorf("matches = %+v, want the legacy entry served as a normal memo hit", res.Matches)
	}
	if len(res.Memo.Entries) != 3 {
		t.Fatalf("memo entries = %d, want all 3 legacy entries kept (migrated, not pruned)", len(res.Memo.Entries))
	}
	// Map iteration order randomizes which entry receives which draw, so
	// assert the SET of stamped expiries equals the scripted draws.
	want := map[time.Time]bool{
		memoTestClock.Add(memoMigrationMin):                                   false,
		memoTestClock.Add(memoMigrationMin + (memoTTLMax-memoMigrationMin)/2): false,
		memoTestClock.Add(memoMigrationMin + (memoTTLMax-memoMigrationMin)/4): false,
	}
	lo, hi := memoTestClock.Add(memoMigrationMin), memoTestClock.Add(memoTTLMax)
	for id, ent := range res.Memo.Entries {
		if ent.Expiry.Before(lo) || !ent.Expiry.Before(hi) {
			t.Errorf("memo[%d].Expiry = %s, want inside [%s, %s)", id, ent.Expiry, lo, hi)
		}
		seen, ok := want[ent.Expiry]
		if !ok {
			t.Errorf("memo[%d].Expiry = %s, not one of the scripted migration draws", id, ent.Expiry)
			continue
		}
		if seen {
			t.Errorf("memo[%d].Expiry = %s duplicated: each legacy entry must draw its own stagger", id, ent.Expiry)
		}
		want[ent.Expiry] = true
	}
}

// TestMemoEntryExpiryWireFormat pins the persisted field contract: Expiry
// round-trips through JSON under the "expiry" key, a legacy record without
// the key decodes to a zero Expiry (the migration trigger), and a zero Expiry
// is omitted on encode (omitzero) so an unmigrated in-memory entry never
// persists a fake 0001-01-01 stamp.
func TestMemoEntryExpiryWireFormat(t *testing.T) {
	expiry := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	out, err := json.Marshal(MemoEntry{NotFound: true, Expiry: expiry})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"expiry":"2026-07-15T12:00:00Z"`) {
		t.Errorf("encoded entry = %s, want an RFC3339 expiry field", out)
	}
	var back MemoEntry
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Expiry.Equal(expiry) || !back.NotFound {
		t.Errorf("round-tripped entry = %+v, want expiry %s and the negative preserved", back, expiry)
	}

	var legacy MemoEntry
	if err := json.Unmarshal([]byte(`{"titles":["Frieren"],"format":"TV","year":2023}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if !legacy.Expiry.IsZero() {
		t.Errorf("legacy Expiry = %s, want zero (the migration trigger)", legacy.Expiry)
	}
	zeroOut, err := json.Marshal(MemoEntry{NotFound: true})
	if err != nil {
		t.Fatalf("marshal zero-expiry: %v", err)
	}
	if strings.Contains(string(zeroOut), "expiry") {
		t.Errorf("zero-expiry encoding = %s, want the expiry key omitted", zeroOut)
	}
}


// TestMemoDegradedPassRetainsExpiredEntries pins the prune guard (h-f8): a
// degraded pass (here a total AniList outage) could not renew what expired,
// so it must NOT prune the expired entries — the feed's stale-title tier
// (scout/feedinfo.go) still serves them, and they stay pending for next
// cycle's batch either way.
func TestMemoDegradedPassRetainsExpiredEntries(t *testing.T) {
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 11, Type: "MOVIE"}}) // id-less: needs the lookup
	m := expiryMatcher(degradedAniList{}, 0.5)
	memo := Memo{Entries: map[int]MemoEntry{
		11: {Titles: []string{"Stale Title"}, Format: "MOVIE", Year: 2020, Expiry: memoTestClock.Add(-time.Hour)},
	}}

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 11}}, &library.Snapshot{}, idx, memo)

	if !res.Degraded {
		t.Fatal("Degraded = false, want true on a total AniList outage")
	}
	ent, ok := res.Memo.Entries[11]
	if !ok {
		t.Fatal("expired entry 11 was pruned on a degraded pass; want it retained for the feed's stale-title tier")
	}
	if len(ent.Titles) != 1 || ent.Titles[0] != "Stale Title" {
		t.Errorf("memo[11] = %+v, want the stale entry retained verbatim", ent)
	}
}

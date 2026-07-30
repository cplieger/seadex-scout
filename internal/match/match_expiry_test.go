package match

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
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
// negatives — gets its own uniform random expiry in [now+memoMinTTL,
// now+memoMaxTTL), each from a separate jitter draw, so entries written by the
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
	if got, want := res.Memo.Entries[11].Expiry, memoTestClock.Add(memoMinTTL); !got.Equal(want) {
		t.Errorf("memo[11].Expiry = %s, want the window floor %s", got, want)
	}
	if got, want := res.Memo.Entries[22].Expiry, memoTestClock.Add(memoMinTTL+(memoMaxTTL-memoMinTTL)/2); !got.Equal(want) {
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
		if want := memoTestClock.Add(memoMinTTL + (memoMaxTTL-memoMinTTL)/2); !ent.Expiry.Equal(want) {
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
	if want := memoTestClock.Add(memoMinTTL); !ent11.Expiry.Equal(want) {
		t.Errorf("memo[11].Expiry = %s, want re-stamped %s", ent11.Expiry, want)
	}
	ent22 := res.Memo.Entries[22]
	if len(ent22.Titles) != 1 || ent22.Titles[0] != "New Title" {
		t.Errorf("memo[22].Titles = %v, want the fresh AniList title", ent22.Titles)
	}
	if want := memoTestClock.Add(memoMinTTL + (memoMaxTTL-memoMinTTL)/2); !ent22.Expiry.Equal(want) {
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
// already-expired entry the pass neither consulted nor renewed is dropped from
// the memo (it is a miss either way; the next batch re-fetches it if it is ever
// needed again), while a live unconsulted entry survives untouched. The
// catalogue is empty here, so nothing is held back for the feed's stale tier
// (that retention is TestMemoPruneKeepsExpiredStaleDataForCuratedEntries).
// Pruning itself spends no AniList requests.
//
// It is an EXPLICIT call by the pass that holds a catalogue, not something Match
// does on the way out - see PruneMemo.
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
	m.PruneMemo(&res, nil)

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

// TestMemoPruneKeepsExpiredStaleDataForCuratedEntries pins the retention half
// of the save-side hygiene: the match is not the memo's only reader, so an
// expired POSITIVE whose AniList id SeaDex still curates survives the prune
// because Memo.StaleTitle/StaleFormat (which ignore expiry by design) feed the
// indexer feed's title and category tiers for exactly that set. Everything else
// expired still goes: an entry for an id absent from the catalogue is dead cache
// data, and an expired negative carries nothing the stale tier could serve.
func TestMemoPruneKeepsExpiredStaleDataForCuratedEntries(t *testing.T) {
	expired := memoTestClock.Add(-time.Hour)
	live := memoTestClock.Add(48 * time.Hour)
	memo := Memo{Entries: map[int]MemoEntry{
		1: {Titles: []string{"Curated"}, Format: "TV", Year: 2020, Expiry: expired}, // curated positive: kept
		2: {Format: "MOVIE", Expiry: expired},                                       // curated, format only: kept
		3: {Titles: []string{"Dropped"}, Format: "TV", Expiry: expired},             // uncurated: pruned
		4: {NotFound: true, Expiry: expired},                                        // curated negative: pruned
		5: {Expiry: expired},                                                        // curated but empty: pruned
		6: {Titles: []string{"Live"}, Expiry: live},                                 // live: kept
	}}
	entries := []seadex.Entry{{AniListID: 1}, {AniListID: 2}, {AniListID: 4}, {AniListID: 5}}

	pruneExpired(&memo, memoTestClock, entries)

	for _, id := range []int{1, 2, 6} {
		if _, ok := memo.Entries[id]; !ok {
			t.Errorf("entry %d was pruned, want it kept for the feed's stale title/type tier", id)
		}
	}
	for _, id := range []int{3, 4, 5} {
		if _, ok := memo.Entries[id]; ok {
			t.Errorf("entry %d survived, want it pruned (nothing the stale tier can serve)", id)
		}
	}
	if title, _, ok := memo.StaleTitle(1); !ok || title != "Curated" {
		t.Errorf("StaleTitle(1) = %q (ok=%v), want the retained stale title", title, ok)
	}
	if format, ok := memo.StaleFormat(2); !ok || format != "MOVIE" {
		t.Errorf("StaleFormat(2) = %q (ok=%v), want the retained stale format", format, ok)
	}
}

// TestMemoEntryWithoutAnExpiryIsRefetchedNotServed pins the absence of a
// migration, which is a deliberate policy and not an oversight: this app ships
// no old-to-new conversion for persisted state.
//
// An entry written by a build older than the expiry policy carries no expiry at
// all. Rather than being stamped and served, it reads as expired: consulted, it
// is re-fetched and re-stamped like any other miss; unconsulted, it is pruned at
// the end of a clean pass. The cost is a one-time re-fetch of whatever the memo
// held, which the batched prefetch amortizes; the benefit is that there is no
// conversion path to carry, test, or get wrong.
func TestMemoEntryWithoutAnExpiryIsRefetchedNotServed(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrRadarr, ArrID: 1, Title: "Movie A", TmdbID: 100, Year: 2020},
	}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 11, Type: "MOVIE"}})
	fake := &countingAniList{}
	m := expiryMatcher(fake, 0.5)
	memo := Memo{Entries: map[int]MemoEntry{
		11: {Titles: []string{"Movie A"}, Format: "MOVIE", Year: 2020}, // consulted this pass
		12: {NotFound: true},                                           // unconsulted negative
		13: {Titles: []string{"Other"}, Format: "TV", Year: 2019},      // unconsulted positive
	}}

	entries := []seadex.Entry{{AniListID: 11}}
	res := m.Match(context.Background(), entries, snap, idx, memo)
	m.PruneMemo(&res, entries)

	if fake.calls == 0 {
		t.Error("AniList calls = 0, want the unstamped entry treated as a miss and re-fetched")
	}
	ent, ok := res.Memo.Entries[11]
	if !ok {
		t.Fatalf("memo = %+v, want the consulted entry re-stamped and kept", res.Memo.Entries)
	}
	lo, hi := memoTestClock.Add(memoMinTTL), memoTestClock.Add(memoMaxTTL)
	if ent.Expiry.Before(lo) || !ent.Expiry.Before(hi) {
		t.Errorf("memo[11].Expiry = %s, want a FRESH stamp inside [%s, %s) - the same window any other write draws from", ent.Expiry, lo, hi)
	}
	// The unconsulted pair was expired at the end of a clean pass and neither id
	// is curated by this pass's entries, so nothing retains them.
	for _, id := range []int{12, 13} {
		if _, kept := res.Memo.Entries[id]; kept {
			t.Errorf("memo[%d] survived, want an unstamped unconsulted entry pruned like any other expired one", id)
		}
	}
}

// TestMemoEntryExpiryWireFormat pins the persisted field contract: Expiry
// round-trips through JSON under the "expiry" key, a record written before the
// expiry policy decodes to a zero Expiry (which reads as expired, so it is
// re-fetched rather than migrated), and a zero Expiry is omitted on encode
// (omitzero) so an unstamped in-memory entry never persists a fake 0001-01-01
// stamp.
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

	var unstamped MemoEntry
	if err := json.Unmarshal([]byte(`{"titles":["Frieren"],"format":"TV","year":2023}`), &unstamped); err != nil {
		t.Fatalf("unmarshal a pre-policy record: %v", err)
	}
	if !unstamped.Expiry.IsZero() {
		t.Errorf("Expiry of a record with no expiry key = %s, want zero (which expired() reads as expired)", unstamped.Expiry)
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

// TestMemoStaleTitleUsesExpiredPositiveEntry pins StaleTitle's deliberate
// expiry-ignorance: the memo expiry governs re-fetch cadence only, so an
// expired positive entry must still yield its title/year for the feed's
// title tier (a stale show title beats a file-name derivation).
func TestMemoStaleTitleUsesExpiredPositiveEntry(t *testing.T) {
	memo := Memo{Entries: map[int]MemoEntry{
		42: {
			Titles: []string{"Frieren: Beyond Journey's End"},
			Year:   2023,
			Expiry: memoTestClock.Add(-time.Hour),
		},
	}}

	title, year, ok := memo.StaleTitle(42)
	if !ok {
		t.Fatal("StaleTitle(42) ok = false, want true for an expired positive entry")
	}
	if title != "Frieren: Beyond Journey's End" || year != 2023 {
		t.Errorf("StaleTitle(42) = (%q, %d), want (%q, %d)", title, year, "Frieren: Beyond Journey's End", 2023)
	}
}

// TestMemoStaleTitleRejectsUnusableEntries pins the three unusable shapes:
// a not-found negative, a title-less entry, and an absent id all return zero
// values and false, never a fabricated title.
func TestMemoStaleTitleRejectsUnusableEntries(t *testing.T) {
	memo := Memo{Entries: map[int]MemoEntry{
		1: {Titles: []string{"Negative"}, Year: 2020, NotFound: true, Expiry: memoTestClock.Add(time.Hour)},
		2: {Year: 2021, Expiry: memoTestClock.Add(time.Hour)},
	}}
	tests := map[string]int{
		"not found":    1,
		"empty titles": 2,
		"absent":       3,
	}
	for name, id := range tests {
		t.Run(name, func(t *testing.T) {
			title, year, ok := memo.StaleTitle(id)
			if ok || title != "" || year != 0 {
				t.Errorf("StaleTitle(%d) = (%q, %d, %v), want zero values and false", id, title, year, ok)
			}
		})
	}
}

// TestMemoStaleFormatUsesExpiredEntryAndRejectsUnusable pins StaleFormat, the
// typing half of the feed's stale tier (scout/feedinfo.go reads it to route an
// unmapped entry's RSS category): an EXPIRED positive still yields its format
// (expiry governs re-fetch cadence only, exactly as for StaleTitle), while a
// not-found negative, an entry AniList gave no format for, and an absent id all
// read as absent rather than as false typing evidence.
func TestMemoStaleFormatUsesExpiredEntryAndRejectsUnusable(t *testing.T) {
	memo := Memo{Entries: map[int]MemoEntry{
		1: {Titles: []string{"Expired Movie"}, Format: "MOVIE", Year: 2020, Expiry: memoTestClock.Add(-time.Hour)},
		2: {Format: "MOVIE", NotFound: true, Expiry: memoTestClock.Add(time.Hour)},
		3: {Titles: []string{"No Format"}, Year: 2021, Expiry: memoTestClock.Add(time.Hour)},
	}}
	tests := map[string]struct {
		id     int
		want   string
		wantOK bool
	}{
		"expired positive still types the item": {id: 1, want: "MOVIE", wantOK: true},
		"not-found negative":                    {id: 2},
		"entry AniList gave no format for":      {id: 3},
		"absent id":                             {id: 4},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := memo.StaleFormat(tc.id)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("StaleFormat(%d) = (%q, %v), want (%q, %v)", tc.id, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestMemoExpiryBeyondHorizonRestamped pins migrateMemo's second normalization
// arm: an expiry further out than any this policy can stamp (a clock skew when
// the entry was written, or an edited state.json) is re-stamped like a fresh
// write, so the entry rejoins the renewal/prune cycle instead of living
// forever. An in-policy future expiry is left untouched, the correction
// spends no AniList request (a re-stamped entry is still a live memo hit), and
// the repair is reported once with its count.
func TestMemoExpiryBeyondHorizonRestamped(t *testing.T) {
	fake := &countingAniList{}
	m := expiryMatcher(fake, 0.5)
	logger, recorder := capture.New()
	m.log = logger
	skewed := memoTestClock.Add(90 * 24 * time.Hour) // far beyond now+memoMaxTTL
	inPolicy := memoTestClock.Add(memoMaxTTL - time.Hour)
	memo := Memo{Entries: map[int]MemoEntry{
		1: {NotFound: true, Expiry: skewed},
		2: {Titles: []string{"Kept"}, Format: "TV", Year: 2020, Expiry: inPolicy},
	}}

	res := m.Match(context.Background(), []seadex.Entry{{AniListID: 1}, {AniListID: 2}},
		&library.Snapshot{}, mapping.NewIndex(nil), memo)

	if fake.calls != 0 {
		t.Errorf("AniList calls = %d, want 0: re-stamping must not trigger a fetch", fake.calls)
	}
	want := memoTestClock.Add(memoMinTTL + (memoMaxTTL-memoMinTTL)/2)
	if got := res.Memo.Entries[1].Expiry; !got.Equal(want) {
		t.Errorf("memo[1].Expiry = %s, want the fresh stamp %s (the out-of-policy value must be corrected)", got, want)
	}
	if got := res.Memo.Entries[2].Expiry; !got.Equal(inPolicy) {
		t.Errorf("memo[2].Expiry = %s, want the untouched in-policy %s", got, inPolicy)
	}
	records := recorder.Records()
	if len(records) != 1 {
		t.Fatalf("log records = %d, want 1 counted WARN for the re-stamped entries", len(records))
	}
	if records[0].Level != slog.LevelWarn ||
		records[0].Message != "anilist memo: expiries beyond the policy horizon re-stamped" {
		t.Errorf("record = level %s message %q, want the WARN re-stamp summary",
			records[0].Level, records[0].Message)
	}
	var restamped int64 = -1
	records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "restamped" {
			restamped = a.Value.Int64()
			return false
		}
		return true
	})
	if restamped != 1 {
		t.Errorf("restamped attribute = %d, want 1 (only the out-of-policy entry is corrected)", restamped)
	}
}

// outageAniList models an AniList outage the memo could ride out: every per-id
// Fetch fails transiently, and the batch prefetch returns an (unscoped) error so
// nothing is memoized from it and every pending id reaches the per-id path.
type outageAniList struct{ fetchCalls int }

func (o *outageAniList) Fetch(_ context.Context, _ int) (anilist.Media, error) {
	o.fetchCalls++
	return anilist.Media{}, errors.New("anilist: dial tcp: connection refused")
}

func (o *outageAniList) FetchMany(_ context.Context, _ []int) (anilist.BatchResult, error) {
	return anilist.BatchResult{Media: map[int]anilist.Media{}, Completed: true},
		errors.New("anilist: dial tcp: connection refused")
}

// TestLookupServesExpiredMemoDuringOutage pins the stale-on-error arm of the
// per-entry lookup: when the upstream that would renew an expired entry is
// unreachable, the expired POSITIVE is served so the entry still matches,
// instead of preferring "unknown" over "stale" and dropping a match whose
// titles the app is holding. The pass still reports the id as incomplete, so
// prior findings are preserved and the AniList degradation streak still
// advances - the stale answer adds a match, it does not clear the degradation.
// An expired NEGATIVE is not stale evidence and is never served that way.
func TestLookupServesExpiredMemoDuringOutage(t *testing.T) {
	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 5, Title: "Clannad", TvdbID: 700, Year: 2007},
	}}
	idx := mapping.NewIndex(nil) // no Fribb record: the entry needs the title fallback
	expired := memoTestClock.Add(-time.Hour)

	t.Run("expired positive is served", func(t *testing.T) {
		fake := &outageAniList{}
		memo := Memo{Entries: map[int]MemoEntry{
			600: {Titles: []string{"Clannad"}, Format: "TV", Year: 2007, Expiry: expired},
		}}

		res := expiryMatcher(fake, 0.5).Match(context.Background(),
			[]seadex.Entry{{AniListID: 600}}, snap, idx, memo)

		if len(res.Matches) != 1 || !res.Matches[0].InLibrary() || res.Matches[0].Source != SourceTitle {
			t.Fatalf("matches = %+v, want one title match from the expired memo entry", res.Matches)
		}
		if !res.Degraded {
			t.Error("Degraded = false, want true: a stale-served match does not clear the outage")
		}
		if _, incomplete := res.IncompleteIDs[600]; !incomplete {
			t.Error("IncompleteIDs is missing 600, want the id reported so prior findings are preserved")
		}
		if got := res.Memo.Entries[600].Expiry; !got.Equal(expired) {
			t.Errorf("memo[600].Expiry = %s, want the entry left unrenewed at %s", got, expired)
		}
	})

	t.Run("expired negative is not served", func(t *testing.T) {
		fake := &outageAniList{}
		memo := Memo{Entries: map[int]MemoEntry{600: {NotFound: true, Expiry: expired}}}

		res := expiryMatcher(fake, 0.5).Match(context.Background(),
			[]seadex.Entry{{AniListID: 600}}, snap, idx, memo)

		if len(res.Matches) != 1 || res.Matches[0].InLibrary() {
			t.Errorf("matches = %+v, want the entry unmatched: an expired not-found is not stale evidence", res.Matches)
		}
	})
}

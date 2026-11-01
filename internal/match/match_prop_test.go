package match

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestMemoExpiryLifecycleProperty pins the memo lifecycle invariants under randomized
// pre-state (absent / live / expired / legacy entries), randomized jitter draws and a
// randomized AniList answer set, across one clean Match pass: no returned entry is immortal
// (zero expiry) or already expired, a pending id is re-stamped inside [now+memoMinTTL,
// now+memoMaxTTL) with the batch answer deciding positive vs negative, a live entry
// survives untouched with zero AniList traffic, and an entry with no expiry behaves exactly
// like an expired one (no migration exists, so the two must not diverge). The model is the
// drawn state labels, so the assertions restate the contract, not the implementation.
func TestMemoExpiryLifecycleProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		now := memoTestClock
		nIDs := rapid.IntRange(1, 8).Draw(rt, "ids")

		const (
			stateAbsent = iota
			stateLive
			stateExpired
			stateUnstamped
		)
		entries := make([]seadex.Entry, 0, nIDs)
		records := make([]mapping.Record, 0, nIDs)
		media := make(map[int]anilist.Media)
		states := make(map[int]int, nIDs)
		pre := make(map[int]MemoEntry, nIDs)
		memo := Memo{Entries: make(map[int]MemoEntry)}
		for id := 1; id <= nIDs; id++ {
			entries = append(entries, seadex.Entry{AniListID: id})
			// A record with no route: the split-mapping shape that needs the
			// AniList title fallback, so every id is a lookup candidate.
			records = append(records, mapping.Record{AniListID: id})
			if rapid.Bool().Draw(rt, "known") {
				media[id] = anilist.Media{Titles: []string{"Movie"}, Format: "MOVIE", Year: 2020}
			}
			st := rapid.IntRange(stateAbsent, stateUnstamped).Draw(rt, "state")
			states[id] = st
			switch st {
			case stateLive:
				ttl := time.Duration(rapid.Int64Range(1, int64(memoMaxTTL)).Draw(rt, "liveTTL"))
				memo.Entries[id] = MemoEntry{Titles: []string{"Cached"}, Format: "MOVIE", Year: 2019, Expiry: now.Add(ttl)}
			case stateExpired:
				age := time.Duration(rapid.Int64Range(0, int64(memoMaxTTL)).Draw(rt, "expiredAge"))
				memo.Entries[id] = MemoEntry{NotFound: true, Expiry: now.Add(-age)}
			case stateUnstamped:
				// Written by a build older than the expiry policy: no expiry
				// field at all. It must behave as expired, not as a special
				// case - that equivalence is what lets the app ship no
				// migration.
				memo.Entries[id] = MemoEntry{Titles: []string{"Unstamped"}, Format: "TV", Year: 2018}
			}
			if ent, ok := memo.Entries[id]; ok {
				pre[id] = ent
			}
		}

		// Background entries: in the memo but consulted by NO entry this pass.
		// A live one must survive untouched; an expired one must be pruned.
		nBg := rapid.IntRange(0, 4).Draw(rt, "bg")
		bgLive := make(map[int]bool, nBg)
		for id := nIDs + 1; id <= nIDs+nBg; id++ {
			if rapid.Bool().Draw(rt, "bgLive") {
				bgLive[id] = true
				memo.Entries[id] = MemoEntry{Titles: []string{"Bg"}, Format: "TV", Expiry: now.Add(time.Hour)}
			} else {
				bgLive[id] = false
				memo.Entries[id] = MemoEntry{NotFound: true, Expiry: now.Add(-time.Hour)}
			}
		}

		fake := &batchCountingAniList{media: media}
		draws := rapid.SliceOfN(rapid.Float64Range(0, 0.999999), 1, 4).Draw(rt, "draws")
		m := expiryMatcher(fake, draws...)

		res := m.Match(t.Context(), entries, &library.Snapshot{}, mapping.NewIndex(mapping.Source{Records: records}), memo)
		// entries IS the whole catalogue in this model, which is the only thing
		// PruneMemo may be handed; the property covers the pruning half of the
		// lifecycle, so it runs the call the reconcile runs.
		PruneMemo(&res, entries)

		if res.Degraded {
			rt.Fatalf("Degraded = true, want false on a clean pass")
		}
		pending := 0
		for id := 1; id <= nIDs; id++ {
			st := states[id]
			ent, ok := res.Memo.Entries[id]
			if !ok {
				rt.Fatalf("memo[%d] missing after a clean pass (state %d)", id, st)
			}
			if ent.Expiry.IsZero() {
				rt.Fatalf("memo[%d].Expiry is zero: an immortal entry survived the pass (state %d)", id, st)
			}
			if !ent.Expiry.After(now) {
				rt.Fatalf("memo[%d].Expiry = %s is not after now: a clean pass must renew or prune what expired", id, ent.Expiry)
			}
			switch st {
			case stateAbsent, stateExpired, stateUnstamped:
				pending++
				lo, hi := now.Add(memoMinTTL), now.Add(memoMaxTTL)
				if ent.Expiry.Before(lo) || !ent.Expiry.Before(hi) {
					rt.Fatalf("memo[%d].Expiry = %s outside the fresh-stamp window [%s, %s)", id, ent.Expiry, lo, hi)
				}
				if _, known := media[id]; known == ent.NotFound {
					rt.Fatalf("memo[%d].NotFound = %v with batch answer known=%v; the completed batch must decide positive vs negative", id, ent.NotFound, known)
				}
			case stateLive:
				if !ent.Expiry.Equal(pre[id].Expiry) || ent.NotFound != pre[id].NotFound {
					rt.Fatalf("memo[%d] = %+v, want the live pre-state entry untouched (%+v)", id, ent, pre[id])
				}
			}
		}
		for id, live := range bgLive {
			ent, ok := res.Memo.Entries[id]
			if live && (!ok || !ent.Expiry.Equal(now.Add(time.Hour))) {
				rt.Fatalf("background live memo[%d] = %+v (present=%v), want kept untouched", id, ent, ok)
			}
			if !live && ok {
				rt.Fatalf("background expired memo[%d] = %+v survived a clean pass, want it pruned", id, ent)
			}
		}
		if fake.fetchCalls != 0 {
			rt.Fatalf("single Fetch calls = %d, want 0 (a completed batch answers every pending id)", fake.fetchCalls)
		}
		wantBatches := 0
		if pending > 0 {
			wantBatches = 1
		}
		if fake.batchCalls != wantBatches {
			rt.Fatalf("batch calls = %d, want %d (only pending ids consult AniList)", fake.batchCalls, wantBatches)
		}
	})
}

// wantRoutes is each route's documented lookup, as the id its hit must carry:
// the first of the record's TMDB movie ids a Radarr item holds, and its TVDB
// id when a Sonarr item holds it. 0 means no hit.
func wantRoutes(rec *mapping.Record, items []library.Item) (tmdb, tvdb int) {
	holds := func(match func(*library.Item) bool) bool {
		return slices.ContainsFunc(items, func(it library.Item) bool { return match(&it) })
	}
	for _, id := range rec.TmdbMovies {
		if holds(func(it *library.Item) bool { return it.Arr == library.ArrRadarr && it.TmdbID == id }) {
			tmdb = id
			break
		}
	}
	if rec.TvdbID > 0 && holds(func(it *library.Item) bool { return it.Arr == library.ArrSonarr && it.TvdbID == rec.TvdbID }) {
		tvdb = rec.TvdbID
	}
	return tmdb, tvdb
}

// TestRouteLookupCatalogueConsistencyProperty pins the Radarr and Sonarr
// lookups against their documented order, and every hit as recognized by the
// Catalogue built from the same records. IDs draw from a small pool so hits,
// wrong-arr collisions and shadowing all occur often.
func TestRouteLookupCatalogueConsistencyProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		nRec := rapid.IntRange(1, 6).Draw(rt, "records")
		records := make([]mapping.Record, 0, nRec)
		for i := range nRec {
			rec := mapping.Record{AniListID: i + 1}
			rec.TmdbMovies = rapid.SliceOfNDistinct(rapid.IntRange(1, 8), 0, 2, rapid.ID[int]).Draw(rt, "tmdb")
			rec.TvdbID = rapid.IntRange(0, 8).Draw(rt, "tvdb")
			records = append(records, rec)
		}
		nItems := rapid.IntRange(1, 6).Draw(rt, "items")
		items := make([]library.Item, 0, nItems)
		for i := range nItems {
			it := library.Item{ArrID: i + 1, Title: fmt.Sprintf("Item %d", i)}
			if rapid.Bool().Draw(rt, "radarr") {
				it.Arr = library.ArrRadarr
				it.TmdbID = rapid.IntRange(0, 8).Draw(rt, "itemTmdb")
				// A stray same-key TVDB id on the film: the Sonarr route must
				// not reach a Radarr item.
				it.TvdbID = rapid.IntRange(0, 8).Draw(rt, "strayTvdb")
			} else {
				it.Arr = library.ArrSonarr
				it.TvdbID = rapid.IntRange(0, 8).Draw(rt, "itemTvdb")
				// A stray same-key movie id on the series item: neither the
				// Radarr route nor the catalogue may recognize a Sonarr item by it.
				it.TmdbID = rapid.IntRange(0, 8).Draw(rt, "strayTmdb")
			}
			items = append(items, it)
		}
		li := NewLibIndex(&library.Snapshot{Items: items})
		cat := NewCatalogue(mapping.NewIndex(mapping.Source{Records: records}), false)
		for i := range records {
			rec := &records[i]
			tmdb, tvdb := wantRoutes(rec, items)
			for _, route := range []struct {
				arr  string
				got  *library.Item
				want int
				id   func(*library.Item) int
			}{
				{library.ArrRadarr, li.Radarr(rec), tmdb, func(it *library.Item) int { return it.TmdbID }},
				{library.ArrSonarr, li.Sonarr(rec), tvdb, func(it *library.Item) int { return it.TvdbID }},
			} {
				if route.got == nil {
					if route.want > 0 {
						rt.Fatalf("%s route of %+v = nil, want the item with id %d", route.arr, *rec, route.want)
					}
					continue
				}
				if route.want == 0 || route.got.Arr != route.arr || route.id(route.got) != route.want {
					rt.Fatalf("%s route of %+v = %s item %q (id %d), want id %d", route.arr, *rec, route.got.Arr, route.got.Title, route.id(route.got), route.want)
				}
				if !cat.Has(route.got) {
					rt.Fatalf("forward/reverse drift: the %s route of record %d resolved %q but Catalogue.Has rejects it", route.arr, rec.AniListID, route.got.Title)
				}
			}
		}
	})
}

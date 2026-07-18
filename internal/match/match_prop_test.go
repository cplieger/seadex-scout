package match

import (
	"context"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestMemoExpiryLifecycleProperty pins the memo lifecycle invariants under
// randomized pre-state (absent / live / expired / legacy entries), randomized
// jitter draws, and a randomized AniList answer set, across one clean Match
// pass: no returned entry is immortal (zero expiry) or already expired, a
// pending id (absent or expired) is re-stamped inside [now+memoTTLMin,
// now+memoTTLMax) with the batch answer deciding positive vs negative, a live
// entry survives untouched with zero AniList traffic for it, and a legacy
// entry is migrated into [now+memoMigrationMin, now+memoTTLMax) keeping its
// payload (migration never re-fetches). The model is the drawn state labels,
// so the assertions restate the documented contract, not the implementation.
func TestMemoExpiryLifecycleProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		now := memoTestClock
		nIDs := rapid.IntRange(1, 8).Draw(rt, "ids")

		const (
			stateAbsent = iota
			stateLive
			stateExpired
			stateLegacy
		)
		entries := make([]seadex.Entry, 0, nIDs)
		records := make([]mapping.Record, 0, nIDs)
		media := make(map[int]anilist.Media)
		states := make(map[int]int, nIDs)
		pre := make(map[int]MemoEntry, nIDs)
		memo := Memo{Entries: make(map[int]MemoEntry)}
		for id := 1; id <= nIDs; id++ {
			entries = append(entries, seadex.Entry{AniListID: id})
			// An id-less MOVIE record: the split-mapping shape that needs the
			// AniList title fallback, so every id is a lookup candidate.
			records = append(records, mapping.Record{AniListID: id, Type: "MOVIE"})
			if rapid.Bool().Draw(rt, "known") {
				media[id] = anilist.Media{Titles: []string{"Movie"}, Format: "MOVIE", Year: 2020}
			}
			st := rapid.IntRange(stateAbsent, stateLegacy).Draw(rt, "state")
			states[id] = st
			switch st {
			case stateLive:
				ttl := time.Duration(rapid.Int64Range(1, int64(memoTTLMax)).Draw(rt, "liveTTL"))
				memo.Entries[id] = MemoEntry{Titles: []string{"Cached"}, Format: "MOVIE", Year: 2019, Expiry: now.Add(ttl)}
			case stateExpired:
				age := time.Duration(rapid.Int64Range(0, int64(memoTTLMax)).Draw(rt, "expiredAge"))
				memo.Entries[id] = MemoEntry{NotFound: true, Expiry: now.Add(-age)}
			case stateLegacy:
				memo.Entries[id] = MemoEntry{Titles: []string{"Legacy"}, Format: "TV", Year: 2018}
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

		res := m.Match(context.Background(), entries, &library.Snapshot{}, mapping.NewIndex(records), memo)

		if res.Degraded {
			t.Fatalf("Degraded = true, want false on a clean pass")
		}
		pending := 0
		for id := 1; id <= nIDs; id++ {
			st := states[id]
			ent, ok := res.Memo.Entries[id]
			if !ok {
				t.Fatalf("memo[%d] missing after a clean pass (state %d)", id, st)
			}
			if ent.Expiry.IsZero() {
				t.Fatalf("memo[%d].Expiry is zero: an immortal entry survived the pass (state %d)", id, st)
			}
			if !ent.Expiry.After(now) {
				t.Fatalf("memo[%d].Expiry = %s is not after now: a clean pass must renew or prune what expired", id, ent.Expiry)
			}
			switch st {
			case stateAbsent, stateExpired:
				pending++
				lo, hi := now.Add(memoTTLMin), now.Add(memoTTLMax)
				if ent.Expiry.Before(lo) || !ent.Expiry.Before(hi) {
					t.Fatalf("memo[%d].Expiry = %s outside the fresh-stamp window [%s, %s)", id, ent.Expiry, lo, hi)
				}
				if _, known := media[id]; known == ent.NotFound {
					t.Fatalf("memo[%d].NotFound = %v with batch answer known=%v; the completed batch must decide positive vs negative", id, ent.NotFound, known)
				}
			case stateLive:
				if !ent.Expiry.Equal(pre[id].Expiry) || ent.NotFound != pre[id].NotFound {
					t.Fatalf("memo[%d] = %+v, want the live pre-state entry untouched (%+v)", id, ent, pre[id])
				}
			case stateLegacy:
				lo, hi := now.Add(memoMigrationMin), now.Add(memoTTLMax)
				if ent.Expiry.Before(lo) || !ent.Expiry.Before(hi) {
					t.Fatalf("memo[%d].Expiry = %s outside the migration window [%s, %s)", id, ent.Expiry, lo, hi)
				}
				if len(ent.Titles) != 1 || ent.Titles[0] != "Legacy" {
					t.Fatalf("memo[%d] = %+v, want the legacy payload kept (migration never re-fetches)", id, ent)
				}
			}
		}
		for id, live := range bgLive {
			ent, ok := res.Memo.Entries[id]
			if live && (!ok || !ent.Expiry.Equal(now.Add(time.Hour))) {
				t.Fatalf("background live memo[%d] = %+v (present=%v), want kept untouched", id, ent, ok)
			}
			if !live && ok {
				t.Fatalf("background expired memo[%d] = %+v survived a clean pass, want it pruned", id, ent)
			}
		}
		if fake.fetchCalls != 0 {
			t.Fatalf("single Fetch calls = %d, want 0 (a completed batch answers every pending id)", fake.fetchCalls)
		}
		wantBatches := 0
		if pending > 0 {
			wantBatches = 1
		}
		if fake.batchCalls != wantBatches {
			t.Fatalf("batch calls = %d, want %d (only pending ids consult AniList)", fake.batchCalls, wantBatches)
		}
	})
}

package state

import (
	"context"
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/match"
	"pgregory.net/rapid"
)

// TestStoreSaveLoadRoundTripProperty pins the persistence round trip for
// arbitrary generated states: every persisted field (the AniList memo with its
// jittered expiry stamps and arbitrary unicode titles, plus the per-arr shrink
// streak map and the two scalar escalation streaks) survives Save then Load
// exactly, and Save stamps
// SchemaVersion. This is the
// generative net over the json-tag/projection drift the deterministic
// round-trip tests pin with single sample values.
func TestStoreSaveLoadRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		genTime := rapid.Custom(func(rt *rapid.T) time.Time {
			sec := rapid.Int64Range(0, 4102444800).Draw(rt, "sec")
			nsec := rapid.Int64Range(0, 999999999).Draw(rt, "nsec")
			return time.Unix(sec, nsec).UTC()
		})
		memo := rapid.MapOfN(
			rapid.IntRange(1, 1<<30),
			rapid.Custom(func(rt *rapid.T) match.MemoEntry {
				return match.MemoEntry{
					Titles:   rapid.SliceOfN(rapid.String(), 1, 3).Draw(rt, "titles"),
					Format:   rapid.String().Draw(rt, "format"),
					Year:     rapid.IntRange(0, 2100).Draw(rt, "year"),
					Expiry:   genTime.Draw(rt, "expiry"),
					NotFound: rapid.Bool().Draw(rt, "not_found"),
				}
			}),
			0, 8,
		).Draw(rt, "memo")
		want := &State{
			Memo: match.Memo{Entries: memo},
			ShrunkWalksByArr: rapid.MapOfN(
				rapid.SampledFrom([]string{library.ArrSonarr, library.ArrRadarr}),
				rapid.IntRange(0, 1000), 0, 2,
			).Draw(rt, "shrunk_walks_by_arr"),
			SeadexFailures:  rapid.IntRange(0, 1000).Draw(rt, "seadex_failures"),
			AniListDegraded: rapid.IntRange(0, 1000).Draw(rt, "anilist_degraded"),
		}

		store := NewStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
		if err := store.Save(context.Background(), want); err != nil {
			rt.Fatalf("Save returned error: %v", err)
		}
		got, err := store.Load(context.Background())
		if err != nil {
			rt.Fatalf("Load after Save returned error: %v", err)
		}
		if got.Version != SchemaVersion {
			rt.Errorf("Version = %d, want stamped %d", got.Version, SchemaVersion)
		}
		if !maps.Equal(got.ShrunkWalksByArr, want.ShrunkWalksByArr) {
			rt.Errorf("ShrunkWalksByArr = %v, want %v", got.ShrunkWalksByArr, want.ShrunkWalksByArr)
		}
		if got.SeadexFailures != want.SeadexFailures || got.AniListDegraded != want.AniListDegraded {
			rt.Errorf("streaks = %d/%d, want %d/%d", got.SeadexFailures, got.AniListDegraded, want.SeadexFailures, want.AniListDegraded)
		}
		if len(got.Memo.Entries) != len(want.Memo.Entries) {
			rt.Fatalf("memo len = %d, want %d", len(got.Memo.Entries), len(want.Memo.Entries))
		}
		for id, w := range want.Memo.Entries {
			g, ok := got.Memo.Entries[id]
			if !ok {
				rt.Fatalf("memo id %d lost in round trip", id)
			}
			if !g.Expiry.Equal(w.Expiry) || g.Format != w.Format || g.Year != w.Year || g.NotFound != w.NotFound {
				rt.Errorf("memo[%d] = %+v, want %+v", id, g, w)
			}
			if len(g.Titles) != len(w.Titles) {
				rt.Fatalf("memo[%d] titles len = %d, want %d", id, len(g.Titles), len(w.Titles))
			}
			for i := range w.Titles {
				if g.Titles[i] != w.Titles[i] {
					rt.Errorf("memo[%d] titles[%d] = %q, want %q", id, i, g.Titles[i], w.Titles[i])
				}
			}
		}
	})
}

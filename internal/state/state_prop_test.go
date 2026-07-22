package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/notify"
	"pgregory.net/rapid"
)

// TestStoreSaveLoadRoundTripProperty pins the persistence round trip for
// arbitrary generated states: every persisted field (the findings dedupe map
// keyed by arbitrary unicode dedupe keys, the AniList memo with its jittered
// expiry stamps, all three escalation streaks, and both baseline flags) survives
// Save then Load exactly, and Save stamps SchemaVersion. This is the
// generative net over the json-tag/projection drift the deterministic
// round-trip tests pin with single sample values.
func TestStoreSaveLoadRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		genTime := rapid.Custom(func(rt *rapid.T) time.Time {
			sec := rapid.Int64Range(0, 4102444800).Draw(rt, "sec")
			nsec := rapid.Int64Range(0, 999999999).Draw(rt, "nsec")
			return time.Unix(sec, nsec).UTC()
		})
		findings := rapid.MapOfN(
			rapid.String(),
			rapid.Custom(func(rt *rapid.T) notify.Alerted {
				return notify.Alerted{
					AlertedAt: genTime.Draw(rt, "alerted_at"),
					Finding: notify.StoredFinding{
						Title:     rapid.String().Draw(rt, "title"),
						Arr:       rapid.SampledFrom([]string{"sonarr", "radarr"}).Draw(rt, "arr"),
						AniListID: rapid.IntRange(0, 1<<30).Draw(rt, "al_id"),
						Season:    rapid.IntRange(0, 99).Draw(rt, "season"),
					},
				}
			}),
			0, 8,
		).Draw(rt, "findings")
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
			Findings:           findings,
			Memo:               match.Memo{Entries: memo},
			ShrunkWalks:        rapid.IntRange(0, 1000).Draw(rt, "shrunk"),
			SeadexFailures:     rapid.IntRange(0, 1000).Draw(rt, "seadex_failures"),
			AniListDegraded:    rapid.IntRange(0, 1000).Draw(rt, "anilist_degraded"),
			Baselined:          rapid.Bool().Draw(rt, "baselined"),
			BaselineIncomplete: rapid.Bool().Draw(rt, "baseline_incomplete"),
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
		if got.ShrunkWalks != want.ShrunkWalks || got.SeadexFailures != want.SeadexFailures || got.AniListDegraded != want.AniListDegraded {
			rt.Errorf("streaks = %d/%d/%d, want %d/%d/%d", got.ShrunkWalks, got.SeadexFailures, got.AniListDegraded, want.ShrunkWalks, want.SeadexFailures, want.AniListDegraded)
		}
		if got.Baselined != want.Baselined || got.BaselineIncomplete != want.BaselineIncomplete {
			rt.Errorf("flags = %v/%v, want %v/%v", got.Baselined, got.BaselineIncomplete, want.Baselined, want.BaselineIncomplete)
		}
		if len(got.Findings) != len(want.Findings) {
			rt.Fatalf("findings len = %d, want %d", len(got.Findings), len(want.Findings))
		}
		for k, w := range want.Findings {
			g, ok := got.Findings[k]
			if !ok {
				rt.Fatalf("findings key %q lost in round trip", k)
			}
			if !g.AlertedAt.Equal(w.AlertedAt) || g.Finding != w.Finding {
				rt.Errorf("findings[%q] = %+v, want %+v", k, g, w)
			}
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

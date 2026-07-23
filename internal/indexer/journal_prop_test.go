package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestRenderJournalItemOrderInvariantFoldProperty pins the documented
// order-independent fold contract of renderJournalItem across arbitrary
// occurrence counts and orders: for a torrent attached to N SeaDex entries,
// the rendered item is identical under any permutation of the refs (category
// union compared as a set), the marker is best-wins (dvfBest iff ANY
// occurrence is best), the synthesis source is the lowest AniList id, and the
// category union contains Movies iff any occurrence is a movie and Anime iff
// any is a series.
func TestRenderJournalItemOrderInvariantFoldProperty(t *testing.T) {
	w := newTestWriter(filepath.Join(t.TempDir(), "feed.json"), "", false)
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(rt, "n")
		ids := rapid.SliceOfNDistinct(rapid.IntRange(1, 1_000_000), n, n, rapid.ID[int]).Draw(rt, "ids")
		movie := make(map[int]bool, n)
		best := make([]bool, n)
		refs := make([]curatedRef, n)
		for i, id := range ids {
			best[i] = rapid.Bool().Draw(rt, "best"+strconv.Itoa(i))
			movie[id] = rapid.Bool().Draw(rt, "movie"+strconv.Itoa(i))
			e := &seadex.Entry{AniListID: id, Torrents: []seadex.Torrent{{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/77", IsBest: best[i],
				Files: []seadex.File{{Length: 9, Name: "Show - S01E01 (1080p) [G].mkv"}},
			}}}
			refs[i] = curatedRef{entry: e, torrent: &e.Torrents[0]}
		}
		infoFor := func(alID int) EntryInfo { return EntryInfo{IsMovie: movie[alID]} }

		it, ok, noPasskey := w.renderJournalItem("nyaa:77", refs, infoFor)
		if !ok || noPasskey {
			rt.Fatalf("renderJournalItem = (ok=%v, noPasskey=%v), want (true, false)", ok, noPasskey)
		}

		wantDVF := dvfAlt
		if slices.Contains(best, true) {
			wantDVF = dvfBest
		}
		if it.DownloadVolumeFactor != wantDVF {
			rt.Errorf("marker = %q, want %q (best-wins across all occurrences)", it.DownloadVolumeFactor, wantDVF)
		}
		if want := slices.Min(ids); it.AniListID != want {
			rt.Errorf("AniListID = %d, want the lowest id %d (deterministic synthesis source)", it.AniListID, want)
		}
		anyMovie, anySeries := false, false
		for _, id := range ids {
			if movie[id] {
				anyMovie = true
			} else {
				anySeries = true
			}
		}
		if got := slices.Contains(it.Categories, catMovies); got != anyMovie {
			rt.Errorf("Categories %v contains Movies = %v, want %v", it.Categories, got, anyMovie)
		}
		if got := slices.Contains(it.Categories, catAnime); got != anySeries {
			rt.Errorf("Categories %v contains Anime = %v, want %v", it.Categories, got, anySeries)
		}

		perm := rapid.Permutation(refs).Draw(rt, "perm")
		it2, ok2, _ := w.renderJournalItem("nyaa:77", perm, infoFor)
		if !ok2 {
			rt.Fatalf("permuted renderJournalItem not rendered")
		}
		catsOf := func(c []int) []int { c = slices.Clone(c); slices.Sort(c); return c }
		if it2.Title != it.Title || it2.GUID != it.GUID || it2.InfoURL != it.InfoURL ||
			it2.DownloadURL != it.DownloadURL || it2.Size != it.Size ||
			it2.AniListID != it.AniListID || it2.DownloadVolumeFactor != it.DownloadVolumeFactor ||
			!slices.Equal(catsOf(it2.Categories), catsOf(it.Categories)) {
			rt.Errorf("permuted render differs:\n got %+v\nwant %+v", it2, it)
		}
	})
}

// TestRebuildNeverRebroadcastsProperty is a model-based property over the
// journal's novelty ledger across arbitrary rebuild sequences: for a fixed
// pool of distinct Nyaa torrents, each rebuild curating a random subset, (1)
// a torrent curated in the FIRST rebuild (the fresh-install baseline) never
// journals in any later rebuild, (2) a journaled key's FirstSeen never
// changes across rebuilds, and (3) no feed ever holds duplicate keys. The
// model is a plain set of first-round keys plus a key->FirstSeen map - never
// a reimplementation of the ledger - so a broken seen-fold (re-broadcast), a
// broken baseline, or a duplicate append all fail the property.
func TestRebuildNeverRebroadcastsProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		path := filepath.Join(t.TempDir(), "feed.json")
		w := newTestWriter(path, "", false)
		now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
		w.now = func() time.Time { return now }

		const poolSize = 5
		rounds := rapid.IntRange(2, 6).Draw(rt, "rounds")
		baselined := map[string]bool{} // keys curated in round 0 (fresh-install baseline)
		firstSeen := map[string]time.Time{}
		for r := range rounds {
			var cat []seadex.Entry
			for i := range poolSize {
				if rapid.Bool().Draw(rt, "in"+strconv.Itoa(r)+"_"+strconv.Itoa(i)) {
					cat = append(cat, nyaaEntry(100+i, 1000+i, true,
						"Show "+strconv.Itoa(i)+" - S01E01 (1080p) [G].mkv"))
					if r == 0 {
						baselined["nyaa:"+strconv.Itoa(1000+i)] = true
					}
				}
			}
			if err := w.Rebuild(context.Background(), cat, nil); err != nil {
				rt.Fatalf("Rebuild round %d: %v", r, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				rt.Fatalf("read snapshot round %d: %v", r, err)
			}
			var snap snapshot
			if err := json.Unmarshal(data, &snap); err != nil {
				rt.Fatalf("unmarshal snapshot round %d: %v", r, err)
			}
			inFeed := map[string]bool{}
			for i := range snap.NyaaFeed {
				it := snap.NyaaFeed[i]
				if inFeed[it.Key] {
					rt.Errorf("round %d: duplicate journal key %q in one feed", r, it.Key)
				}
				inFeed[it.Key] = true
				if baselined[it.Key] {
					rt.Errorf("round %d: baselined key %q re-entered the journal (baseline must permanently veto novelty)", r, it.Key)
				}
				if prev, ok := firstSeen[it.Key]; ok {
					if !it.FirstSeen.Equal(prev) {
						rt.Errorf("round %d: key %q FirstSeen changed %v -> %v (a journaled item must keep its first-seen stamp)", r, it.Key, prev, it.FirstSeen)
					}
				} else {
					firstSeen[it.Key] = it.FirstSeen
				}
			}
			now = now.Add(time.Hour)
		}
	})
}

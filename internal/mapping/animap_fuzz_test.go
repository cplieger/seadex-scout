package mapping

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/seadex-scout/internal/mediatype"
)

// FuzzProject exercises the refresh's decode-and-project path against
// arbitrary bytes. For any document mapread accepts, the projection keeps
// one canonical record per entry, by AniList id, a season only beside a TVDB
// id, and facts only under positive keys, each list ascending and each range
// within its entry's episodes.
// fuzzEpisodes keeps each input's expansion small, so the fuzzer explores
// shapes rather than one expensive document.
const fuzzEpisodes = 1 << 12

func FuzzProject(f *testing.F) {
	f.Add(entriesBody(f, seriesEntries(2)...))
	f.Add(entriesBody(f, entry{AniList: 3, Type: "MOVIE", TVDB: 9, Lands: targets(at(0, 4), at(0, 5)), Movies: []int{7}, IMDb: []string{"tt0000007"}}))
	f.Add(entriesBody(f, entry{AniList: 4, Type: "TV", TVDB: 9}, entry{AniList: 5, Type: "OVA", TVDB: 9, Lands: targets(dflt(2, 1))}))
	f.Add(entriesBody(f, entry{AniList: 6, Type: "MOVIE", Movies: []int{8}}, entry{AniList: 7, Type: "TV", TVDB: 9}))
	f.Add(encode(f, document([]mapread.Anime{
		{AniDBID: 1, TVDBID: 5, Last: mapread.Last{Regular: 4, Special: 1}, Episodes: []mapread.Run{
			regular(1, 2, targets(at(1, 1))), regular(3, 3, targets(at(0, 9))), regular(4, 4, targets(at(1, 3))),
			{Kind: mapread.Special, Start: 1, End: 1, TVDB: targets(at(0, 1))},
		}},
	}, []mapread.Entry{
		{AniListID: 1, Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Regular}}},
		{AniListID: 2, Parts: []mapread.Part{{AniDBID: 1, Kind: mapread.Special, Start: 1, End: 1}, {AniDBID: 1, Kind: mapread.Regular, Combined: true}}},
	})))
	f.Add([]byte(`{"version":1,"records":[]}`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := mapread.Decode(bytes.NewReader(data), mapread.Limits{MaxDocumentBytes: 1 << 20, MaxEpisodes: fuzzEpisodes})
		if err != nil {
			return
		}
		entries := len(doc.Entries)
		p, err := project(doc, fuzzEpisodes)
		if err != nil {
			if !errors.Is(err, mapread.ErrTooLarge) || p.records != nil {
				t.Fatalf("project error %v with %+v, want only mapread.ErrTooLarge and nothing projected", err, p)
			}
			return
		}
		if len(p.records) != entries {
			t.Errorf("projection holds %d records of %d entries, want one per entry", len(p.records), entries)
		}
		for i, r := range p.records {
			if r.AniListID <= 0 || i > 0 && r.AniListID <= p.records[i-1].AniListID {
				t.Errorf("record %d has AniList id %d, want positive ids ascending once", i, r.AniListID)
			}
			if r.Type != mediatype.Normalize(r.Type) || slices.ContainsFunc(r.TmdbMovies, func(v int) bool { return v <= 0 }) {
				t.Errorf("record not canonical: %+v", r)
			}
			if r.SeasonKind != SeasonAbsent && (r.SeasonKind != SeasonPresent || r.TvdbID <= 0) || r.SeasonKind == SeasonAbsent && r.SeasonTvdb != 0 {
				t.Errorf("record %+v: want a present season only beside a TVDB id, and no season when absent", r)
			}
		}
		for _, facts := range []map[int]Mapping{p.mappings, p.aniListMappings} {
			for k, m := range facts {
				checkFacts(t, k, &m)
			}
		}
	})
}

func checkFacts(t *testing.T, key int, m *Mapping) {
	t.Helper()
	if key <= 0 || m.empty() {
		t.Errorf("facts %+v under key %d, want non-empty facts under a positive key", m, key)
	}
	ascending := func(xs []int) bool {
		return slices.IsSorted(xs) && len(slices.Compact(slices.Clone(xs))) == len(xs) && (len(xs) == 0 || xs[0] >= 1)
	}
	if !ascending(m.Specials) || !ascending(m.Episodes) {
		t.Errorf("facts %+v: want ascending, distinct, positive episode lists", m)
	}
	if m.Episodes != nil && (m.EpisodesTvdb <= 0 || m.EpisodesSeason < 1) || m.Specials != nil && m.SpecialsTvdb <= 0 {
		t.Errorf("facts %+v: want each episode list beside its series and season", m)
	}
	for _, r := range m.Seasons {
		if r.Season < 1 || r.First < 1 || r.Last < r.First {
			t.Errorf("facts %+v: range %+v is not a positive season's First..Last", m, r)
		}
	}
}

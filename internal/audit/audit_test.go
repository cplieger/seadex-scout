package audit

import (
	"reflect"
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

func TestScope(t *testing.T) {
	tests := []struct {
		name       string
		item       library.Item
		rec        mapping.Record
		wantGroups []string
		wantFile   bool
		wantApprox bool
	}{
		{
			name:       "movie scopes to the movie group",
			item:       library.Item{Arr: library.ArrRadarr, Groups: []string{"arid"}, HasFile: true},
			rec:        mapping.Record{Type: "MOVIE"},
			wantGroups: []string{"arid"}, wantFile: true,
		},
		{
			name:       "series with a positive season scopes to that season (exact)",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{2: {"sam"}}},
			rec:        mapping.Record{Type: "TV", SeasonTvdb: 2},
			wantGroups: []string{"sam"}, wantFile: true,
		},
		{
			name:       "series season mapped but not on disk has no file",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"sam"}}},
			rec:        mapping.Record{Type: "TV", SeasonTvdb: 3},
			wantGroups: nil, wantFile: false,
		},
		{
			name:       "special with a single-group season 0 is exact",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"legion"}}},
			rec:        mapping.Record{Type: "OVA"},
			wantGroups: []string{"legion"}, wantFile: true, wantApprox: false,
		},
		{
			name:       "special with a multi-group season 0 is approximate",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"cait-sidhe", "sallysubs"}}},
			rec:        mapping.Record{Type: "SPECIAL"},
			wantGroups: []string{"cait-sidhe", "sallysubs"}, wantFile: true, wantApprox: true,
		},
		{
			name:       "special with no season-0 files has no file",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"x"}}},
			rec:        mapping.Record{Type: "OVA"},
			wantGroups: nil, wantFile: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &match.Match{Item: &tt.item, Record: tt.rec}
			groups, hasFile, approx := scope(m)
			if !reflect.DeepEqual(groups, tt.wantGroups) {
				t.Errorf("groups = %v, want %v", groups, tt.wantGroups)
			}
			if hasFile != tt.wantFile {
				t.Errorf("hasFile = %v, want %v", hasFile, tt.wantFile)
			}
			if approx != tt.wantApprox {
				t.Errorf("approx = %v, want %v", approx, tt.wantApprox)
			}
		})
	}
}

func TestVerdict(t *testing.T) {
	tests := []struct {
		name    string
		hasFile bool
		current []string
		best    []string
		alt     []string
		want    Verdict
	}{
		{"no file is no_file", false, nil, []string{"a"}, nil, VerdictNoFile},
		{"file but no identifiable group is unverified", true, nil, []string{"a"}, nil, VerdictUnverified},
		{"current group is best", true, []string{"sam"}, []string{"sam"}, nil, VerdictBest},
		{"current group is an alt", true, []string{"kh"}, []string{"sam"}, []string{"kh"}, VerdictAlt},
		{"current group is unlisted", true, []string{"zzz"}, []string{"sam"}, []string{"kh"}, VerdictUnlisted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verdict(tt.hasFile, tt.current, tt.best, tt.alt); got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuditNotOnSeaDex(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})

	snap := &library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: "Covered", TvdbID: 100, SeasonGroups: map[int][]string{1: {"x"}}, Groups: []string{"x"}, HasFile: true},
		{Arr: library.ArrSonarr, ArrID: 2, Title: "UncoveredCatalogued", TvdbID: 200, Groups: []string{"y"}, HasFile: true},
		{Arr: library.ArrSonarr, ArrID: 3, Title: "UncoveredUncatalogued", TvdbID: 300, Groups: []string{"z"}, HasFile: true},
		{Arr: library.ArrRadarr, ArrID: 4, Title: "UncoveredMovie", TmdbID: 400, HasFile: true},
	}}
	idx := mapping.NewIndex([]mapping.Record{
		{AniListID: 1, Type: "TV", TvdbID: 100},
		{AniListID: 2, Type: "TV", TvdbID: 200},
		{AniListID: 4, Type: "MOVIE", TmdbMovies: []int{400}},
	})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 1},
		Record: mapping.Record{Type: "TV", TvdbID: 100, SeasonTvdb: 1},
	}}

	rep := a.Audit(matches, snap, idx)

	got := map[string]bool{}
	for i := range rep.Rows {
		if rep.Rows[i].Verdict == VerdictNotOnSeaDex {
			got[rep.Rows[i].Title] = true
		}
	}
	if !got["UncoveredCatalogued"] {
		t.Error("expected the uncovered catalogued series in not_on_seadex")
	}
	if !got["UncoveredMovie"] {
		t.Error("expected the uncovered catalogued movie in not_on_seadex")
	}
	if got["UncoveredUncatalogued"] {
		t.Error("an uncovered item absent from Fribb must not be listed")
	}
	if got["Covered"] {
		t.Error("a covered item must not be listed as not_on_seadex")
	}
	if n := rep.Totals[string(VerdictNotOnSeaDex)]; n != 2 {
		t.Errorf("not_on_seadex total = %d, want 2", n)
	}
}

// TestAuditNoGroupMatchesBest proves the NoGroup fallback end to end: a
// group-less on-disk release compares equal to a group-less SeaDex best (both
// resolve to NOGRP), yielding have_best rather than an unresolved row.
func TestAuditNoGroupMatchesBest(t *testing.T) {
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
	snap := &library.Snapshot{Items: []library.Item{{
		Arr: library.ArrSonarr, ArrID: 9, Title: "Groupless", TvdbID: 900,
		SeasonGroups: map[int][]string{1: {"nogrp"}}, Groups: []string{"nogrp"}, HasFile: true,
	}}}
	idx := mapping.NewIndex([]mapping.Record{{AniListID: 9, Type: "TV", TvdbID: 900}})
	matches := []match.Match{{
		Item:   &snap.Items[0],
		Arr:    library.ArrSonarr,
		Source: match.SourceID,
		Entry:  seadex.Entry{AniListID: 9, Torrents: []seadex.Torrent{{Tracker: "Nyaa", IsBest: true}}},
		Record: mapping.Record{Type: "TV", TvdbID: 900, SeasonTvdb: 1},
	}}

	rep := a.Audit(matches, snap, idx)

	var got Verdict
	for i := range rep.Rows {
		if rep.Rows[i].AniListID == 9 {
			got = rep.Rows[i].Verdict
		}
	}
	if got != VerdictBest {
		t.Errorf("group-less item vs group-less SeaDex best = %q, want %q", got, VerdictBest)
	}
}

// TestWholeSeriesVerdict covers the conservative per-season aggregation for an
// absolute-numbered / whole-series entry: have_best only when every real season
// carries a best group, downgrading otherwise, with season 0 excluded.
func TestWholeSeriesVerdict(t *testing.T) {
	best := []string{"a&c"}
	alt := []string{"kh"}
	tests := []struct {
		name    string
		seasons map[int][]string
		want    Verdict
		approx  bool
	}{
		{"all seasons best", map[int][]string{1: {"a&c"}, 2: {"a&c"}}, VerdictBest, true},
		{"best plus unlisted downgrades to unlisted", map[int][]string{1: {"a&c"}, 2: {"kitsune"}}, VerdictUnlisted, true},
		{"best plus alt downgrades to alt", map[int][]string{1: {"a&c"}, 2: {"kh"}}, VerdictAlt, true},
		{"season 0 is excluded", map[int][]string{0: {"kitsune"}, 1: {"a&c"}}, VerdictBest, false},
		{"single season is not approx", map[int][]string{1: {"a&c"}}, VerdictBest, false},
		{"only season 0 on disk is no_file", map[int][]string{0: {"a&c"}}, VerdictNoFile, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: tt.seasons, HasFile: true}
			got, _, approx := wholeSeriesVerdict(item, best, alt)
			if got != tt.want {
				t.Errorf("verdict = %q, want %q", got, tt.want)
			}
			if approx != tt.approx {
				t.Errorf("approx = %v, want %v", approx, tt.approx)
			}
		})
	}
}

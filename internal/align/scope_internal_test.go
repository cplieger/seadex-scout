package align

import (
	"reflect"
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

// TestScope pins the four-branch scope dispatch Decide consumes, and is the SINGLE
// owner of every scoping branch. It lives in an internal test file because
// scope/scopeResult are package-private: the only production way to obtain a scope
// is through the decision that consumes it, which covers the dispatch indirectly.
func TestScope(t *testing.T) {
	tests := []struct {
		name       string
		wantGroups []string
		rec        mapping.Record
		item       library.Item
		wantKind   ScopeKind
		wantFile   bool
		wantApprox bool
	}{
		{
			name:       "movie scopes to the movie group",
			item:       library.Item{Arr: library.ArrRadarr, Groups: []string{"arid"}, HasFile: true},
			rec:        mapping.Record{Type: "MOVIE"},
			wantGroups: []string{"arid"}, wantKind: ScopeMovie, wantFile: true,
		},
		{
			name:       "radarr movie with a positive Fribb season still scopes to the movie",
			item:       library.Item{Arr: library.ArrRadarr, Groups: []string{"arid"}, HasFile: true, SeasonGroups: map[int][]string{2: {"seasongrp"}}},
			rec:        mapping.Record{Type: "MOVIE", SeasonTvdb: 2},
			wantGroups: []string{"arid"}, wantKind: ScopeMovie, wantFile: true,
		},
		{
			name:       "series with a positive season scopes to that season (exact)",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{2: {"sam"}}},
			rec:        mapping.Record{Type: "TV", SeasonTvdb: 2},
			wantGroups: []string{"sam"}, wantKind: ScopeSeason, wantFile: true,
		},
		{
			name:       "series season mapped but not on disk has no file",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"sam"}}},
			rec:        mapping.Record{Type: "TV", SeasonTvdb: 3},
			wantGroups: nil, wantKind: ScopeSeason, wantFile: false,
		},
		{
			// A single-group bucket carries no more attribution evidence than a
			// multi-group one, so Approx marks "never attributed" rather than
			// "spans several groups".
			name:       "offered with a single-group season 0 is still approximate",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"legion"}}},
			rec:        mapping.Record{Type: "OVA"},
			wantGroups: []string{"legion"}, wantKind: ScopeOffered, wantFile: true, wantApprox: true,
		},
		{
			name:       "offered with a multi-group season 0 is approximate",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"cait-sidhe", "sallysubs"}}},
			rec:        mapping.Record{Type: "SPECIAL"},
			wantGroups: []string{"cait-sidhe", "sallysubs"}, wantKind: ScopeOffered, wantFile: true, wantApprox: true,
		},
		{
			name:       "offered with no season-0 files has no file and no approximation",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"x"}}},
			rec:        mapping.Record{Type: "OVA"},
			wantGroups: nil, wantKind: ScopeOffered, wantFile: false,
		},
		{
			name:       "seasonless non-special series is classified whole-series, not offered",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"legion"}, 1: {"sam"}}},
			rec:        mapping.Record{Type: "TV"},
			wantGroups: nil, wantKind: ScopeWholeSeries, wantFile: false,
		},
		{
			// Heya Camp's shape: Fribb types it TV and maps it to season 0, so the
			// type label would send it to a whole-series comparison against every
			// real season while the season presence says offered.
			name:       "a TV-typed mapped zero is offered, and reads the bucket",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"koala"}, 1: {"sam"}}},
			rec:        mapping.Record{Type: "TV", SeasonKind: mapping.SeasonPresent},
			wantGroups: []string{"koala"}, wantKind: ScopeOffered, wantFile: true, wantApprox: true,
		},
		{
			// LoGH's shape: an OVA-typed absolute-numbered run, whose comparison is
			// against the real seasons it actually spans.
			name:       "an OVA-typed absent season is whole-series",
			item:       library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{0: {"legion"}, 1: {"koala"}}},
			rec:        mapping.Record{Type: "OVA", SeasonKind: mapping.SeasonAbsent},
			wantGroups: nil, wantKind: ScopeWholeSeries, wantFile: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scope(&tt.item, &tt.rec)
			if !reflect.DeepEqual(got.Groups, tt.wantGroups) {
				t.Errorf("Groups = %v, want %v", got.Groups, tt.wantGroups)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tt.wantKind)
			}
			if got.HasFile != tt.wantFile {
				t.Errorf("HasFile = %v, want %v", got.HasFile, tt.wantFile)
			}
			if got.Approx != tt.wantApprox {
				t.Errorf("Approx = %v, want %v", got.Approx, tt.wantApprox)
			}
		})
	}
}

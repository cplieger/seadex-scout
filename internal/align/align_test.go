package align_test

import (
	"reflect"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
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
			groups, hasFile, approx := align.Scope(&tt.item, &tt.rec)
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

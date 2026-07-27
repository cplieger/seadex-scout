package align_test

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
)

// TestItemKind pins the record-less classification align owns for a library
// item the audit's reverse catalogue enumerated (no SeaDex entry, hence no
// Fribb record): a Radarr item is a movie, a Sonarr item reads as the
// whole-series comparison.
func TestItemKind(t *testing.T) {
	tests := []struct {
		name string
		item library.Item
		want align.ScopeKind
	}{
		{
			name: "radarr item with no record scopes to the movie",
			item: library.Item{Arr: library.ArrRadarr, Groups: []string{"arid"}, HasFile: true},
			want: align.ScopeMovie,
		},
		{
			name: "sonarr item with no record reads as the whole series",
			item: library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{1: {"sam"}}},
			want: align.ScopeWholeSeries,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := align.ItemKind(&tt.item); got != tt.want {
				t.Errorf("ItemKind = %v, want %v", got, tt.want)
			}
		})
	}
}

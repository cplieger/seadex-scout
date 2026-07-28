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

// TestScopeKindString pins the operator-facing scope vocabulary align owns: the
// label rides the daemon finding line's scope attribute (internal/compare's
// Finding.Scope) and the audit report's scope cell, so a swapped or dropped arm
// mislabels every finding of that kind in Loki without failing anything else.
// The zero value must read "series": ScopeWholeSeries is the conservative
// default an unset kind falls back to.
func TestScopeKindString(t *testing.T) {
	tests := []struct {
		name string
		kind align.ScopeKind
		want string
	}{
		{"movie", align.ScopeMovie, "movie"},
		{"season", align.ScopeSeason, "season"},
		{"special", align.ScopeSpecial, "special"},
		{"whole series", align.ScopeWholeSeries, "series"},
		{"unknown kind falls back to the series label", align.ScopeKind(99), "series"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.String(); got != tt.want {
				t.Errorf("ScopeKind(%d).String() = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

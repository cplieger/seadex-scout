package align_test

import (
	"encoding/json"
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

// TestScopeKindJSONRoundTrip pins the published vocabulary in both directions. The
// wire form is the String() name rather than the iota, so reordering the constants
// cannot silently change what a published audit report means; and the decoder is
// the inverse rather than a lenient reader, so a token this build does not know
// fails loudly instead of collapsing onto align.ScopeWholeSeries - which String() maps
// every unknown kind to, and would therefore be a confident wrong answer.
func TestScopeKindJSONRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		kind align.ScopeKind
		wire string
	}{
		{align.ScopeWholeSeries, `"series"`},
		{align.ScopeMovie, `"movie"`},
		{align.ScopeSeason, `"season"`},
		{align.ScopeSpecial, `"special"`},
	} {
		t.Run(tc.kind.String(), func(t *testing.T) {
			data, err := json.Marshal(tc.kind)
			if err != nil {
				t.Fatalf("marshal %v: %v", tc.kind, err)
			}
			if string(data) != tc.wire {
				t.Errorf("marshal %v = %s, want %s", tc.kind, data, tc.wire)
			}
			var back align.ScopeKind
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("unmarshal %s: %v", data, err)
			}
			if back != tc.kind {
				t.Errorf("round trip of %v = %v", tc.kind, back)
			}
		})
	}

	var unknown align.ScopeKind
	if err := json.Unmarshal([]byte(`"cour"`), &unknown); err == nil {
		t.Error("an unrecognized scope token must be an error, not the series zero value")
	}
	if err := json.Unmarshal([]byte(`3`), &unknown); err == nil {
		t.Error("a numeric scope must be an error: the wire form is the name, not the iota")
	}
}

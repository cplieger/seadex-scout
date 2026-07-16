package align_test

import (
	"reflect"
	"testing"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
)

func TestWholeSeries(t *testing.T) {
	tests := []struct {
		name string
		rec  mapping.Record
		item library.Item
		want bool
	}{
		{"sonarr seasonless non-special is whole-series", mapping.Record{Type: "TV", SeasonTvdb: 0}, library.Item{Arr: library.ArrSonarr}, true},
		{"sonarr with a positive season is not whole-series", mapping.Record{Type: "TV", SeasonTvdb: 2}, library.Item{Arr: library.ArrSonarr}, false},
		{"sonarr special is not whole-series", mapping.Record{Type: "OVA", SeasonTvdb: 0}, library.Item{Arr: library.ArrSonarr}, false},
		{"radarr is never whole-series", mapping.Record{Type: "MOVIE", SeasonTvdb: 0}, library.Item{Arr: library.ArrRadarr}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := align.Scope(&tt.item, &tt.rec).Kind == align.ScopeWholeSeries
			if got != tt.want {
				t.Errorf("Scope().Kind == ScopeWholeSeries = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSummarizeWholeSeriesExcludesSeasonZeroAndUnionsGroups(t *testing.T) {
	item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{
		0: {"specialgrp"},
		1: {"a&c"},
		2: {"kh"},
	}}
	s := align.SummarizeWholeSeries(item, []string{"a&c"}, []string{"kh"})
	if s.Seasons != 2 {
		t.Errorf("Seasons = %d, want 2 (season 0 excluded)", s.Seasons)
	}
	if !s.AnyAlt {
		t.Error("AnyAlt = false, want true (season 2 carries an alt group)")
	}
	if s.AnyUnlisted {
		t.Error("AnyUnlisted = true, want false")
	}
	if want := []string{"a&c", "kh"}; !reflect.DeepEqual(s.Groups, want) {
		t.Errorf("Groups = %v, want %v (season 0 group excluded, sorted)", s.Groups, want)
	}
}

func TestSummarizeWholeSeriesNilAltTreatsBestLessSeasonAsUnlisted(t *testing.T) {
	item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{
		1: {"a&c"},
		2: {"kh"},
	}}
	s := align.SummarizeWholeSeries(item, []string{"a&c"}, nil)
	if !s.AnyUnlisted {
		t.Error("AnyUnlisted = false, want true (nil alt: a best-less season is unlisted)")
	}
	if s.AnyAlt {
		t.Error("AnyAlt = true, want false (nil alt can never match)")
	}
}

// TestSummarizeWholeSeriesDeduplicatesGroupsAcrossSeasons pins the seen-group
// dedupe in the whole-series aggregate: a group present in several seasons
// appears once in Groups, not once per season.
func TestSummarizeWholeSeriesDeduplicatesGroupsAcrossSeasons(t *testing.T) {
	item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{
		1: {"shared", "alpha"},
		2: {"shared", "beta"},
	}}
	got := align.SummarizeWholeSeries(item, []string{"shared"}, nil)
	want := []string{"alpha", "beta", "shared"}
	if !reflect.DeepEqual(got.Groups, want) {
		t.Errorf("Groups = %v, want deduplicated sorted groups %v", got.Groups, want)
	}
}

func TestSummarizeWholeSeriesSkipsEmptySeasons(t *testing.T) {
	item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: map[int][]string{
		1: {},
		2: {"a&c"},
	}}
	s := align.SummarizeWholeSeries(item, []string{"a&c"}, nil)
	if s.Seasons != 1 {
		t.Errorf("Seasons = %d, want 1 (an empty season contributes nothing)", s.Seasons)
	}
	if s.AnyUnlisted {
		t.Error("AnyUnlisted = true, want false (only the best season counted)")
	}
}

// TestSummarizeWholeSeriesApprox pins the coarseness rule on the whole-series
// aggregate: the comparison is approximate exactly when it spans more than one
// real season or more than one release group, and season 0 / empty seasons
// never contribute to that count.
func TestSummarizeWholeSeriesApprox(t *testing.T) {
	tests := []struct {
		name    string
		seasons map[int][]string
		want    bool
	}{
		{"one season with one group is exact", map[int][]string{1: {"a&c"}}, false},
		{"two seasons sharing one group are approximate", map[int][]string{1: {"a&c"}, 2: {"a&c"}}, true},
		{"one season with two groups is approximate", map[int][]string{1: {"a&c", "kh"}}, true},
		{"season 0 groups never make it approximate", map[int][]string{0: {"x", "y"}, 1: {"a&c"}}, false},
		{"an empty season never makes it approximate", map[int][]string{1: {"a&c"}, 2: {}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Arr: library.ArrSonarr, SeasonGroups: tt.seasons}
			got := align.SummarizeWholeSeries(item, []string{"a&c"}, nil)
			if got.Approx != tt.want {
				t.Errorf("SummarizeWholeSeries(%v).Approx = %v, want %v", tt.seasons, got.Approx, tt.want)
			}
		})
	}
}

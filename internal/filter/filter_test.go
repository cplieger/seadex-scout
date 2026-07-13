package filter

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
)

func TestKeepNonTracker(t *testing.T) {
	tests := []struct {
		name     string
		rel      release.Release
		opts     Options
		wantKeep bool
	}{
		{"remux kept by default", release.Release{Kind: release.KindRemux}, Options{}, true},
		{"remux dropped when excluded", release.Release{Kind: release.KindRemux}, Options{ExcludeRemux: true}, false},
		{"unknown kind never dropped by remux policy", release.Release{Kind: release.KindUnknown}, Options{ExcludeRemux: true}, true},
		{"encode kept when exclude_remux", release.Release{Kind: release.KindEncode}, Options{ExcludeRemux: true}, true},
		{"non-dual dropped when dual required", release.Release{DualAudio: false}, Options{RequireDualAudio: true}, false},
		{"dual kept when dual required", release.Release{DualAudio: true}, Options{RequireDualAudio: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keep, reason := KeepNonTracker(&tt.rel, tt.opts)
			if keep != tt.wantKeep {
				t.Errorf("KeepNonTracker() keep = %v, want %v (reason %q)", keep, tt.wantKeep, reason)
			}
			if !keep && reason == "" {
				t.Error("a dropped release must carry a reason")
			}
		})
	}
}

func TestObtainable(t *testing.T) {
	tests := []struct {
		name string
		rel  release.Release
		opts Options
		want bool
	}{
		{"public always obtainable", release.Release{TrackerType: release.TrackerPublic}, Options{}, true},
		{"animebytes obtainable when enabled", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, Options{AnimeBytes: true}, true},
		{"animebytes not obtainable when disabled", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, Options{}, false},
		{"other private tracker never obtainable even with AB on", release.Release{TrackerType: release.TrackerPrivate, Tracker: "beyondhd"}, Options{AnimeBytes: true}, false},
		{"unknown tracker not obtainable", release.Release{TrackerType: release.TrackerUnknown}, Options{AnimeBytes: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Obtainable(&tt.rel, tt.opts); got != tt.want {
				t.Errorf("Obtainable() = %v, want %v", got, tt.want)
			}
		})
	}
}

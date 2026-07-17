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
			if got := Obtainable(&tt.rel, "", tt.opts); got != tt.want {
				t.Errorf("Obtainable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExcludeSpecial(t *testing.T) {
	tests := map[string]struct {
		isSpecial       bool
		excludeSpecials bool
		want            bool
	}{
		"ordinary entry remains visible when exclusion is off": {false, false, false},
		"ordinary entry remains visible when exclusion is on":  {false, true, false},
		"special remains visible when exclusion is off":        {true, false, false},
		"special is excluded when exclusion is on":             {true, true, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := ExcludeSpecial(tt.isSpecial, tt.excludeSpecials); got != tt.want {
				t.Errorf("ExcludeSpecial(%v, %v) = %v, want %v", tt.isSpecial, tt.excludeSpecials, got, tt.want)
			}
		})
	}
}

func TestABVisible(t *testing.T) {
	tests := []struct {
		name       string
		tracker    string
		url        string
		animeBytes bool
		want       bool
	}{
		{"AB tracker hidden when off", "AB", "https://animebytes.tv/torrents.php?id=1", false, false},
		{"AB tracker visible when on", "AB", "https://animebytes.tv/torrents.php?id=1", true, true},
		{"public tracker with public URL visible when off", "Nyaa", "https://nyaa.si/view/1", false, true},
		{"mislabeled public tracker with AB URL hidden when off", "Nyaa", "https://animebytes.tv/torrents.php?id=1", false, false},
		{"mislabeled public tracker with AB subdomain URL hidden when off", "Nyaa", "https://cdn.animebytes.tv/t/1", false, false},
		{"mislabeled public tracker with trailing-dot AB FQDN hidden when off", "Nyaa", "https://animebytes.tv./torrents.php?id=1", false, false},
		{"AB-suffix lookalike host is not AnimeBytes", "Nyaa", "https://notanimebytes.tv/t/1", false, true},
		{"mislabeled public tracker with AB URL visible when on", "Nyaa", "https://animebytes.tv/torrents.php?id=1", true, true},
		{"malformed URL hidden conservatively when off", "Nyaa", "https://nyaa.si/\x7f", false, false},
		{"malformed absolute URL with scheme but no host hidden when off", "Nyaa", "https:/animebytes.tv/torrents.php?id=1", false, false},
		{"schemeless AB host with port hidden when off", "Nyaa", "animebytes.tv:443/torrents.php?id=1", false, false},
		{"schemeless AB subdomain with port hidden when off", "Nyaa", "cdn.animebytes.tv:443/t/1", false, false},
		{"empty URL carries no link and passes", "Nyaa", "", false, true},
		{"relative path has no host and passes", "unknown", "/local/path", false, true},
		{"schemeless AB URL hidden when off", "unknown", "animebytes.tv/torrents.php?id=1&torrentid=2", false, false},
		{"schemeless AB subdomain URL hidden when off", "Nyaa", "cdn.animebytes.tv/t/1", false, false},
		{"schemeless non-AB URL visible when off", "Nyaa", "nyaa.si/view/1", false, true},
		{"schemeless URL failing authority reparse hidden when off", "Nyaa", `animebytes.tv\@evil/x`, false, false},
		{"schemeless URL with space-userinfo failing reparse hidden when off", "Nyaa", "foo bar@animebytes.tv/x", false, false},
		{"backslash protocol-relative AB URL hidden when off", "Nyaa", `/\animebytes.tv/x`, false, false},
		{"double-backslash AB URL hidden when off", "Nyaa", `\\animebytes.tv/x`, false, false},
		{"multi-slash protocol-relative AB URL hidden when off", "Nyaa", "///animebytes.tv/x", false, false},
		{"unicode fullwidth-dot AB host hidden when off", "Nyaa", "https://animebytes\uFF0Etv/torrents.php?id=1", false, false},
		{"unicode ideographic-dot AB host hidden when off", "Nyaa", "https://animebytes\u3002tv/torrents.php?id=1", false, false},
		{"unicode fullwidth-letter AB host hidden when off", "Nyaa", "https://animebyte\uFF53.tv/torrents.php?id=1", false, false},
		{"unicode fullwidth-dot AB host visible when on", "Nyaa", "https://animebytes\uFF0Etv/torrents.php?id=1", true, true},
		{"empty-label AB host is not AnimeBytes (unresolvable, visible when off)", "Nyaa", "https://.animebytes.tv/x", false, true},
		{"inner-empty-label AB host is not AnimeBytes (unresolvable, visible when off)", "Nyaa", "https://a..animebytes.tv/x", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ABVisible(tt.tracker, tt.url, tt.animeBytes); got != tt.want {
				t.Errorf("ABVisible(%q, %q, %v) = %v, want %v", tt.tracker, tt.url, tt.animeBytes, got, tt.want)
			}
		})
	}
}

// TestObtainableAppliesABURLCrossCheck pins the Obtainable->ABVisible wiring:
// the raw upstream URL passed to Obtainable must feed the AnimeBytes host
// cross-check, so a mislabeled public release carrying an AB URL never counts
// as obtainable while the animebytes toggle is off.
func TestObtainableAppliesABURLCrossCheck(t *testing.T) {
	abURL := "https://animebytes.tv/torrents.php?id=1&torrentid=2"
	tests := []struct {
		name   string
		rel    release.Release
		rawURL string
		opts   Options
		want   bool
	}{
		{"public tracker with AB URL hidden when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, abURL, Options{}, false},
		{"public tracker with AB subdomain URL hidden when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://cdn.animebytes.tv/t/1", Options{}, false},
		{"public tracker with AB URL obtainable when AB on", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, abURL, Options{AnimeBytes: true}, true},
		{"public tracker with public URL obtainable when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://nyaa.si/view/1", Options{}, true},
		{"public tracker with malformed URL hidden when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://nyaa.si/\x7f", Options{}, false},
		{"AB release with AB URL obtainable when AB on", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, abURL, Options{AnimeBytes: true}, true},
		{"AB release with AB URL hidden when AB off", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, abURL, Options{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Obtainable(&tt.rel, tt.rawURL, tt.opts); got != tt.want {
				t.Errorf("Obtainable(%q, %q, %+v) = %v, want %v", tt.rel.Tracker, tt.rawURL, tt.opts, got, tt.want)
			}
		})
	}
}

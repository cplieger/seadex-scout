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

// TestRequireDualAudioKeysOnStructuredFlag pins require_dual_audio to the
// classifier's structured dual-audio sourcing end to end: a release whose
// structured flag is set passes whatever the text says, while a release whose
// text merely mentions "dual audio" (a name tag, or the entry-wide SeaDex
// notes — which can even negate: "lacks dual audio") is dropped, because text
// is never dual-audio evidence.
func TestRequireDualAudioKeysOnStructuredFlag(t *testing.T) {
	opts := Options{RequireDualAudio: true}

	flagged := release.Classify(&release.Input{DualAudio: true, Notes: "lacks dual audio"})
	if keep, reason := KeepNonTracker(&flagged, opts); !keep {
		t.Errorf("structured dual-audio release dropped (%q); the flag must pass require_dual_audio", reason)
	}

	for _, tt := range []struct {
		name string
		in   release.Input
	}{
		{name: "notes mention", in: release.Input{Notes: "this release is dual audio"}},
		{name: "negated notes mention", in: release.Input{Notes: "lacks dual audio"}},
		{name: "name tag", in: release.Input{Names: []string{"Show - 01 [1080p][Dual Audio].mkv"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rel := release.Classify(&tt.in)
			if keep, _ := KeepNonTracker(&rel, opts); keep {
				t.Error("text-only dual-audio mention passed require_dual_audio; the structured flag is the only evidence")
			}
		})
	}
}

func TestObtainable(t *testing.T) {
	tests := []struct {
		name       string
		rel        release.Release
		rawURL     string
		usableURL  string
		animeBytes bool
		want       bool
	}{
		{"public always obtainable", release.Release{TrackerType: release.TrackerPublic}, "", "https://nyaa.si/view/1", false, true},
		{"animebytes obtainable when enabled", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, "", "https://animebytes.tv/torrents.php?id=1", true, true},
		{"animebytes not obtainable when disabled", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, "", "https://animebytes.tv/torrents.php?id=1", false, false},
		{"other private tracker never obtainable even with AB on", release.Release{TrackerType: release.TrackerPrivate, Tracker: "beyondhd"}, "", "https://beyondhd.co/t/1", true, false},
		{"unknown tracker not obtainable", release.Release{TrackerType: release.TrackerUnknown}, "", "https://example.com/t/1", true, false},
		{"public with empty usable URL not obtainable", release.Release{TrackerType: release.TrackerPublic}, "", "", false, false},
		{"animebytes with empty usable URL not obtainable even when enabled", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, "", "", true, false},
		{"mislabeled AB torrent-page relative raw URL not obtainable when off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "/torrents.php?id=1&torrentid=2", "https://animebytes.tv/torrents.php?id=1&torrentid=2", false, false},
		{"mislabeled AB torrent-page relative raw URL obtainable when on", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "/torrents.php?id=1&torrentid=2", "https://animebytes.tv/torrents.php?id=1&torrentid=2", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Obtainable(&tt.rel, tt.rawURL, tt.usableURL, tt.animeBytes); got != tt.want {
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
		{"hidden-host AB form hidden when off (recovered evidence)", "Nyaa", "https:/animebytes.tv/torrents.php?id=1", false, false},
		{"hidden-host public form visible when off (recovered evidence)", "Nyaa", "https:/nyaa.si/view/1", false, true},
		{"zero-slash AB form hidden when off (recovered evidence)", "Nyaa", "https:animebytes.tv/torrents.php?id=1", false, false},
		{"tab-smuggled AB URL hidden when off", "Nyaa", "https://anime\tbytes.tv/torrents.php?id=1", false, false},
		{"schemeless AB host with port hidden when off", "Nyaa", "animebytes.tv:443/torrents.php?id=1", false, false},
		{"schemeless AB subdomain with port hidden when off", "Nyaa", "cdn.animebytes.tv:443/t/1", false, false},
		{"empty URL carries no link and passes", "Nyaa", "", false, true},
		{"relative path has no host and passes", "unknown", "/local/path", false, true},
		{"AB torrent-page relative URL hidden when off despite public label", "Nyaa", "/torrents.php?id=1&torrentid=2", false, false},
		{"AB torrent-page relative URL visible when on", "Nyaa", "/torrents.php?id=1&torrentid=2", true, true},
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
// as obtainable while the animebytes toggle is off. It also pins the
// usable-URL gate: a release whose canonical usable URL is empty (no URL, or
// one seadex.Torrent.UsableURL rejected as malformed or foreign-host) is
// never obtainable regardless of tracker and toggle.
func TestObtainableAppliesABURLCrossCheck(t *testing.T) {
	abURL := "https://animebytes.tv/torrents.php?id=1&torrentid=2"
	tests := []struct {
		name       string
		rel        release.Release
		rawURL     string
		usableURL  string
		animeBytes bool
		want       bool
	}{
		{"public tracker with AB URL hidden when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, abURL, abURL, false, false},
		{"public tracker with AB subdomain URL hidden when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://cdn.animebytes.tv/t/1", "https://cdn.animebytes.tv/t/1", false, false},
		{"public tracker with AB URL obtainable when AB on", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, abURL, abURL, true, true},
		{"public tracker with public URL obtainable when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://nyaa.si/view/1", "https://nyaa.si/view/1", false, true},
		{"public tracker with malformed raw URL hidden by ABVisible even with a usable URL", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://nyaa.si/\x7f", "https://nyaa.si/view/1", false, false},
		{"public tracker with foreign-host URL rejected by UsableURL not obtainable", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://evil.example/view/1", "", false, false},
		{"public tracker with no URL at all not obtainable", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "", "", false, false},
		{"public tracker with AB torrent-page relative URL hidden when AB off", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "/torrents.php?id=1&torrentid=2", abURL, false, false},
		{"AB release with AB URL obtainable when AB on", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, abURL, abURL, true, true},
		{"AB release with AB URL hidden when AB off", release.Release{TrackerType: release.TrackerPrivate, Tracker: "AB"}, abURL, abURL, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Obtainable(&tt.rel, tt.rawURL, tt.usableURL, tt.animeBytes); got != tt.want {
				t.Errorf("Obtainable(%q, %q, %q, %v) = %v, want %v", tt.rel.Tracker, tt.rawURL, tt.usableURL, tt.animeBytes, got, tt.want)
			}
		})
	}
}

// TestABGatedMatchesToggleOffVisibility pins ABGated as the named form of the
// toggle-off hide decision consumed by the alert URL routing
// (notify.trackerURLs):
// an AB label or AB-hosted URL is gated, a public link is not, and the
// conservative hides (malformed or non-ASCII host evidence) are gated even
// though DefinitelyAB fails open on them - the asymmetry the two predicates
// exist to encode.
func TestABGatedMatchesToggleOffVisibility(t *testing.T) {
	tests := []struct {
		name    string
		tracker string
		url     string
		want    bool
	}{
		{"AB label gated", "AB", "https://animebytes.tv/torrents.php?id=1", true},
		{"public URL not gated", "Nyaa", "https://nyaa.si/view/1", false},
		{"AB URL under public label gated", "Nyaa", "https://animebytes.tv/torrents.php?id=1", true},
		{"malformed URL gated conservatively but not definitely AB", "Nyaa", "https://nyaa.si/\x7f", true},
		{"non-ASCII AB host gated conservatively but not definitely AB", "Nyaa", "https://animebytes\uFF0Etv/torrents.php?id=1", true},
		{"empty URL not gated", "Nyaa", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ABGated(tt.tracker, tt.url); got != tt.want {
				t.Errorf("ABGated(%q, %q) = %v, want %v", tt.tracker, tt.url, got, tt.want)
			}
		})
	}
}

// TestDefinitelyAB pins the fail-OPEN contract of DefinitelyAB (the inverse
// fail direction of ABVisible's fail-closed gate): evidence that cannot be
// extracted is not AnimeBytes evidence, so malformed, hidden-host,
// unrecoverable-authority, and non-ASCII hosts all read false (the release
// stays LISTED, annotated), while an AB tracker label or extractable AB host
// evidence reads true. Each true row is also cross-checked against ABGated:
// the fail-open set must stay a subset of the fail-closed gate, so a definite
// AB release is always hidden with the toggle off.
func TestDefinitelyAB(t *testing.T) {
	tests := []struct {
		name    string
		tracker string
		url     string
		want    bool
	}{
		{"AB label with no URL", "AB", "", true},
		{"animebytes label with public URL", "animebytes", "https://nyaa.si/view/1", true},
		{"public label with AB URL", "Nyaa", "https://animebytes.tv/torrents.php?id=1", true},
		{"public label with AB subdomain URL", "Nyaa", "https://cdn.animebytes.tv/t/1", true},
		{"public label with trailing-dot AB FQDN", "Nyaa", "https://animebytes.tv./torrents.php?id=1", true},
		{"schemeless AB host", "Nyaa", "animebytes.tv/torrents.php?id=1&torrentid=2", true},
		{"protocol-relative AB host", "Nyaa", "//animebytes.tv/x", true},
		{"backslash-canonicalized AB host is definite (browser semantics)", "Nyaa", `animebytes.tv\@evil/x`, true},
		{"public label with public URL", "Nyaa", "https://nyaa.si/view/1", false},
		{"empty URL carries no evidence", "Nyaa", "", false},
		{"relative path carries no host evidence", "Nyaa", "/local/path", false},
		{"AB torrent-page relative URL is definitive", "Nyaa", "/torrents.php?id=1&torrentid=2", true},
		{"lookalike suffix host is not AB", "Nyaa", "https://notanimebytes.tv/t/1", false},
		{"AB-suffixed foreign domain is not AB", "Nyaa", "https://animebytes.tv.evil.example/t/1", false},
		{"malformed URL fails open", "Nyaa", "https://nyaa.si/\x7f", false},
		{"hidden-host special form recovers definite AB evidence", "Nyaa", "https:/animebytes.tv/torrents.php?id=1", true},
		{"zero-slash AB form recovers definite AB evidence", "Nyaa", "https:animebytes.tv/torrents.php?id=1", true},
		{"tab-smuggled AB URL is definite (browser strips the tab)", "Nyaa", "https://anime\tbytes.tv/torrents.php?id=1", true},
		{"opaque host-as-scheme still fails open (non-special, no recovery)", "Nyaa", "animebytes.tv:443/x", false},
		{"space-userinfo host failing authority reparse fails open", "Nyaa", "foo bar@animebytes.tv/x", false},
		{"non-ASCII fullwidth-dot AB host fails open", "Nyaa", "https://animebytes\uFF0Etv/torrents.php?id=1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefinitelyAB(tt.tracker, tt.url)
			if got != tt.want {
				t.Errorf("DefinitelyAB(%q, %q) = %v, want %v", tt.tracker, tt.url, got, tt.want)
			}
			if got && !ABGated(tt.tracker, tt.url) {
				t.Errorf("DefinitelyAB(%q, %q) = true but ABGated = false; the fail-open set must stay a subset of the fail-closed gate", tt.tracker, tt.url)
			}
		})
	}
}

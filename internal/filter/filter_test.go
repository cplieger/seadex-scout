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
		{"schemeless AB torrent-page shape hidden when off", "Nyaa", "torrents.php?id=1&torrentid=2", false, false},
		{"backslash-rooted AB relative URL hidden when off", "Nyaa", `\torrents.php?id=1&torrentid=2`, false, false},
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
// one trackerlink.Publish rejected as malformed or foreign-host) is
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
		{"public tracker with foreign-host URL rejected by the publisher not obtainable", release.Release{TrackerType: release.TrackerPublic, Tracker: "Nyaa"}, "https://evil.example/view/1", "", false, false},
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

// TestClassifyAB pins the grade of every AnimeBytes-evidence shape, replacing
// the two boolean tables (fail-closed gate, fail-open predicate) this one table
// now covers in a single pass. The three grades carry the two fail directions
// the app needs: ABNone surfaces with the toggle off, ABDefinite is the audit
// report's row-listing gate, and ABAmbiguous is the band where the two
// directions disagree - hidden by ABVisible, still LISTED by the report.
//
// The subset invariant the old tables cross-checked (definite implies gated) is
// now structural: one value cannot be definite without also being non-None, so
// it is asserted once through ABVisible's exhaustive reading below rather than
// restated per row.
func TestClassifyAB(t *testing.T) {
	tests := []struct {
		name    string
		tracker string
		url     string
		want    ABEvidence
	}{
		{"AB label with no URL", "AB", "", ABDefinite},
		{"animebytes label with public URL", "animebytes", "https://nyaa.si/view/1", ABDefinite},
		{"public label with AB URL", "Nyaa", "https://animebytes.tv/torrents.php?id=1", ABDefinite},
		{"public label with AB subdomain URL", "Nyaa", "https://cdn.animebytes.tv/t/1", ABDefinite},
		{"public label with trailing-dot AB FQDN", "Nyaa", "https://animebytes.tv./torrents.php?id=1", ABDefinite},
		{"schemeless AB host", "Nyaa", "animebytes.tv/torrents.php?id=1&torrentid=2", ABDefinite},
		{"protocol-relative AB host", "Nyaa", "//animebytes.tv/x", ABDefinite},
		{"backslash-canonicalized AB host is definite (browser semantics)", "Nyaa", `animebytes.tv\@evil/x`, ABDefinite},
		{"AB torrent-page relative URL is definitive", "Nyaa", "/torrents.php?id=1&torrentid=2", ABDefinite},
		{"schemeless AB torrent-page shape is definitive", "Nyaa", "torrents.php?id=1&torrentid=2", ABDefinite},
		{"hidden-host special form recovers definite AB evidence", "Nyaa", "https:/animebytes.tv/torrents.php?id=1", ABDefinite},
		{"zero-slash AB form recovers definite AB evidence", "Nyaa", "https:animebytes.tv/torrents.php?id=1", ABDefinite},
		{"tab-smuggled AB URL is definite (browser strips the tab)", "Nyaa", "https://anime\tbytes.tv/torrents.php?id=1", ABDefinite},

		{"public label with public URL", "Nyaa", "https://nyaa.si/view/1", ABNone},
		{"empty URL carries no evidence", "Nyaa", "", ABNone},
		{"relative path carries no host evidence", "Nyaa", "/local/path", ABNone},
		{"lookalike suffix host is not AB", "Nyaa", "https://notanimebytes.tv/t/1", ABNone},
		{"AB-suffixed foreign domain is not AB", "Nyaa", "https://animebytes.tv.evil.example/t/1", ABNone},

		{"malformed URL settles nothing", "Nyaa", "https://nyaa.si/\x7f", ABAmbiguous},
		{"opaque host-as-scheme settles nothing (non-special, no recovery)", "Nyaa", "animebytes.tv:443/x", ABAmbiguous},
		{"space-userinfo host failing authority reparse settles nothing", "Nyaa", "foo bar@animebytes.tv/x", ABAmbiguous},
		{"non-ASCII fullwidth-dot AB host settles nothing", "Nyaa", "https://animebytes\uFF0Etv/torrents.php?id=1", ABAmbiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyAB(tt.tracker, tt.url); got != tt.want {
				t.Errorf("ClassifyAB(%q, %q) = %d, want %d", tt.tracker, tt.url, got, tt.want)
			}
		})
	}
}

// TestABVisibleReadsEveryGrade pins the toggle policy over the grades: with the
// toggle on everything surfaces, and with it off ONLY ABNone does. That is the
// structural form of the old "definite is a subset of gated" cross-check - the
// definite and ambiguous grades are hidden by the same comparison, so a definite
// AB release can no longer be visible while an ambiguous one is hidden. The
// table is keyed by subtest NAME rather than by the grade, so the readable
// subtest names need no production formatter.
func TestABVisibleReadsEveryGrade(t *testing.T) {
	grades := map[string]struct {
		tracker string
		url     string
		grade   ABEvidence
	}{
		"none":      {tracker: "Nyaa", url: "https://nyaa.si/view/1", grade: ABNone},
		"ambiguous": {tracker: "Nyaa", url: "https://nyaa.si/\x7f", grade: ABAmbiguous},
		"definite":  {tracker: "Nyaa", url: "https://animebytes.tv/torrents.php?id=1", grade: ABDefinite},
	}
	for name, in := range grades {
		t.Run(name, func(t *testing.T) {
			// Guard the fixtures: a grading change must fail here rather than
			// silently retarget the policy assertion at the wrong grade.
			if got := ClassifyAB(in.tracker, in.url); got != in.grade {
				t.Fatalf("fixture drift: ClassifyAB(%q, %q) = %d, want %d", in.tracker, in.url, got, in.grade)
			}
			if !ABVisible(in.tracker, in.url, true) {
				t.Errorf("ABVisible(%d, toggle on) = false, want true", in.grade)
			}
			wantOff := in.grade == ABNone
			if got := ABVisible(in.tracker, in.url, false); got != wantOff {
				t.Errorf("ABVisible(%d, toggle off) = %v, want %v", in.grade, got, wantOff)
			}
		})
	}
}

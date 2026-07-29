package notify

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
)

// TestTrackerURLs pins the alert link-splitting rules WITHIN one affinity
// tier (every fixture here is non-headline): the first Nyaa link wins the
// nyaa slot, the first AnimeBytes link wins the ab slot, and when no Nyaa
// link exists the first other public link (e.g. AnimeTosho) stands in as the
// public URL so an alert never renders an empty public link while one exists.
// Headline affinity outranks all of that; see
// TestTrackerURLsPrefersHeadlineGroupOverNyaaTier.
func TestTrackerURLs(t *testing.T) {
	tests := []struct {
		name     string
		wantNyaa string
		wantAB   string
		links    []compare.ReleaseLink
	}{
		{
			name: "nyaa and ab split",
			links: []compare.ReleaseLink{
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
				{Tracker: "AB", URL: "https://animebytes.tv/t/1"},
			},
			wantNyaa: "https://nyaa.si/view/1",
			wantAB:   "https://animebytes.tv/t/1",
		},
		{
			name:     "ab only leaves nyaa empty",
			links:    []compare.ReleaseLink{{Tracker: "animebytes", URL: "https://animebytes.tv/t/2"}},
			wantNyaa: "",
			wantAB:   "https://animebytes.tv/t/2",
		},
		{
			name:     "other public tracker fills the nyaa slot",
			links:    []compare.ReleaseLink{{Tracker: "AnimeTosho", URL: "https://animetosho.org/v/3"}},
			wantNyaa: "https://animetosho.org/v/3",
			wantAB:   "",
		},
		{
			name: "real nyaa link wins over an earlier other-public link",
			links: []compare.ReleaseLink{
				{Tracker: "AnimeTosho", URL: "https://animetosho.org/v/3"},
				{Tracker: "nyaa", URL: "https://nyaa.si/view/4"},
			},
			wantNyaa: "https://nyaa.si/view/4",
			wantAB:   "",
		},
		{
			name: "first of each tracker wins",
			links: []compare.ReleaseLink{
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/5"},
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/6"},
				{Tracker: "AB", URL: "https://animebytes.tv/t/5"},
				{Tracker: "AB", URL: "https://animebytes.tv/t/6"},
			},
			wantNyaa: "https://nyaa.si/view/5",
			wantAB:   "https://animebytes.tv/t/5",
		},
		{name: "no links", links: nil, wantNyaa: "", wantAB: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pub, abLink := trackerURLs(gradedLinks(tc.links...))
			ab := abLink.url
			if pub.url != tc.wantNyaa {
				t.Errorf("public url = %q, want %q", pub.url, tc.wantNyaa)
			}
			if ab != tc.wantAB {
				t.Errorf("ab = %q, want %q", ab, tc.wantAB)
			}
		})
	}
}

// TestSeadexTags pins the alert-footer tag line per status arm, including the
// arms the existing finding fixtures never exercise (incomplete, theoretical,
// mixed-group), the unknown-kind suppression, and the no-dual-audio case.
func TestSeadexTags(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		finding compare.Finding
	}{
		{
			name:    "better with full detail",
			finding: compare.Finding{Status: compare.StatusBetter, Kind: "encode", Resolution: "1080p", DualAudio: true},
			want:    "best · encode · 1080p · dual-audio",
		},
		{
			name:    "incomplete bare",
			finding: compare.Finding{Status: compare.StatusIncomplete},
			want:    "incomplete",
		},
		{
			name:    "theoretical with remux and resolution",
			finding: compare.Finding{Status: compare.StatusTheoretical, Kind: "remux", Resolution: "2160p"},
			want:    "theoretical-best · remux · 2160p",
		},
		{
			name:    "mixed group with dual audio",
			finding: compare.Finding{Status: compare.StatusMixedGroup, DualAudio: true},
			want:    "mixed-group · dual-audio",
		},
		{
			name:    "unverifiable with resolution",
			finding: compare.Finding{Status: compare.StatusUnverifiable, Resolution: "1080p"},
			want:    "unverifiable · 1080p",
		},
		{
			name:    "unknown kind is suppressed",
			finding: compare.Finding{Status: compare.StatusBetter, Kind: "unknown", Resolution: "720p"},
			want:    "best · 720p",
		},
		{
			name:    "unmapped status omits the qualifier tag",
			finding: compare.Finding{Status: compare.Status("future_status"), Kind: "encode", Resolution: "1080p"},
			want:    "encode · 1080p",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := seadexTags(&tc.finding); got != tc.want {
				t.Errorf("seadexTags = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTrackerURLsRoutesMislabeledABURLToABSlot pins the URL-aware half of
// the AB routing rule: the tracker label is untrusted upstream data, so a
// link labeled "Nyaa" whose URL points at animebytes.tv must land in the AB
// slot (hidden while the toggle is off), never in the public/Nyaa slot, and
// the genuine Nyaa link still wins the nyaa slot.
func TestTrackerURLsRoutesMislabeledABURLToABSlot(t *testing.T) {
	links := gradedLinks(
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://animebytes.tv/torrents.php?id=9"},
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/9"},
	)
	pub, abLink := trackerURLs(links)
	ab := abLink.url
	if ab != "https://animebytes.tv/torrents.php?id=9" {
		t.Errorf("ab = %q, want the mislabeled animebytes.tv URL routed to the AB slot", ab)
	}
	if pub.url != "https://nyaa.si/view/9" {
		t.Errorf("public url = %q, want the genuine Nyaa URL", pub.url)
	}
}

// TestTrackerURLsDefiniteABWinsOverMalformedFallback pins the precedence of
// definite AnimeBytes evidence over the fail-closed fallback: a malformed
// Nyaa-labeled URL (unclassifiable, so filter.ABAmbiguous) appearing BEFORE a
// genuine AnimeBytes link must not occupy the AB slot - the later definite AB
// URL wins it, and the unclassifiable link still never renders as the public
// URL.
func TestTrackerURLsDefiniteABWinsOverMalformedFallback(t *testing.T) {
	links := gradedLinks(
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://animebytes.tv exploit"},
		compare.ReleaseLink{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=9&torrentid=10"},
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/9"},
	)
	pub, abLink := trackerURLs(links)
	ab := abLink.url
	if ab != "https://animebytes.tv/torrents.php?id=9&torrentid=10" {
		t.Errorf("ab = %q, want the definite AnimeBytes URL to win the AB slot over the malformed fallback", ab)
	}
	if pub.url != "https://nyaa.si/view/9" {
		t.Errorf("public url = %q, want the genuine Nyaa URL, never the unclassifiable one", pub.url)
	}
}

// TestTrackerURLsMalformedURLFailsClosedToABSlot pins the conservative fail
// direction trackerURLs documents: a link whose raw URL is malformed,
// host-hiding, or has a non-ASCII (homoglyph) host is unclassifiable, so it
// must fill the AB slot (hidden while the toggle is off) and never render as
// the clickable public URL - even when its tracker label claims a public
// tracker. The genuine Nyaa link still wins the nyaa slot.
func TestTrackerURLsMalformedURLFailsClosedToABSlot(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "malformed URL", url: "https://animebytes.tv exploit"},
		{name: "hidden host form", url: "https:/animebytes.tv/torrents.php?id=9"},
		{name: "non-ascii homoglyph host", url: "https://animebytes\uff0etv/torrents.php?id=9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			links := gradedLinks(
				compare.ReleaseLink{Tracker: "Nyaa", URL: tc.url},
				compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/9"},
			)
			pub, abLink := trackerURLs(links)
			ab := abLink.url
			if ab != tc.url {
				t.Errorf("ab = %q, want the unclassifiable URL %q routed to the AB slot (fail closed)", ab, tc.url)
			}
			if pub.url != "https://nyaa.si/view/9" {
				t.Errorf("public url = %q, want the genuine Nyaa URL, never the unclassifiable one", pub.url)
			}
		})
	}
}

// TestPublicLinkAlertLabel pins the alert label a public source is rendered
// under: canonicalTracker prefers the canonical tracker table over the
// untrusted SeaDex label, falls back to the URL's own host when the label
// names no known tracker, and labels an unknown host with itself so a
// non-Nyaa public link is never published nameless (the l-f5 defect). The
// existing tests only ever supply canonical-tracker hosts, so the host
// fallback and the self-labelling last resort are unexercised.
func TestPublicLinkAlertLabel(t *testing.T) {
	tests := []struct {
		name        string
		tracker     string
		url         string
		wantNyaa    string
		wantOther   string
		wantTracker string
	}{
		{
			name: "canonical label wins", tracker: "nyaa", url: "https://nyaa.si/view/1",
			wantNyaa: "https://nyaa.si/view/1",
		},
		{
			name: "blank label resolved by known host", tracker: "", url: "https://nyaa.si/view/2",
			wantNyaa: "https://nyaa.si/view/2",
		},
		{
			name: "unknown label resolved by a known tracker subdomain", tracker: "Unknown",
			url:       "https://mirror.animetosho.org/v/3",
			wantOther: "https://mirror.animetosho.org/v/3", wantTracker: "AnimeTosho",
		},
		{
			name: "host naming no known tracker labels the link with itself", tracker: "Unknown",
			url:       "https://tracker.example/v/4",
			wantOther: "https://tracker.example/v/4", wantTracker: "tracker.example",
		},
		{
			name: "hostless value has nothing to name it", tracker: "Unknown", url: "/view/5",
			wantOther: "/view/5", wantTracker: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pub, abLink := trackerURLs(gradedLinks(compare.ReleaseLink{Tracker: tc.tracker, URL: tc.url}))
			ab := abLink.url
			if ab != "" {
				t.Fatalf("ab = %q, want the link routed to a public slot", ab)
			}
			if got := pub.nyaaURL(); got != tc.wantNyaa {
				t.Errorf("nyaa_url = %q, want %q", got, tc.wantNyaa)
			}
			if got := pub.otherURL(); got != tc.wantOther {
				t.Errorf("public_url = %q, want %q", got, tc.wantOther)
			}
			if got := pub.otherTracker(); got != tc.wantTracker {
				t.Errorf("public_tracker = %q, want %q", got, tc.wantTracker)
			}
		})
	}
}

// TestTrackerURLsHostOverridesMismatchedLabel pins the host-first precedence
// canonicalTracker applies when the untrusted SeaDex label contradicts the
// URL: a link labeled "Nyaa" whose host is AnimeTosho must NOT occupy the
// nyaa slot (alerts.yaml renders that slot under a hardcoded "[Nyaa]" label,
// so it would mislabel the destination) and must not displace the genuine
// Nyaa link that follows it. Every other case in this file supplies matching
// label/host pairs, so a label-first canonicalTracker (or a label-only Nyaa
// decision in classifyTrackerLink) still satisfies them; these assertions
// fail under either regression.
func TestTrackerURLsHostOverridesMismatchedLabel(t *testing.T) {
	links := gradedLinks(
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://animetosho.org/view/1"},
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
	)
	pub, abLink := trackerURLs(links)
	ab := abLink.url
	if ab != "" {
		t.Fatalf("ab = %q, want both public links routed to public slots", ab)
	}
	if got := pub.nyaaURL(); got != "https://nyaa.si/view/2" {
		t.Errorf("nyaa_url = %q, want the genuine Nyaa URL", got)
	}
	if got := pub.otherURL(); got != "" {
		t.Errorf("public_url = %q, want empty (the Nyaa link wins the public slot)", got)
	}
}

// TestTrackerURLsMismatchedLabelAloneRendersItsRealTracker pins the same
// host-first rule for a lone mislabeled link: an AnimeTosho URL labeled
// "Nyaa" renders as public_url with public_tracker "AnimeTosho", never as
// nyaa_url.
func TestTrackerURLsMismatchedLabelAloneRendersItsRealTracker(t *testing.T) {
	pub, abLink := trackerURLs(gradedLinks(
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://animetosho.org/view/1"},
	))
	ab := abLink.url
	if ab != "" {
		t.Fatalf("ab = %q, want the link routed to a public slot", ab)
	}
	if got := pub.nyaaURL(); got != "" {
		t.Errorf("nyaa_url = %q, want empty for an AnimeTosho destination", got)
	}
	if got := pub.otherURL(); got != "https://animetosho.org/view/1" {
		t.Errorf("public_url = %q, want the AnimeTosho URL", got)
	}
	if got := pub.otherTracker(); got != "AnimeTosho" {
		t.Errorf("public_tracker = %q, want AnimeTosho", got)
	}
}

// TestCapURLAttrPreservesIPv6LiteralHost pins the one Markdown-escaping
// exclusion capURLAttr must keep: square brackets are not CommonMark link
// destination delimiters, but they are required syntax around an IPv6 literal
// host, so percent-encoding them would turn a valid arr deep link into a URL
// browsers and curl reject. The sibling internal/audit escapeLinkURL policy
// leaves them alone for the same reason.
func TestCapURLAttrPreservesIPv6LiteralHost(t *testing.T) {
	const want = "http://[fd00::1]:8989/series/frieren"
	if got := capURLAttr(want); got != want {
		t.Errorf("capURLAttr(%q) = %q, want it unchanged", want, got)
	}
}

// TestTrackerURLsPrefersHeadlineGroupOverNyaaTier pins the headline-first half
// of the public-slot precedence: compare.obtainableLinks ranks the headline
// candidate's own sources first so the rendered link belongs to the group
// Finding.RecommendedGroup names, and a tracker-class-only preference would
// discard that - presenting another recommended group's Nyaa link as the
// action for the headline group.
func TestTrackerURLsPrefersHeadlineGroupOverNyaaTier(t *testing.T) {
	pub, abLink := trackerURLs(gradedLinks(
		compare.ReleaseLink{Tracker: "AnimeTosho", URL: "https://animetosho.org/view/1", Headline: true},
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
	))
	ab := abLink.url
	if ab != "" {
		t.Fatalf("ab = %q, want both links routed to public slots", ab)
	}
	if got := pub.nyaaURL(); got != "" {
		t.Errorf("nyaa_url = %q, want empty (the non-headline Nyaa link must not win)", got)
	}
	if got := pub.otherURL(); got != "https://animetosho.org/view/1" {
		t.Errorf("public_url = %q, want the headline group's AnimeTosho URL", got)
	}
	if got := pub.otherTracker(); got != "AnimeTosho" {
		t.Errorf("public_tracker = %q, want AnimeTosho", got)
	}
}

// TestTrackerURLsPrefersNyaaWithinTheHeadlineTier pins the other half: Nyaa
// still outranks another public tracker when both belong to the headline
// candidate, whatever order the link set arrives in.
func TestTrackerURLsPrefersNyaaWithinTheHeadlineTier(t *testing.T) {
	pub, _ := trackerURLs(gradedLinks(
		compare.ReleaseLink{Tracker: "AnimeTosho", URL: "https://animetosho.org/view/1", Headline: true},
		compare.ReleaseLink{Tracker: "Nyaa", URL: "https://nyaa.si/view/2", Headline: true},
	))
	if got := pub.nyaaURL(); got != "https://nyaa.si/view/2" {
		t.Errorf("nyaa_url = %q, want the headline Nyaa URL", got)
	}
	if got := pub.otherURL(); got != "" {
		t.Errorf("public_url = %q, want empty (Nyaa wins its tier)", got)
	}
}

// TestABSlotTrackerNamesTheRealTracker pins l-f121: the AB slot is filled by a
// fail-closed grade that reads the untrusted SeaDex tracker LABEL first, so it
// can legitimately hold a link whose host belongs to a public tracker. The
// alert must not announce that link as AnimeBytes, so the slot publishes the
// name resolved from the URL's own host, and only an unnameable link falls back
// to AnimeBytes (the slot's own meaning).
func TestABSlotTrackerNamesTheRealTracker(t *testing.T) {
	cases := map[string]struct {
		tracker, url, wantURL, wantTracker string
	}{
		"genuine AnimeBytes link": {
			"AB", "https://animebytes.tv/torrents.php?id=1&torrentid=2",
			"https://animebytes.tv/torrents.php?id=1&torrentid=2", "AnimeBytes",
		},
		"AB label over a Nyaa URL is named Nyaa": {
			"AB", "https://nyaa.si/view/1", "https://nyaa.si/view/1", "Nyaa",
		},
		"AB label over an unclaimed host keeps the label": {
			"AB", "https://example.org/t", "https://example.org/t", "AnimeBytes",
		},
		"unnameable ambiguous link falls back to AnimeBytes": {
			"SomeTracker", "http://[::1", "http://[::1", "AnimeBytes",
		},
		"no AB link leaves both empty": {
			"Nyaa", "https://nyaa.si/view/1", "", "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, abLink := trackerURLs(gradedLinks(compare.ReleaseLink{Tracker: tc.tracker, URL: tc.url}))
			if abLink.url != tc.wantURL {
				t.Errorf("ab_url = %q, want %q", abLink.url, tc.wantURL)
			}
			if got := abLink.abTracker(); got != tc.wantTracker {
				t.Errorf("ab_tracker = %q, want %q", got, tc.wantTracker)
			}
		})
	}
}

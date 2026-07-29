package trackerlink

import (
	"testing"
)

// TestPublish pins the publish-or-drop ladder over the (tracker, rawURL) pair:
// an absolute URL on a canonical tracker host publishes verbatim, a foreign,
// suffix-confused, prefix-confused or homograph host drops, a schemeless
// canonical host is recovered as the mislabeled absolute URL it is, a
// tracker-specific relative shape publishes under its INFERRED owner's base
// rather than the untrusted label's, any other relative value publishes under
// the label's base, and an unknown tracker has no canonical base to vouch for
// either form so it drops.
func TestPublish(t *testing.T) {
	tests := []struct {
		name    string
		tracker string
		url     string
		want    string
	}{
		{name: "blank", tracker: "Nyaa", url: "   ", want: ""},
		{name: "absolute canonical host", tracker: "AB", url: " https://animebytes.tv/torrents.php?id=1&torrentid=2 ", want: "https://animebytes.tv/torrents.php?id=1&torrentid=2"},
		{name: "absolute canonical host case-insensitive", tracker: "Nyaa", url: "https://NYAA.SI/view/1", want: "https://NYAA.SI/view/1"},
		{name: "absolute canonical subdomain", tracker: "Nyaa", url: "https://sukebei.nyaa.si/view/1", want: "https://sukebei.nyaa.si/view/1"},
		{name: "absolute canonical host trailing dot", tracker: "Nyaa", url: "https://nyaa.si./view/1", want: "https://nyaa.si./view/1"},
		{name: "absolute canonical host with valid port kept", tracker: "Nyaa", url: "https://nyaa.si:8080/view/1", want: "https://nyaa.si:8080/view/1"},
		{name: "nyaa-labeled foreign host drops", tracker: "Nyaa", url: "https://evil.example/view/1", want: ""},
		{name: "suffix-confusion host drops", tracker: "Nyaa", url: "https://evilnyaa.si/view/1", want: ""},
		{name: "prefix-confusion host drops", tracker: "Nyaa", url: "https://nyaa.si.evil.example/view/1", want: ""},
		{name: "idn lookalike host drops", tracker: "Nyaa", url: "https://ny\u0430a.si/view/1", want: ""},
		{name: "mislabeled cross-tracker canonical host kept", tracker: "Nyaa", url: "https://animebytes.tv/torrents.php?id=9&torrentid=10", want: "https://animebytes.tv/torrents.php?id=9&torrentid=10"},
		{name: "mislabeled schemeless canonical host recovers", tracker: "Nyaa", url: "animebytes.tv/torrents.php?id=9&torrentid=10", want: "https://animebytes.tv/torrents.php?id=9&torrentid=10"},
		{name: "schemeless canonical host with userinfo never publishes canonicalized", tracker: "Nyaa", url: "user@animebytes.tv/torrents.php?id=9", want: "https://nyaa.si/user@animebytes.tv/torrents.php?id=9"},
		{name: "animebytes relative", tracker: "AB", url: "/torrents.php?id=1", want: "https://animebytes.tv/torrents.php?id=1"},
		{name: "mislabeled AB torrent-page relative canonicalizes to AB base", tracker: "Nyaa", url: "/torrents.php?id=1&torrentid=2", want: "https://animebytes.tv/torrents.php?id=1&torrentid=2"},
		{name: "mislabeled slashless AB torrent-page shape canonicalizes to AB base", tracker: "Nyaa", url: "torrents.php?id=1&torrentid=2", want: "https://animebytes.tv/torrents.php?id=1&torrentid=2"},
		{name: "relative without slash", tracker: "Nyaa", url: "view/1", want: "https://nyaa.si/view/1"},
		{name: "unknown tracker relative drops", tracker: "unknown", url: "/local/path", want: ""},
		{name: "unknown tracker absolute drops", tracker: "unknown", url: "https://example.test/t/9", want: ""},
		{name: "stripped tracker relative drops", tracker: "beyondhd", url: "/torrents/1", want: ""},
		{name: "rutracker relative", tracker: "RuTracker", url: "forum/viewtopic.php?t=1", want: "https://rutracker.org/forum/viewtopic.php?t=1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Publish(tc.tracker, tc.url); got != tc.want {
				t.Errorf("Publish(%q, %q) = %q, want %q", tc.tracker, tc.url, got, tc.want)
			}
		})
	}
}

// TestPublishUnknownTrackerDropsCanonicalAbsoluteURL pins the unknown-label
// gate itself, which TestPublish's unknown-tracker rows cannot: they use a
// non-canonical host (example.test), so they drop on the canonical-host gate
// whether or not the label is resolved first. A CANONICAL absolute URL under an
// unknown label isolates the label gate - the concrete regression where
// absolute-URL handling moves ahead of tracker resolution would publish this.
func TestPublishUnknownTrackerDropsCanonicalAbsoluteURL(t *testing.T) {
	const rawURL = "https://nyaa.si/view/1"
	if got := Publish("unknown", rawURL); got != "" {
		t.Errorf("Publish(%q, %q) = %q, want empty for unknown tracker", "unknown", rawURL, got)
	}
}

// TestPublishRejectsUnsafeSchemes pins the unsafe-scheme and
// malformed-URL gate on the untrusted upstream URL: javascript:, data:, and
// file: values must never be converted into clickable tracker links, and a
// malformed or anomalous value (hostless, unparseable escape, whitespace in
// the host, backslash authority, a tab/newline-smuggled form the WHATWG
// preprocessing de-smuggled, a hidden-host quirk form) must drop to the
// empty-URL case rather than be published as a link a human cannot follow -
// publish-or-drop rejects what it cannot vouch for even when the classifier
// recovered the evidence.
func TestPublishRejectsUnsafeSchemes(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "javascript", url: "javascript:alert(1)"},
		{name: "data", url: "data:text/html,<script>alert(1)</script>"},
		{name: "file", url: "file:///etc/passwd"},
		{name: "hostless https", url: "https://"},
		{name: "port-only authority", url: "https://:443/path"},
		{name: "invalid escape", url: "https://example.test/%zz"},
		{name: "whitespace in host", url: "https://bad host/path"},
		{name: "backslash authority", url: `\\evil.example/path`},
		{name: "tab-smuggled canonical host", url: "https://nyaa\t.si/view/1"},
		{name: "newline-smuggled scheme", url: "ht\ntps://nyaa.si/view/1"},
		{name: "hidden-host single-slash form (evidence recovered, still unvouchable)", url: "https:/animebytes.tv/torrents.php?id=1"},
		{name: "userinfo authority confusion", url: "https://animebytes.tv@evil.example/torrent"},
		{name: "query-only with colon", url: "?x:y"},
		{name: "fragment-only with colon", url: "#a:b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Publish("Nyaa", tc.url)
			if got != "" {
				t.Errorf("Publish(%q) = %q, want empty for unsafe scheme", tc.url, got)
			}
		})
	}
}

// TestPublishRelativeShapeFloor pins the relative arm's shape floor (l-f88)
// through the PUBLIC publisher rather than the unexported helper, so a
// behavior-preserving rename, inline, or decomposition of that helper cannot
// break the suite while Publish keeps returning identical links. It keeps only
// the distinct externally observable cases: a colon safely inside a later path
// segment publishes, and a structureless token drops instead of publishing a
// plausible-looking 404 (the live catalogue carries exactly one such record -
// AB, url "Chihiro", a release-group name typed into the url field). A value
// that is ONLY a query or fragment has no path segment at all, so publishing it
// would emit the tracker root ("https://nyaa.si/?id=1"), which the floor
// refuses; and a delimiter-only tail ("/view?", "/view#") carries no
// identifying content, so it resolves to the same page as the bare
// single-segment path and drops with it (h-f30). The colon-before-slash and
// leading-slash-normalization rows live in
// TestPublishRejectsUnsafeSchemes and TestPublish.
func TestPublishRelativeShapeFloor(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want string
	}{
		"colon after slash publishes":           {raw: "path/a:b", want: "https://nyaa.si/path/a:b"},
		"bare single-segment token drops":       {raw: "view", want: ""},
		"rooted single-segment token drops":     {raw: "/Chihiro", want: ""},
		"single-segment query publishes":        {raw: "/view?id=1", want: "https://nyaa.si/view?id=1"},
		"single-segment fragment publishes":     {raw: "/view#1", want: "https://nyaa.si/view#1"},
		"root alone drops":                      {raw: "/", want: ""},
		"query without a path segment drops":    {raw: "?id=1", want: ""},
		"fragment without a path segment drops": {raw: "#1167293", want: ""},
		"delimiter-only query drops":            {raw: "/view?", want: ""},
		"delimiter-only fragment drops":         {raw: "/view#", want: ""},
		"delimiter-only pair drops":             {raw: "/view?#", want: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Publish("Nyaa", tc.raw); got != tc.want {
				t.Errorf("Publish(%q, %q) = %q, want %q", "Nyaa", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPublishPortBoundaries pins the publisher's shared port rule
// (portOK) at the 16-bit boundary through the PUBLIC publisher, on real raw
// upstream values rather than a fabricated classifier result: the maximum port
// publishes, a port above the 16-bit range drops, port zero drops (it parses
// as a uint16 but names no destination port), and a non-numeric port
// drops (net/url cannot read the authority at all). TestPublish continues to
// cover schemeless-host recovery and the labeled-relative fallback.
func TestPublishPortBoundaries(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "maximum port is published", url: "https://nyaa.si:65535/view/1", want: "https://nyaa.si:65535/view/1"},
		{name: "port above maximum drops", url: "https://nyaa.si:65536/view/1", want: ""},
		{name: "nonnumeric port drops", url: "https://nyaa.si:abc/view/1", want: ""},
		{name: "port zero drops", url: "https://nyaa.si:0/view/1", want: ""},
		{name: "padded port zero drops", url: "https://nyaa.si:00/view/1", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Publish("Nyaa", tc.url)
			if got != tc.want {
				t.Errorf("Publish(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestPublishCanonicalizesScheme pins the no-cleartext-publish rule
// (l-f89). Every canonical tracker base in the internal/tracker table is https, and
// the schemeless publish branch already prefixes "https://" for that reason -
// but the ABSOLUTE branch emitted the upstream's scheme verbatim, so a tampered
// SeaDex record could publish "http://nyaa.si/view/1" as the clickable release
// link. Neither tracker host is HSTS-preloaded, so that first hop is genuinely
// cleartext and an on-path attacker can answer it with a phishing page under
// the tracker's own URL bar (AnimeBytes is login-bearing). The host is already
// proven canonical by the time the scheme is read, so the link is upgraded
// rather than dropped; everything after the scheme survives byte-for-byte.
func TestPublishCanonicalizesScheme(t *testing.T) {
	tests := map[string]struct {
		tracker string
		url     string
		want    string
	}{
		"cleartext nyaa is upgraded": {
			tracker: "Nyaa", url: "http://nyaa.si/view/1", want: "https://nyaa.si/view/1",
		},
		"mixed-case cleartext scheme is upgraded": {
			tracker: "Nyaa", url: "HtTp://nyaa.si/view/1", want: "https://nyaa.si/view/1",
		},
		"path, query and case after the scheme survive": {
			tracker: "AB",
			url:     "http://animebytes.tv/torrents.php?id=1&torrentid=456#Frag",
			want:    "https://animebytes.tv/torrents.php?id=1&torrentid=456#Frag",
		},
		"https is unchanged": {
			tracker: "Nyaa", url: "https://nyaa.si/view/1", want: "https://nyaa.si/view/1",
		},
		"a non-tracker host is still dropped, not upgraded": {
			tracker: "Nyaa", url: "http://evil.example/view/1", want: "",
		},
		// The upgrade rewrites the scheme only, so a cleartext URL naming the
		// http service's port would publish an https link to a plaintext port.
		// 443 is the one port that stays coherent; every other explicit port
		// drops, so the caller reports a URL error instead of a dead link.
		"cleartext on the http port drops": {
			tracker: "Nyaa", url: "http://nyaa.si:80/view/1", want: "",
		},
		"cleartext on an alternate port drops": {
			tracker: "Nyaa", url: "http://nyaa.si:8080/view/1", want: "",
		},
		"cleartext already naming the https port is upgraded": {
			tracker: "Nyaa", url: "http://nyaa.si:443/view/1", want: "https://nyaa.si:443/view/1",
		},
		"an https URL keeps its explicit port": {
			tracker: "Nyaa", url: "https://nyaa.si:8080/view/1", want: "https://nyaa.si:8080/view/1",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Publish(tc.tracker, tc.url); got != tc.want {
				t.Errorf("Publish(%q, %q) = %q, want %q", tc.tracker, tc.url, got, tc.want)
			}
		})
	}
}

// TestPublishRequiresATargetBeyondTheHost pins the host-form shape floor
// (hostFormTargeted): a value that carries only a canonical tracker host
// resolves to the front page, which identifies no torrent, so it drops like
// every other unvouchable form - and drops rather than publishes so the caller
// reports it as a URL error instead of a plausible-looking 404. Both
// host-bearing arms are covered: the absolute branch and the canonicalized
// schemeless-host branch. A tail made only of further delimiters names no
// target either, while a genuinely targeted root query still publishes. A
// fragment-only tail is NOT a target (it resolves client-side, leaving the
// browser on the front page), matching the relative twin pathShaped.
func TestPublishRequiresATargetBeyondTheHost(t *testing.T) {
	tests := map[string]struct{ tracker, url, want string }{
		"bare schemeless host drops":               {"Nyaa", "nyaa.si", ""},
		"schemeless host with a root slash drops":  {"Nyaa", "nyaa.si/", ""},
		"bare absolute host drops":                 {"Nyaa", "https://nyaa.si", ""},
		"absolute host with a root slash drops":    {"Nyaa", "https://nyaa.si/", ""},
		"bare AB host drops":                       {"AB", "animebytes.tv", ""},
		"a subdomain tracker host is no exception": {"Nyaa", "sukebei.nyaa.si", ""},
		"a delimiter-only query tail drops":        {"Nyaa", "nyaa.si/?", ""},
		"a delimiter-only fragment tail drops":     {"Nyaa", "nyaa.si/#", ""},
		"a doubled root slash drops":               {"Nyaa", "nyaa.si//", ""},
		"an absolute delimiter-only tail drops":    {"Nyaa", "https://nyaa.si/?", ""},
		"a dot-segment-only tail drops":            {"Nyaa", "nyaa.si/.", ""},
		"a double-dot-only tail drops":             {"Nyaa", "nyaa.si/..", ""},
		"an absolute dot-segment tail drops":       {"Nyaa", "https://nyaa.si/.", ""},
		"an encoded dot segment drops":             {"Nyaa", "https://nyaa.si/%2e", ""},
		"an encoded double-dot segment drops":      {"Nyaa", "https://nyaa.si/%2e%2e/", ""},
		"a dot segment before a target publishes":  {"Nyaa", "https://nyaa.si/../view/1", "https://nyaa.si/../view/1"},
		"a targeted root query still publishes":    {"Nyaa", "nyaa.si/?page=view&tid=1", "https://nyaa.si/?page=view&tid=1"},
		"a fragment-only root tail drops":          {"Nyaa", "nyaa.si/#1167293", ""},
		"an absolute fragment-only tail drops":     {"Nyaa", "https://nyaa.si/#1167293", ""},
		"a pathless fragment-only tail drops":      {"Nyaa", "https://nyaa.si#1167293", ""},
		"a fragment cannot mask a dot-only path":   {"Nyaa", "nyaa.si/.#x", ""},
		"a fragment on a real path publishes":      {"Nyaa", "nyaa.si/view/1#Frag", "https://nyaa.si/view/1#Frag"},
		"a real torrent path still publishes":      {"Nyaa", "nyaa.si/view/1", "https://nyaa.si/view/1"},
		"an absolute torrent path still publishes": {"Nyaa", "https://nyaa.si/view/1", "https://nyaa.si/view/1"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Publish(tc.tracker, tc.url); got != tc.want {
				t.Errorf("Publish(%q, %q) = %q, want %q", tc.tracker, tc.url, got, tc.want)
			}
		})
	}
}

// TestPublishReasonGrades pins the refusal REASON the diagnostic consumers read
// (l-f127): the empty string alone cannot distinguish a tracker this build does
// not carry - whose remedy is a seadex-scout table entry - from an unvouchable
// url, whose remedy is fixing the SeaDex record, and the audit row marker plus
// the SeaDex client's catalogue WARN each name one of those remedies. It also
// pins the invariant both consumers rest on: a non-empty link always grades
// RefusalNone, and every refusal grade yields an empty link.
func TestPublishReasonGrades(t *testing.T) {
	tests := map[string]struct {
		tracker  string
		url      string
		wantLink bool
		want     Refusal
	}{
		"a canonical absolute link publishes":        {"Nyaa", "https://nyaa.si/view/1", true, RefusalNone},
		"an AB relative shape publishes":             {"AB", "/torrents.php?id=1&torrentid=2", true, RefusalNone},
		"an omitted url is no url, not an error":     {"Nyaa", "", false, RefusalNoURL},
		"a whitespace-only url is no url":            {"Nyaa", "   ", false, RefusalNoURL},
		"an unknown tracker is an app-table gap":     {"beyondhd", "https://beyondhd.co/t/1", false, RefusalUnknownTracker},
		"an unknown tracker with no url is no url":   {"beyondhd", "", false, RefusalNoURL},
		"a foreign host is an unvouchable url":       {"Nyaa", "https://evil.example/view/1", false, RefusalUnvouchableURL},
		"an unsafe scheme is an unvouchable url":     {"Nyaa", "javascript:alert(1)", false, RefusalUnvouchableURL},
		"a smuggled url is an unvouchable url":       {"Nyaa", "https://nyaa.si/view\\1", false, RefusalUnvouchableURL},
		"a protocol-relative url is unvouchable":     {"Nyaa", "//evil.example/x", false, RefusalUnvouchableURL},
		"a structureless token is unvouchable":       {"AB", "Chihiro", false, RefusalUnvouchableURL},
		"a tracker front page is unvouchable":        {"Nyaa", "https://nyaa.si/", false, RefusalUnvouchableURL},
		"a query-leading colon is unvouchable":       {"Nyaa", "?x:y", false, RefusalUnvouchableURL},
		"an unknown tracker beats a bad url shape":   {"beyondhd", "Chihiro", false, RefusalUnknownTracker},
		"a userinfo authority is an unvouchable url": {"Nyaa", "https://trusted@evil.example/x", false, RefusalUnvouchableURL},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			link, refusal := PublishReason(tc.tracker, tc.url)
			if (link != "") != tc.wantLink {
				t.Errorf("PublishReason(%q, %q) link = %q, want non-empty = %v", tc.tracker, tc.url, link, tc.wantLink)
			}
			if refusal != tc.want {
				t.Errorf("PublishReason(%q, %q) refusal = %d, want %d", tc.tracker, tc.url, refusal, tc.want)
			}
			// Structural invariant: link presence and RefusalNone are the same
			// fact, so no consumer can read one and act on the other.
			if (link != "") != (refusal == RefusalNone) {
				t.Errorf("PublishReason(%q, %q) = %q/%d: a link must mean RefusalNone and a refusal must mean no link", tc.tracker, tc.url, link, refusal)
			}
			// The link-only form stays exactly the reasoned form's string, so
			// the five link-only call sites cannot drift from the diagnostics.
			if got := Publish(tc.tracker, tc.url); got != link {
				t.Errorf("Publish(%q, %q) = %q but PublishReason gave %q", tc.tracker, tc.url, got, link)
			}
		})
	}
}

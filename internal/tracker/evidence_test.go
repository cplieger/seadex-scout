package tracker

import "testing"

// TestClassifyAB pins the grade of every AnimeBytes-evidence shape, replacing
// the two boolean tables (fail-closed gate, fail-open predicate) this one table
// now covers in a single pass. The three grades carry the two fail directions
// the app needs: ABNone surfaces with the toggle off, ABDefinite is the audit
// report's row-listing gate, and ABAmbiguous is the band where the two
// directions disagree - hidden by ABVisible, still LISTED by the report.
//
// The subset invariant the old tables cross-checked (definite implies gated) is
// now structural: one value cannot be definite without also being non-None, so
// it is asserted once through filter.ABVisible's exhaustive reading
// (internal/filter's TestABVisibleReadsEveryGrade) rather than restated per row.
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

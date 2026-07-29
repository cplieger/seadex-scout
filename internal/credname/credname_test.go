package credname

import "testing"

// TestContainsWordCoversEveryName pins the containment relation the two consumer
// policies depend on: every canonical name the operator warning matches exactly
// is ALSO matched by the broad redaction predicate. Before both rules shared this
// package the relation held only by coincidence of word choice, and the
// asymmetric consequence sat on the redaction side (a credential written to a log
// line, CWE-532). Adding a name with no matching word stem now fails here.
func TestContainsWordCoversEveryName(t *testing.T) {
	for name := range names {
		if !ContainsWord(name) {
			t.Errorf("ContainsWord(%q) = false: a canonical credential name is not covered by the broad predicate", name)
		}
		if !IsName(name) {
			t.Errorf("IsName(%q) = false for a canonical name", name)
		}
	}
}

func TestIsName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"apikey", true},
		{"API_KEY", true},
		{"torrent_pass", true},
		{"rss_key", true},
		// Exact match only: a compound spelling is the broad policy's job.
		{"prowlarr_apikey", false},
		{"mode", false},
		{"", false},
	} {
		if got := IsName(tc.name); got != tc.want {
			t.Errorf("IsName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestContainsWord(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"apikey", true},
		{"X-Api-Key", true},
		{"prowlarr_apikey", true},
		{"rsskey", true},
		{"credentials", true},
		{"mode", false},
		{"indexer", false},
		{"", false},
	} {
		if got := ContainsWord(tc.name); got != tc.want {
			t.Errorf("ContainsWord(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

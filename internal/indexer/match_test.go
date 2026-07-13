package indexer

import "testing"

// TestTrackerScope pins the two documented contracts of trackerScope that the
// other indexer tests only exercise indirectly: the defensive "animebytes"
// alias for the "AB" spelling, and the tail-drop default (any unknown tracker
// maps to "") that makes feedItemFor/downloadURL exclude those releases from
// the synthesized feed. Nyaa/AB spellings are normalized case- and
// whitespace-insensitively.
func TestTrackerScope(t *testing.T) {
	tests := []struct{ tracker, want string }{
		{"Nyaa", upstreamNyaa},
		{"nyaa", upstreamNyaa},
		{"  Nyaa  ", upstreamNyaa},
		{"AB", upstreamAB},
		{"ab", upstreamAB},
		{"animebytes", upstreamAB},
		{"AnimeBytes", upstreamAB},
		{"AnimeTosho", ""},
		{"RuTracker", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := trackerScope(tc.tracker); got != tc.want {
			t.Errorf("trackerScope(%q) = %q, want %q", tc.tracker, got, tc.want)
		}
	}
}

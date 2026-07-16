package indexer

import (
	"testing"
)

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

// TestTrackerKeyRejectsUnknownAndUnparseable pins the empty-key fallbacks the
// happy-path tests skip: an unknown tracker and a tracker URL without its id
// both yield no key (the release simply cannot be matched), and an unparseable
// URL yields no key from the Prowlarr-side extractor rather than an error or a
// bogus match.
func TestTrackerKeyRejectsUnknownAndUnparseable(t *testing.T) {
	if got := trackerKey("AnimeTosho", "https://animetosho.org/view/123"); got != "" {
		t.Errorf("unknown tracker key = %q, want empty", got)
	}
	if got := trackerKey("Nyaa", "https://nyaa.si/about"); got != "" {
		t.Errorf("nyaa URL without an id key = %q, want empty", got)
	}
	if got := trackerKey("AB", "/torrents.php?id=1"); got != "" {
		t.Errorf("ab URL without a torrentid key = %q, want empty", got)
	}
	if got := trackerKeyFromURL("http://nyaa.si/view/1%zz"); got != "" {
		t.Errorf("unparseable URL key = %q, want empty", got)
	}
	if got := trackerKey("Nyaa", "http://nyaa.si/view/1%zz"); got != "" {
		t.Errorf("nyaa unparseable URL key = %q, want empty", got)
	}
	if got := trackerKey("AB", "http://animebytes.tv/torrent/1%zz"); got != "" {
		t.Errorf("ab unparseable URL key = %q, want empty", got)
	}
}

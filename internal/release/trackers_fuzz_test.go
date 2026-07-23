package release

import (
	"strings"
	"testing"

	"github.com/cplieger/urlform"
)

// FuzzLookupTrackerByRelativeURL fuzzes the structural relative-URL tracker
// resolver over arbitrary untrusted URL strings (SeaDex-published torrent
// URLs) with bounded-output and cross-function invariants, never a
// reimplementation of the shape rule: a match is always exactly the canonical
// AnimeBytes table entry (the resolver can never mint a tracker the table
// does not carry); a match implies urlform classified the input as a rooted
// relative path (so an absolute, protocol-relative, or schemeless-host input
// can never resolve - tracker identity from those forms must come from the
// host gate); and prefixing a scheme+host onto any matching input never
// creates a match (the relative-shape rule cannot be bypassed by embedding
// the path in an absolute URL).
func FuzzLookupTrackerByRelativeURL(f *testing.F) {
	f.Add("/torrents.php?id=12345&torrentid=1167293")
	f.Add("/torrents.php?torrentid=1")
	f.Add("/TORRENTS.PHP?torrentid=1")
	f.Add("/torrents.php?id=12345")
	f.Add("/view/1918784")
	f.Add("https://animebytes.tv/torrents.php?torrentid=1")
	f.Add("//animebytes.tv/torrents.php?torrentid=1")
	f.Add("torrents.php?torrentid=1")
	f.Add("")
	f.Add("/torrents.php?%gg=1&torrentid=1")
	f.Fuzz(func(t *testing.T, raw string) {
		got, ok := LookupTrackerByRelativeURL(raw)
		if !ok {
			if got.Name != "" {
				t.Errorf("LookupTrackerByRelativeURL(%q) = %+v with ok=false, want the zero Tracker", raw, got)
			}
			return
		}
		if got.Name != TrackerNameAnimeBytes {
			t.Errorf("LookupTrackerByRelativeURL(%q) = %q, want only %q can match", raw, got.Name, TrackerNameAnimeBytes)
		}
		canonical, tableOK := LookupTracker(TrackerNameAnimeBytes)
		if !tableOK || got.Name != canonical.Name || got.Type != canonical.Type || got.BaseURL != canonical.BaseURL {
			t.Errorf("LookupTrackerByRelativeURL(%q) = %+v, want the canonical table entry %+v", raw, got, canonical)
		}
		if f := urlform.Classify(raw); f.Class != urlform.ClassRelative {
			t.Errorf("LookupTrackerByRelativeURL(%q) matched but urlform classifies it %v, not ClassRelative", raw, f.Class)
		}
		if abs := "https://evil.example" + strings.TrimSpace(raw); func() bool { _, ok := LookupTrackerByRelativeURL(abs); return ok }() {
			t.Errorf("LookupTrackerByRelativeURL(%q) = true for the absolutized form of a matching relative URL: shape rule bypassed", abs)
		}
	})
}

package seadex

import (
	"strings"
	"testing"
)

// FuzzTorrentUsableURL fuzzes the unsafe-scheme gate over the untrusted
// upstream (SeaDex-supplied) torrent URL and tracker name. Invariants: a
// protocol-relative URL (//host/...) is always rejected (empty result); a
// non-empty result never carries a non-http(s) scheme (javascript:, data:,
// file: must never become clickable links in findings/reports/feeds); an
// absolute http(s) input is returned unchanged apart from trimming; and the
// function is idempotent (re-running on its own output is a fixed point, so a
// link already made usable is never re-mangled).
func FuzzTorrentUsableURL(f *testing.F) {
	f.Add("https://nyaa.si/view/1", "Nyaa")
	f.Add("/torrents.php?id=1&torrentid=2", "AB")
	f.Add("javascript:alert(1)", "Nyaa")
	f.Add("javascript:alert(1)", "")
	f.Add("data:text/html,x", "sometracker")
	f.Add("file:///etc/passwd", "")
	f.Add("https://trusted@evil.example/x", "Nyaa")
	f.Add("view/1", "nyaa")
	f.Add("  HTTPS://Example.test/T/1  ", "unknown")
	f.Add("", "")
	f.Add("//evil.example/x", "unknowntracker")
	f.Add("a/b:c", "unknowntracker")
	f.Add("?x:y", "Nyaa")
	f.Add("#a:b", "")
	f.Fuzz(func(t *testing.T, rawURL, tracker string) {
		tor := &Torrent{URL: rawURL, Tracker: tracker}
		out := tor.UsableURL()
		trimmed := strings.TrimSpace(rawURL)
		// Security invariant: a protocol-relative URL is never usable (it
		// would resolve against whatever page hosts the link).
		if strings.HasPrefix(trimmed, "//") {
			if out != "" {
				t.Errorf("UsableURL(%q, tracker %q) = %q, want empty for protocol-relative URL", rawURL, tracker, out)
			}
			return
		}
		if out == "" {
			return
		}
		lower := strings.ToLower(out)
		// Security invariant: any scheme on the output must be http or https.
		if i := strings.Index(out, ":"); i >= 0 && !strings.Contains(out[:i], "/") {
			if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
				t.Errorf("UsableURL(%q, tracker %q) = %q carries a non-http(s) scheme", rawURL, tracker, out)
			}
		}
		// Passthrough: an absolute http(s) input comes back unchanged (trimmed).
		tl := strings.ToLower(trimmed)
		if strings.HasPrefix(tl, "http://") || strings.HasPrefix(tl, "https://") {
			if out != trimmed {
				t.Errorf("UsableURL(%q) = %q, want the absolute URL back unchanged (%q)", rawURL, out, trimmed)
			}
		}
		// Idempotence: the output is a fixed point for the same tracker.
		again := (&Torrent{URL: out, Tracker: tracker}).UsableURL()
		if again != out {
			t.Errorf("UsableURL not idempotent for tracker %q: %q -> %q -> %q", tracker, rawURL, out, again)
		}
	})
}

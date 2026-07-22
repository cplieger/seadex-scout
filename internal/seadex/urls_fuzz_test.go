package seadex

import (
	"net/url"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
)

// FuzzTorrentUsableURL fuzzes the unsafe-scheme and host-binding gate over the
// untrusted upstream (SeaDex-supplied) torrent URL and tracker name.
// Invariants: a protocol-relative URL (//host/...) is always rejected (empty
// result); a non-empty result never carries a non-http(s) scheme (javascript:,
// data:, file: must never become clickable links in findings/reports/feeds);
// a non-empty result's host is always bound to a canonical tracker host from
// the release tracker table (so a regression that drops the
// release.LookupTrackerByHost gate cannot let https://evil.example/x or a
// suffix-confusion host through); an absolute http(s) input that survives the
// tracker host-binding gate is returned unchanged apart from trimming; and
// the function is idempotent (re-running on its own output is a fixed point,
// so a link already made usable is never re-mangled).
func FuzzTorrentUsableURL(f *testing.F) {
	f.Add("https://nyaa.si/view/1", "Nyaa")
	f.Add("/torrents.php?id=1&torrentid=2", "AB")
	f.Add("javascript:alert(1)", "Nyaa")
	f.Add("javascript:alert(1)", "")
	f.Add("data:text/html,x", "sometracker")
	f.Add("file:///etc/passwd", "")
	f.Add("https://trusted@evil.example/x", "Nyaa")
	f.Add("https://evil.example/x", "Nyaa")
	f.Add("https://sukebei.nyaa.si/view/1", "Nyaa")
	f.Add("https://nyaa.si./view/1", "Nyaa")
	f.Add("https://nyaa.si.evil.example/view/1", "Nyaa")
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
		parsed, err := url.Parse(out)
		if err != nil {
			t.Errorf("UsableURL(%q, tracker %q) = %q, not parseable: %v", rawURL, tracker, out, err)
			return
		}
		// Security invariant: a usable link never retains a userinfo
		// authority (https://trusted@evil.example/x must be rejected).
		if parsed.User != nil {
			t.Errorf("UsableURL(%q, tracker %q) = %q retains userinfo authority", rawURL, tracker, out)
		}
		// Security invariant: a usable link's host is always a canonical
		// tracker host (or a real dot-delimited subdomain of one) - the
		// host-binding gate a compromised SeaDex response must not bypass.
		if _, ok := release.LookupTrackerByHost(parsed.Hostname()); !ok {
			t.Errorf("UsableURL(%q, tracker %q) = %q has non-canonical host %q", rawURL, tracker, out, parsed.Hostname())
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

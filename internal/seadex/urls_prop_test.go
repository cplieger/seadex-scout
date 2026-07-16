package seadex

import (
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestUsableURLSafeOutputProperty is the every-PR property companion to
// FuzzTorrentUsableURL (whose coverage-guided exploration only runs in the
// weekly fuzz job): for ANY input URL and tracker, a non-empty UsableURL
// result parses as an absolute http(s) URL with a non-empty host and no
// userinfo (the link-safety gate: no javascript:/data:/file:, no
// protocol-relative form, no bare path, no credential-bearing authority),
// and the result is a fixed point (feeding a usable link back
// in returns it unchanged, so an already-usable link is never re-mangled).
func TestUsableURLSafeOutputProperty(t *testing.T) {
	trackers := []string{"Nyaa", "AB", "AnimeTosho", "RuTracker", "unknown", ""}
	prefixes := []string{"", "//", "/", "  ", "javascript:", "data:", "file://", "https://", "http://", "HTTPS://", ":"}
	rapid.Check(t, func(rt *rapid.T) {
		raw := rapid.SampledFrom(prefixes).Draw(rt, "prefix") + rapid.String().Draw(rt, "rest")
		tracker := rapid.SampledFrom(trackers).Draw(rt, "tracker")
		out := (&Torrent{URL: raw, Tracker: tracker}).UsableURL()
		if out == "" {
			return
		}
		parsed, err := url.Parse(out)
		if err != nil {
			rt.Fatalf("UsableURL(%q, tracker %q) = %q, not parseable: %v", raw, tracker, out, err)
		}
		if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
			rt.Fatalf("UsableURL(%q, tracker %q) = %q, scheme %q is not http(s)", raw, tracker, out, parsed.Scheme)
		}
		if parsed.Host == "" {
			rt.Fatalf("UsableURL(%q, tracker %q) = %q has no host", raw, tracker, out)
		}
		if parsed.User != nil {
			rt.Fatalf("UsableURL(%q, tracker %q) = %q retains userinfo authority", raw, tracker, out)
		}
		if again := (&Torrent{URL: out, Tracker: tracker}).UsableURL(); again != out {
			rt.Fatalf("UsableURL not a fixed point for tracker %q: %q -> %q -> %q", tracker, raw, out, again)
		}
	})
}

package trackerlink

import (
	"net/url"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/tracker"
	"pgregory.net/rapid"
)

// TestPublishSafeOutputProperty is the every-PR property companion to FuzzPublish, whose
// coverage-guided exploration only runs in the weekly fuzz job; it holds the same
// link-safety, host-binding and fixed-point invariants over drawn inputs. The host
// component is DRAWN from canonical and near-canonical tracker hosts: a random rest never
// spells a tracker host, so without it every absolute draw dies on the host-binding gate
// and the assertions never run on the absolute arm at all.
func TestPublishSafeOutputProperty(t *testing.T) {
	trackers := []string{"Nyaa", "AB", "AnimeTosho", "RuTracker", "unknown", ""}
	prefixes := []string{"", "//", "/", "  ", "javascript:", "data:", "file://", "https://", "http://", "HTTPS://", ":"}
	hosts := []string{
		"", "nyaa.si", "NYAA.SI", "sukebei.nyaa.si", "nyaa.si.", "animebytes.tv",
		"rutracker.org", "evil.example", "nyaa.si.evil.example", "evilnyaa.si", "ny\u0430a.si",
	}
	rapid.Check(t, func(rt *rapid.T) {
		raw := rapid.SampledFrom(prefixes).Draw(rt, "prefix") +
			rapid.SampledFrom(hosts).Draw(rt, "host") +
			rapid.String().Draw(rt, "rest")
		trackerName := rapid.SampledFrom(trackers).Draw(rt, "tracker")
		out := Publish(trackerName, raw)
		if out == "" {
			return
		}
		parsed, err := url.Parse(out)
		if err != nil {
			rt.Fatalf("Publish(%q, tracker %q) = %q, not parseable: %v", raw, trackerName, out, err)
		}
		if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
			rt.Fatalf("Publish(%q, tracker %q) = %q, scheme %q is not http(s)", raw, trackerName, out, parsed.Scheme)
		}
		if parsed.Host == "" {
			rt.Fatalf("Publish(%q, tracker %q) = %q has no host", raw, trackerName, out)
		}
		if parsed.User != nil {
			rt.Fatalf("Publish(%q, tracker %q) = %q retains userinfo authority", raw, trackerName, out)
		}
		if _, ok := tracker.LookupByHost(parsed.Hostname()); !ok {
			rt.Fatalf("Publish(%q, tracker %q) = %q has non-canonical host %q", raw, trackerName, out, parsed.Hostname())
		}
		if again := Publish(trackerName, out); again != out {
			rt.Fatalf("Publish not a fixed point for tracker %q: %q -> %q -> %q", trackerName, raw, out, again)
		}
	})
}

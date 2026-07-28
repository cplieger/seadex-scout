package indexer

import (
	"net/url"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestSnapshotInfoURLAllowedProperty is the every-PR randomized complement to
// FuzzSnapshotInfoURL: snapshotInfoURLAllowed is a publish gate over tamperable
// feed.json data, and the committed fuzz seeds are the only generated cases a
// PR runs. It generates scheme / host / userinfo / path combinations against
// the documented acceptance model (absolute http(s), no userinfo, canonical
// host under an ASCII fold) and independently re-verifies every accepted URL
// with net/url plus a parse/re-render round trip.
func TestSnapshotInfoURLAllowedProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		scheme := rapid.SampledFrom([]string{"http", "https", "ftp", "javascript"}).Draw(t, "scheme")
		host := rapid.SampledFrom([]string{
			seadexInfoHost(),
			strings.ToUpper(seadexInfoHost()),
			"evil.example",
			seadexInfoHost() + ".evil.example",
			"",
		}).Draw(t, "host")
		userinfo := rapid.SampledFrom([]string{"", "user@", "user:pass@"}).Draw(t, "userinfo")
		path := rapid.StringMatching(`[A-Za-z0-9/_-]{0,32}`).Draw(t, "path")
		raw := scheme + "://" + userinfo + host + "/" + path

		want := (scheme == "http" || scheme == "https") &&
			userinfo == "" && host != "" &&
			strings.EqualFold(host, seadexInfoHost())
		got := snapshotInfoURLAllowed(raw, seadexInfoHost())
		if got != want {
			t.Fatalf("snapshotInfoURLAllowed(%q) = %v, want %v", raw, got, want)
		}
		if !got {
			return
		}

		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("accepted URL %q does not parse with net/url: %v", raw, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("accepted URL %q has scheme %q", raw, u.Scheme)
		}
		if u.User != nil {
			t.Fatalf("accepted URL %q carries userinfo %q", raw, u.User)
		}
		if gotHost := strings.ToLower(u.Hostname()); gotHost != seadexInfoHost() {
			t.Fatalf("accepted URL %q resolves to host %q, want %q", raw, gotHost, seadexInfoHost())
		}
		if !snapshotInfoURLAllowed(u.String(), seadexInfoHost()) {
			t.Fatalf("accepted URL %q is rejected after net/url round trip as %q", raw, u.String())
		}
	})
}

package indexer

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzSnapshotInfoURLAllowed_failsClosedAndKeepsCanonicalLinks exercises the
// persisted-InfoURL publish gate - the load-boundary twin of the search path's
// sanitizeDisplayURL - with arbitrary tampered feed.json values. Three invariants: an
// UNRESOLVED canonical host vouches nothing (a hostless "https:///1" must not slip
// through the empty-host comparison); any accepted URL resolves to the canonical SeaDex
// host over http(s) with no userinfo under an INDEPENDENT parser (net/url); and it stays
// accepted after that consumer's parse/re-render round trip. The independent parse reads
// the VOUCHED spelling the gate returns, which is what sanitizeSnapshotInfoURLs stores.
func FuzzSnapshotInfoURLAllowed_failsClosedAndKeepsCanonicalLinks(f *testing.F) {
	f.Add("https://releases.moe/154587")
	f.Add("http://releases.moe/154587")
	f.Add("https://RELEASES.MOE/154587")
	f.Add("https:///154587")
	f.Add("//releases.moe/154587")
	f.Add("javascript:alert(1)")
	f.Add("https://evil@releases.moe/154587")
	f.Add("https://releases.moe@evil.example/phish")
	f.Add("https://releases%2emoe/154587")
	f.Add("https://releases.moe:8443/154587")
	f.Add("https://evil.example/154587")
	f.Add("https://releases.moe.evil.example/154587")
	f.Add("\thttps://releases.moe/154587")
	f.Add("https://releases.moe/154587\x01")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		if _, ok := snapshotInfoURLAllowed(raw, ""); ok {
			t.Fatalf("snapshotInfoURLAllowed(%q, \"\") = true; an unresolved canonical host must vouch nothing", raw)
		}
		host := seadexInfoHost()
		cleaned, ok := snapshotInfoURLAllowed(raw, host)
		if !ok {
			return
		}
		u, err := url.Parse(cleaned)
		if err != nil {
			t.Fatalf("vouched form %q of accepted URL %q does not parse with net/url: %v; the gate must never vouch a value its own parser of record cannot resolve", cleaned, raw, err)
		}
		if got := strings.ToLower(u.Hostname()); got != host {
			t.Errorf("accepted URL %q (vouched as %q) resolves to host %q under net/url, want the canonical %q; the gate must never vouch a link a consumer sends elsewhere", raw, cleaned, got, host)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("accepted URL %q has scheme %q; only an http(s) link may be published to the arr UI", raw, u.Scheme)
		}
		if u.User != nil {
			t.Fatalf("accepted URL %q carries userinfo %q; a spoofable authority must never be published", raw, u.User)
		}
		if _, ok := snapshotInfoURLAllowed(u.String(), host); !ok {
			t.Fatalf("accepted URL %q is rejected after its consumer's own re-render (%q)", raw, u.String())
		}
	})
}

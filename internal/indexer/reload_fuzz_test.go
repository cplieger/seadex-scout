package indexer

import (
	"net/url"
	"testing"
)

// FuzzSnapshotInfoURLAllowed_failsClosedAndKeepsCanonicalLinks exercises the
// persisted-InfoURL publish gate - the load-boundary twin of the search path's
// sanitizeDisplayURL, which already carries a fuzz target - with arbitrary
// tampered feed.json values. Two invariants: an UNRESOLVED canonical host must
// vouch nothing (a hostless "https:///1" must not slip through the empty-host
// comparison, which is what the host == "" guard exists to stop), and any URL
// the gate accepts must stay accepted after its consumer's own parse/re-render
// round trip, so a vouched link cannot change identity between the gate and the
// arr UI that renders it as <comments>.
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
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		if snapshotInfoURLAllowed(raw, "") {
			t.Fatalf("snapshotInfoURLAllowed(%q, \"\") = true; an unresolved canonical host must vouch nothing", raw)
		}
		host := seadexInfoHost()
		if !snapshotInfoURLAllowed(raw, host) {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("accepted URL %q does not parse: %v", raw, err)
		}
		if !snapshotInfoURLAllowed(u.String(), host) {
			t.Fatalf("accepted URL %q is rejected after its consumer's own re-render (%q)", raw, u.String())
		}
	})
}

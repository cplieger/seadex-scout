package indexer

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzSnapshotInfoURLAllowed_failsClosedAndKeepsCanonicalLinks exercises the
// persisted-InfoURL publish gate - the load-boundary twin of the search path's
// sanitizeDisplayURL, which already carries a fuzz target - with arbitrary
// tampered feed.json values. Three invariants: an UNRESOLVED canonical host must
// vouch nothing (a hostless "https:///1" must not slip through the empty-host
// comparison, which is what the host == "" guard exists to stop); any URL the
// gate accepts must resolve to the canonical SeaDex host over http(s) with no
// userinfo under an INDEPENDENT parser (net/url), so a vouched link can never be
// one a consumer sends elsewhere; and it must stay accepted after that
// consumer's own parse/re-render round trip, so a vouched link cannot change
// identity between the gate and the arr UI that renders it as <comments>.
// A value net/url cannot parse at all is skipped rather than failed: urlform is
// this gate's parser of record (WHATWG input preprocessing), and a link no
// net/url consumer can resolve cannot be misrouted.
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
			// net/url refuses the value outright - urlform reads WHATWG input
			// preprocessing, so whitespace/control padding a browser strips is
			// vouchable here while net/url rejects it. A value no net/url
			// consumer can resolve at all cannot be MISROUTED, which is what
			// the host invariant below guards, so there is nothing to check.
			return
		}
		if got := strings.ToLower(u.Hostname()); got != host {
			t.Fatalf("accepted URL %q resolves to host %q under net/url, want the canonical %q; the gate must never vouch a link a consumer sends elsewhere", raw, got, host)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("accepted URL %q has scheme %q; only an http(s) link may be published to the arr UI", raw, u.Scheme)
		}
		if u.User != nil {
			t.Fatalf("accepted URL %q carries userinfo %q; a spoofable authority must never be published", raw, u.User)
		}
		if !snapshotInfoURLAllowed(u.String(), host) {
			t.Fatalf("accepted URL %q is rejected after its consumer's own re-render (%q)", raw, u.String())
		}
	})
}

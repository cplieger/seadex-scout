package library

import (
	"strings"
	"testing"
)

// FuzzSafeLogURL fuzzes the logging trust-boundary sanitizer with the security
// invariant that matters: secrets placed in the userinfo, query, and fragment
// of a constructed arr URL never appear in the sanitized output, whatever the
// fuzzer does to the host and path around them. net/url reshuffling cannot
// move the markers out of the stripped components (an extra "@" in the host
// only extends the userinfo; a "?" or "#" in the path only starts the query or
// fragment earlier), and a parse failure yields "" which leaks nothing. String
// reparse or idempotence oracles are deliberately NOT used - net/url's
// Parse/String round-trip is lossy on degenerate inputs ("//@// " renders as
// "//%20" which does not reparse), which is harmless for a log string.
func FuzzSafeLogURL(f *testing.F) {
	f.Add("sonarr.example", "series/frieren")
	f.Add("[::1", "path")
	f.Add("", "")
	f.Add("host:8989", "movie/1")
	f.Add("a@b", "p")
	f.Add("h", "x?y#z")
	f.Fuzz(func(t *testing.T, host, path string) {
		if strings.Contains(host+path, "SCRT") {
			return // the fuzzer injected the marker itself; skip to keep the oracle honest
		}
		raw := "https://user:SCRTpass@" + host + "/" + path + "?apikey=SCRTquery#SCRTfrag"
		got := SafeLogURL(raw)
		for _, secret := range []string{"SCRTpass", "SCRTquery", "SCRTfrag"} {
			if strings.Contains(got, secret) {
				t.Errorf("SafeLogURL(%q) = %q leaks %q across the logging trust boundary", raw, got, secret)
			}
		}
		// The scheme-less arm: "user:pass@host/..." parses as an OPAQUE URL
		// (scheme "user", the credential inside u.Opaque where the userinfo
		// strip cannot reach it), which must be dropped, not passed through.
		bare := "user:SCRTpass@" + host + "/" + path + "?apikey=SCRTquery#SCRTfrag"
		got = SafeLogURL(bare)
		for _, secret := range []string{"SCRTpass", "SCRTquery", "SCRTfrag"} {
			if strings.Contains(got, secret) {
				t.Errorf("SafeLogURL(%q) = %q leaks %q across the logging trust boundary", bare, got, secret)
			}
		}
		// The malformed-hierarchical arm: a single-slash scheme form like
		// "https:/user:pass@host/..." parses with an empty Host and the whole
		// credentialed authority inside Path, where the userinfo strip cannot
		// reach it; the host-required guard must drop it, not pass it through.
		malformed := "https:/user:SCRTpass@" + host + "/" + path + "?apikey=SCRTquery#SCRTfrag"
		got = SafeLogURL(malformed)
		for _, secret := range []string{"SCRTpass", "SCRTquery", "SCRTfrag"} {
			if strings.Contains(got, secret) {
				t.Errorf("SafeLogURL(%q) = %q leaks %q across the logging trust boundary", malformed, got, secret)
			}
		}
	})
}

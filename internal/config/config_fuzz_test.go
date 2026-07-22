package config

import (
	"net/url"
	"strings"
	"testing"
)

// The ${VAR} expansion fuzz coverage moved with the expansion engine to
// github.com/cplieger/envx/yamlenv (FuzzExpand there pins the no-op,
// keep-literal, and name-grammar invariants), and the decode-error redaction
// fuzz coverage moved with the sanitizer (FuzzSanitizeDecodeError there pins
// the excerpt-sentinel and exact-rebuild invariants); the app keeps only its
// allowlist policy (isAllowedEnvVar, unit-tested), its unknown-key echo
// policy (the sanitizeYAMLError wrapper, pinned by the Load-level tests), and
// the Load-level integration tests.

// FuzzValidateHTTPURL generalizes the credential-redaction contract of
// validateHTTPURL: however the fuzzer mangles a URL, an embedded userinfo
// password or query-string token must never reach the error message (which
// feeds the startup log).
func FuzzValidateHTTPURL(f *testing.F) {
	f.Add("ftp://user:pw@host/path")
	f.Add("http://[::1")
	f.Add("://bad")
	f.Add("javascript:alert(1)")
	f.Add("not a url at all")
	f.Add("sonarr.u")
	f.Add("")
	f.Add("https://host")
	f.Fuzz(func(t *testing.T, raw string) {
		const sentinel = "LEAK-SENTINEL-9f3a"
		// Security invariant: a credential placed in userinfo or query position
		// around the fuzzed URL never appears in the error.
		for _, in := range []string{
			"ftp://user:" + sentinel + "@" + raw,
			raw + "?apikey=" + sentinel,
			"http://" + sentinel + "@" + raw,
		} {
			if err := validateHTTPURL("sonarr.url", in); err != nil &&
				strings.Contains(err.Error(), sentinel) {
				t.Errorf("validateHTTPURL(%q) error leaks credential sentinel: %v", in, err)
			}
		}
	})
}

// FuzzURLEmbedsCredentialSupersetOfParsedQuery pins urlEmbedsCredential's
// documented contract: the raw-query scan is a strict superset of the parsed
// u.Query() view, so a credential-like parameter net/url itself can see, and
// a userinfo-bearing parseable URL, are always flagged; an unparseable URL is
// never flagged (the parse-failure negative).
func FuzzURLEmbedsCredentialSupersetOfParsedQuery(f *testing.F) {
	f.Add("http://prowlarr:9696/22/api?apikey=k")
	f.Add("http://prowlarr:9696/22/api?apikey=k;foo=x")
	f.Add("http://user:pw@host/x")
	f.Add("http://host/x?foo=1&API_KEY=2")
	f.Add("http://host/x?%61pikey=k")
	f.Add("http://host/x?mode=apikey")
	f.Add("http://[::1")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		got := urlEmbedsCredential(raw)
		u, err := url.Parse(raw)
		if err != nil {
			if got {
				t.Errorf("urlEmbedsCredential(%q) = true for an unparseable URL, want false", raw)
			}
			return
		}
		if u.User != nil && !got {
			t.Errorf("urlEmbedsCredential(%q) = false, want true for a userinfo-bearing URL", raw)
		}
		for name := range u.Query() {
			if isCredentialParam(name) && !got {
				t.Errorf("urlEmbedsCredential(%q) = false, want true: the parsed query carries credential parameter %q", raw, name)
			}
		}
	})
}

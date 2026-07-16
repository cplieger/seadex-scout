package config

import (
	"strings"
	"testing"
)

// The ${VAR} expansion fuzz coverage moved with the expansion engine to
// github.com/cplieger/envx/yamlenv (FuzzExpand there pins the no-op,
// keep-literal, and name-grammar invariants); the app keeps only its
// allowlist policy (isAllowedEnvVar, unit-tested) and the Load-level
// integration tests.

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
	f.Add("")
	f.Add("https://host")
	f.Fuzz(func(t *testing.T, raw string) {
		const sentinel = "LEAK-SENTINEL-9f3a"
		// The raw input must never be echoed wholesale (short inputs alias
		// message words, so the wholesale check needs a meaningful length).
		if err := validateHTTPURL("sonarr.url", raw); err != nil &&
			len(raw) >= 8 && strings.Contains(err.Error(), raw) {
			t.Errorf("validateHTTPURL(%q) error echoes the URL: %v", raw, err)
		}
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

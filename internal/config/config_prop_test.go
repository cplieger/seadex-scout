package config

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestValidateHTTPURLOmitsEmbeddedCredentialProperty pins the security
// invariant of the untrusted-URL validator on every PR run (the weekly fuzz
// target explores the same boundary, but its coverage-guided corpus is
// ephemeral): whatever malformed suffix follows a credential-bearing prefix,
// a rejected URL's error must never echo the embedded credential.
func TestValidateHTTPURLOmitsEmbeddedCredentialProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		const sentinel = "LEAK-SENTINEL-9f3a"
		prefix := rapid.SampledFrom([]string{
			"ftp://user:" + sentinel + "@",
			"ftp://" + sentinel + "@",
			"ftp://host/path?apikey=" + sentinel + "&next=",
		}).Draw(t, "credential-position")
		raw := rapid.String().Draw(t, "suffix")

		err := validateHTTPURL("sonarr.url", prefix+raw)
		if err == nil {
			t.Fatalf("validateHTTPURL(%q) = nil, want an error for a non-HTTP URL", prefix+raw)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("validateHTTPURL(%q) error leaks credential sentinel: %v", prefix+raw, err)
		}
	})
}

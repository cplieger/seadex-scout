package library

import (
	"net/url"
	"testing"

	"pgregory.net/rapid"
)

// TestSafeLogURLPropStripsSensitiveComponents drives SafeLogURL with
// generated credentialed URLs and uses net/url fields as an independent
// oracle: userinfo, query, and fragment must be stripped, the non-sensitive
// scheme/host/path preserved, and the sanitizer idempotent.
func TestSafeLogURLPropStripsSensitiveComponents(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		user := rapid.StringMatching(`[A-Za-z0-9]{1,16}`).Draw(t, "user")
		password := rapid.StringMatching(`[A-Za-z0-9]{1,16}`).Draw(t, "password")
		token := rapid.StringMatching(`[A-Za-z0-9]{1,16}`).Draw(t, "token")
		fragment := rapid.StringMatching(`[A-Za-z0-9]{1,16}`).Draw(t, "fragment")
		path := rapid.StringMatching(`[A-Za-z0-9/_-]{0,64}`).Draw(t, "path")
		raw := (&url.URL{
			Scheme:   "https",
			Host:     "sonarr.example",
			Path:     "/" + path,
			User:     url.UserPassword(user, password),
			RawQuery: url.Values{"apikey": {token}}.Encode(),
			Fragment: fragment,
		}).String()

		got := SafeLogURL(raw)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("url.Parse(SafeLogURL(%q)): %v", raw, err)
		}
		if u.Scheme != "https" || u.Host != "sonarr.example" || u.Path != "/"+path {
			t.Fatalf("SafeLogURL(%q) = %q, want scheme, host, and path preserved", raw, got)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("SafeLogURL(%q) = %q, want userinfo, query, and fragment stripped", raw, got)
		}
		if twice := SafeLogURL(got); twice != got {
			t.Fatalf("SafeLogURL is not idempotent: once %q, twice %q", got, twice)
		}
	})
}

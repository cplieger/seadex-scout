package displaylink

import (
	"strings"
	"testing"

	"github.com/cplieger/urlform"
)

// TestVouch pins the structural legs every display-publish gate in the app
// shares, one case per refusal a renderer would otherwise honor. The host is
// returned as urlform's ASCII-lowercased evidence, since each caller compares
// it against its own trusted host.
func TestVouch(t *testing.T) {
	cases := map[string]struct {
		raw     string
		host    string
		vouched bool
	}{
		"https absolute":     {raw: "https://nyaa.si/view/1", host: "nyaa.si", vouched: true},
		"cleartext absolute": {raw: "http://nyaa.si/view/1", host: "nyaa.si", vouched: true},
		"mixed case host":    {raw: "HTTPS://Nyaa.SI/view/1", host: "nyaa.si", vouched: true},
		"query only":         {raw: "https://animebytes.tv/torrents.php?id=1&torrentid=2", host: "animebytes.tv", vouched: true},

		"empty":              {raw: ""},
		"relative":           {raw: "/view/1"},
		"protocol relative":  {raw: "//nyaa.si/view/1"},
		"schemeless host":    {raw: "nyaa.si/view/1"},
		"javascript scheme":  {raw: "javascript:alert(1)"},
		"data scheme":        {raw: "data:text/html,<script>x</script>"},
		"file scheme":        {raw: "file:///etc/passwd"},
		"userinfo authority": {raw: "https://nyaa.si@evil.example/view/1"},
		"backslash smuggled": {raw: `https:\\nyaa.si\view\1`},
		"tab smuggled":       {raw: "https://nyaa.si/vi\tew/1"},
		"newline smuggled":   {raw: "https://nyaa.si/vi\new/1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			host, ok := vouchHost(tc.raw)
			if ok != tc.vouched {
				t.Errorf("vouchHost(%q) ok = %v, want %v", tc.raw, ok, tc.vouched)
			}
			if host != tc.host {
				t.Errorf("vouchHost(%q) host = %q, want %q", tc.raw, host, tc.host)
			}
		})
	}
}

// FuzzVouch pins the two invariants a caller relies on across arbitrary
// untrusted input: a refused value yields NO host evidence (so a caller cannot
// accidentally match a host from a dropped URL), and a vouched value carries no
// EMBEDDED smuggling byte - the caller emits the raw string, so a de-smuggled
// reading must never be vouched. Edge whitespace is exempt because the
// classifier trims it (the WHATWG C0-and-space strip set) before reading
// anything - a trailing tab smuggles nothing - which is why the oracle looks at
// the trimmed value.
func FuzzVouch(f *testing.F) {
	f.Add("https://nyaa.si/view/1")
	f.Add("//nyaa.si/view/1")
	f.Add(`https:\\nyaa.si\view`)
	f.Add("https://nyaa.si/vi\tew/1")
	f.Add("https://nyaa.si/view/1\t")
	f.Add("javascript:alert(1)")
	f.Add("https://user:pw@nyaa.si/")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		host, ok := vouchHost(raw)
		if !ok {
			if host != "" {
				t.Fatalf("vouchHost(%q) refused the value but returned host %q", raw, host)
			}
			return
		}
		if host == "" {
			t.Fatalf("vouchHost(%q) vouched the value but returned no host evidence", raw)
		}
		if trimmed := strings.Trim(raw, c0AndSpace); strings.ContainsAny(trimmed, "\\\t\n\r") {
			t.Fatalf("vouchHost(%q) vouched a value carrying a smuggling byte", raw)
		}
	})
}

// c0AndSpace is the WHATWG leading/trailing strip set (C0 controls plus space)
// urlform edge-trims before it classifies anything, so the fuzz oracle judges
// smuggling on the same string the classifier read.
var c0AndSpace = func() string {
	b := make([]byte, 0, 0x21)
	for c := byte(0); c <= 0x20; c++ {
		b = append(b, c)
	}
	return string(b)
}()

// TestVouchSanitizingFormDiffersOnlyOnUserInfo pins the ONE leg that separates
// the sanitizing reading from the refusing one, which is the whole reason the
// second entry point exists: a userinfo authority is vouched (its caller strips
// it) while every other refusal is shared verbatim. Without this test the two
// readings could drift apart silently, which is exactly the two-homes drift
// moving the legs here removed (l-f208).
func TestVouchSanitizingFormDiffersOnlyOnUserInfo(t *testing.T) {
	cases := map[string]struct {
		raw        string
		sanitizing bool
	}{
		"userinfo authority": {raw: "https://user:pw@sonarr.example/series/x", sanitizing: true},
		"plain absolute":     {raw: "https://sonarr.example/series/x", sanitizing: true},
		"relative":           {raw: "/series/x"},
		"javascript scheme":  {raw: "javascript:alert(1)"},
		"backslash smuggled": {raw: `https:\\sonarr.example\series`},
		"tab smuggled":       {raw: "https://sonarr.example/ser\ties"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := urlform.Classify(tc.raw)
			if got := VouchSanitizingForm(&f); got != tc.sanitizing {
				t.Errorf("VouchSanitizingForm(%q) = %v, want %v", tc.raw, got, tc.sanitizing)
			}
			// VouchForm is the sanitizing reading MINUS a userinfo authority, so
			// the two agree on every value that carries none.
			wantRefusing := tc.sanitizing && !f.HasUserInfo
			if got := VouchForm(&f); got != wantRefusing {
				t.Errorf("VouchForm(%q) = %v, want %v", tc.raw, got, wantRefusing)
			}
		})
	}
}

// vouchHost is the test-local composition the deleted exported Vouch wrapper
// used to provide: classify once, apply the shared structural legs, and return
// urlform's ASCII-lowercased host evidence. It lives here because no production
// caller needs the host-only shape - every gate holds a classified form and
// calls VouchForm directly - while these assertions are still the ones that pin
// "a vouched value always yields host evidence, a refused one never does".
func vouchHost(raw string) (host string, ok bool) {
	f := urlform.Classify(raw)
	if !VouchForm(&f) {
		return "", false
	}
	return f.Host, true
}

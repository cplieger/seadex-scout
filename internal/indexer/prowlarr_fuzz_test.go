package indexer

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
)

// FuzzSameHTTPOrigin_acceptedURLTargetsProwlarrOrigin exercises the SSRF gate
// (CWE-918) on the Prowlarr hop with arbitrary tracker-controlled URL strings.
// The consumer-side oracle: any URL the gate accepts must, when interpreted the
// way its consumer will interpret it (http.NewRequest's own parse), build a
// plain http(s) request without userinfo whose host equals the configured
// Prowlarr endpoint's.
func FuzzSameHTTPOrigin_acceptedURLTargetsProwlarrOrigin(f *testing.F) {
	f.Add("http://prowlarr:9696/1/download?link=abc")
	f.Add("https://prowlarr:9696/1/download")
	f.Add("file:///etc/passwd")
	f.Add("http://prowlarr:9696@evil.internal/steal")
	f.Add("http://user@prowlarr:9696/x")
	f.Add("http://sonarr:8989/api")
	f.Add("//prowlarr:9696/download")
	f.Add("http://PROWLARR:9696/x")
	f.Add("HTTP://prowlarr:9696/x")
	f.Add(" http://prowlarr:9696/x")
	f.Add("http:///download")
	f.Fuzz(func(t *testing.T, raw string) {
		origin, err := url.Parse("http://prowlarr:9696/1/api")
		if err != nil {
			t.Fatalf("parse origin: %v", err)
		}
		if !sameHTTPOrigin(raw, origin) {
			return
		}
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatalf("accepted URL %q does not build an HTTP request: %v", raw, err)
		}
		if req.URL.User != nil {
			t.Fatalf("accepted URL %q carries userinfo", raw)
		}
		if s := strings.ToLower(req.URL.Scheme); s != "http" && s != "https" {
			t.Fatalf("accepted URL %q resolves to scheme %q, want http(s)", raw, req.URL.Scheme)
		}
		if !strings.EqualFold(req.URL.Host, origin.Host) {
			t.Fatalf("accepted URL %q resolves to host %q, want the origin %q", raw, req.URL.Host, origin.Host)
		}
	})
}

// FuzzSanitizeDisplayURL_keptURLsAreCanonicalTrackerLinks exercises the
// display-URL gate on the two passthrough fields (InfoURL/GUID) with
// arbitrary tracker-controlled strings. Invariants: the gate returns either
// "" or its input unchanged, and anything it keeps must - under the
// consumer's own interpretation (url.Parse) - be an absolute http(s) URL,
// free of userinfo, whose hostname the served scope's tracker predicate
// accepts; an unknown scope keeps nothing.
func FuzzSanitizeDisplayURL_keptURLsAreCanonicalTrackerLinks(f *testing.F) {
	f.Add("nyaa", "https://nyaa.si/view/1234567")
	f.Add("ab", "https://animebytes.tv/torrent/1167293/group")
	f.Add("nyaa", "javascript:alert(1)")
	f.Add("nyaa", "https://nyaa.si@evil.example/phish")
	f.Add("nyaa", "https://trusted@nyaa.si/view/1")
	f.Add("nyaa", "//nyaa.si/view/1")
	f.Add("nyaa", "https://evilnyaa.si/view/1")
	f.Add("ab", "https://nyaa.si/view/1")
	f.Add("other", "https://nyaa.si/view/1")
	f.Add("nyaa", "")
	f.Fuzz(func(t *testing.T, scope, raw string) {
		got := sanitizeDisplayURL(scope, raw)
		if got == "" {
			return
		}
		if got != raw {
			t.Fatalf("sanitizeDisplayURL(%q, %q) = %q, want the input unchanged or empty", scope, raw, got)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("kept URL %q does not parse: %v", got, err)
		}
		if u.User != nil {
			t.Fatalf("kept URL %q carries userinfo", got)
		}
		if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
			t.Fatalf("kept URL %q has scheme %q, want http(s)", got, u.Scheme)
		}
		switch scope {
		case upstreamNyaa:
			if !release.IsNyaaHost(u.Hostname()) {
				t.Fatalf("kept URL %q host %q fails the nyaa tracker predicate", got, u.Hostname())
			}
		case upstreamAB:
			if !release.IsAnimeBytesHost(u.Hostname()) {
				t.Fatalf("kept URL %q host %q fails the animebytes tracker predicate", got, u.Hostname())
			}
		default:
			t.Fatalf("scope %q kept URL %q, want everything blanked under an unknown scope", scope, got)
		}
	})
}

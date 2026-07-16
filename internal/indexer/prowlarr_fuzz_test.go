package indexer

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
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

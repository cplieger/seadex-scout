package library

import (
	"testing"
)

// TestSafeLogURL covers the sanitizer's edge arms directly: an empty and an
// unparseable URL yield empty strings, and a clean deep-link is unchanged.
func TestSafeLogURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"unparseable", "http://[::1", ""},
		{"clean link unchanged", "https://sonarr.example/series/frieren", "https://sonarr.example/series/frieren"},
		{"plain-http internal link unchanged", "http://sonarr.internal:8989/series/frieren", "http://sonarr.internal:8989/series/frieren"},
		{"userinfo stripped", "https://user:pass@host/movie/1", "https://host/movie/1"},
		{"query token stripped", "https://host/movie/1?apikey=secret", "https://host/movie/1"},
		{"opaque credentialed URL dropped", "user:pass@host/series/x", ""},
		{"malformed single-slash credentialed URL dropped", "https:/user:pass@host/series/x", ""},
		{"malformed four-slash credentialed URL dropped", "https:////user:pass@host/series/x", ""},
		{"port-only-authority credentialed URL dropped", "https://:443/user:pass@sonarr.example/series/x", ""},
		{"non-http scheme dropped", "ftp://user:pass@host/series/x", ""},
		{"scheme-relative credentialed URL dropped", "//user:pass@host/series/x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeLogURL(tt.in); got != tt.want {
				t.Errorf("SafeLogURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSafeLogURLRefusesSmuggledSeparators pins the publish-or-drop refusal the
// admission half documents: a raw URL carrying a backslash authority separator
// or an embedded tab/newline is dropped rather than published in its
// de-smuggled form. Without the HasTabOrNewline guard the tab/newline inputs
// below emit "https://sonarr.example/series/x" - a clickable link whose raw
// form the operator never configured - because urlform's WHATWG preprocessing
// removes those bytes before net/url ever sees them.
func TestSafeLogURLRefusesSmuggledSeparators(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"backslash authority separator", `https://sonarr.example\@evil.example/series/x`},
		{"backslash after scheme", `https:/\sonarr.example/series/x`},
		{"tab inside host", "https://son\tarr.example/series/x"},
		{"newline inside path", "https://sonarr.example/ser\nies/x"},
		{"tab inside scheme", "ht\ttps://sonarr.example/series/x"},
		{"credentialed backslash host", `https://user:pass@sonarr.example\@evil.example/x?apikey=secret`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeLogURL(tt.in); got != "" {
				t.Errorf("SafeLogURL(%q) = %q, want \"\" (a de-smuggled URL is not vouchable)", tt.in, got)
			}
		})
	}
}

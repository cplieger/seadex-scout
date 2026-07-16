package library

import "testing"

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
		{"userinfo stripped", "https://user:pass@host/movie/1", "https://host/movie/1"},
		{"query token stripped", "https://host/movie/1?apikey=secret", "https://host/movie/1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeLogURL(tt.in); got != tt.want {
				t.Errorf("SafeLogURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

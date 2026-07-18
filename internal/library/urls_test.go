package library

import (
	"slices"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
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

// TestSnapshotSanitizedForStorage pins the persistence trust boundary: a
// credentialed ArrURL does not survive SanitizedForStorage, the rest of the
// item (Groups, SeasonGroups, Current) is untouched, and the receiver
// snapshot's items are not mutated (the sanitized copy is independent).
func TestSnapshotSanitizedForStorage(t *testing.T) {
	snap := Snapshot{Items: []Item{{
		Arr:          ArrSonarr,
		ArrID:        1,
		Title:        "Alpha",
		ArrURL:       "https://user:pass@sonarr.example/series/alpha",
		Groups:       []string{"pmr"},
		SeasonGroups: map[int][]string{1: {"pmr"}},
		Current:      release.Release{Group: "pmr", Resolution: "1080p"},
		HasFile:      true,
	}}}

	got := snap.SanitizedForStorage()

	if got.Items[0].ArrURL != "https://sonarr.example/series/alpha" {
		t.Errorf("sanitized ArrURL = %q, want the credential stripped", got.Items[0].ArrURL)
	}
	it := got.Items[0]
	if !slices.Equal(it.Groups, []string{"pmr"}) || !slices.Equal(it.SeasonGroups[1], []string{"pmr"}) {
		t.Errorf("Groups/SeasonGroups changed: %v / %v, want untouched", it.Groups, it.SeasonGroups)
	}
	if it.Current.Group != "pmr" || it.Current.Resolution != "1080p" {
		t.Errorf("Current = %+v, want untouched", it.Current)
	}
	if snap.Items[0].ArrURL != "https://user:pass@sonarr.example/series/alpha" {
		t.Errorf("receiver ArrURL = %q, want the original snapshot unmutated", snap.Items[0].ArrURL)
	}
}

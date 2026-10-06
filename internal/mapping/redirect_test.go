package mapping

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mustRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		t.Fatalf("Setup: NewRequest(%q): %v", rawURL, err)
	}
	return req
}

// TestRedirectPolicy pins the hop chain GitHub answers DefaultURL with: the
// release tag on github.com, then the release-asset host. Any other target,
// and a downgrade to http, is refused with errRedirectRefused.
func TestRedirectPolicy(t *testing.T) {
	const tag = "https://github.com/cplieger/animap/releases/download/v1.0.0/animap.json"
	for _, tc := range []struct {
		name   string
		target string
		via    []string
		follow bool
	}{
		{name: "tag", target: tag, via: []string{DefaultURL}, follow: true},
		{name: "release-assets", target: "https://release-assets.githubusercontent.com/github-production-release-asset/1/2?sig=x", via: []string{DefaultURL, tag}, follow: true},
		{name: "objects", target: "https://objects.githubusercontent.com/github-production-release-asset-2e65be/1/2", via: []string{DefaultURL, tag}, follow: true},
		{name: "raw user content", target: "https://raw.githubusercontent.com/x/y/main/animap.json", via: []string{DefaultURL}},
		{name: "another host", target: "https://example.net/animap.json", via: []string{DefaultURL, tag}},
		{name: "downgrade", target: "http://release-assets.githubusercontent.com/a", via: []string{DefaultURL, tag}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			via := make([]*http.Request, 0, len(tc.via))
			for _, u := range tc.via {
				via = append(via, mustRequest(t, u))
			}
			err := RedirectPolicy(mustRequest(t, tc.target), via)
			if tc.follow && err != nil {
				t.Errorf("RedirectPolicy(%q) = %v, want nil", tc.target, err)
			}
			if !tc.follow && !errors.Is(err, errRedirectRefused) {
				t.Errorf("RedirectPolicy(%q) = %v, want errRedirectRefused", tc.target, err)
			}
		})
	}
}

// TestLoader_refreshCache_refusedRedirectAdvancesTheStreak drives a refused hop
// through the real client: it keeps the stale map and advances the rejection
// streak, because a constant URL re-refuses the same hop every cycle.
func TestLoader_refreshCache_refusedRedirectAdvancesTheStreak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/moved/animap.json", http.StatusFound)
	}))
	defer ts.Close()
	client := ts.Client()
	client.CheckRedirect = RedirectPolicy
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(client, ts.URL, WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	if _, ok := errors.AsType[*StaleMapError](err); !ok {
		t.Fatalf("refreshCache through a refused redirect: error = %v, want a *StaleMapError", err)
	}
	if !errors.Is(err, errRedirectRefused) {
		t.Errorf("refreshCache through a refused redirect: error = %v, want it to wrap errRedirectRefused", err)
	}
	if next.RejectedRefreshes != 1 {
		t.Errorf("refreshCache through a refused redirect: RejectedRefreshes = %d, want 1", next.RejectedRefreshes)
	}
	if len(next.Records) != 1 {
		t.Errorf("refreshCache through a refused redirect kept %d records, want the stale 1", len(next.Records))
	}
}

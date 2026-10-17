package mapping

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cplieger/animap/mapread"
)

// errTransport fails every request with a plain error httpx does not retry,
// so a refresh returns after the first attempt without sleeping.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport refused by test")
}

// TestLoader_refreshCache_transportErrorKeepsStale pins the transport arm: a
// request that gets no response at all degrades to the stale map with no
// streak and keeps the cache unchanged.
func TestLoader_refreshCache_transportErrorKeepsStale(t *testing.T) {
	prev := staleCache("v2026.10.16")
	prev.RejectedRefreshes = 3
	l := NewLoader(&http.Client{Transport: errTransport{}}, mapread.Source{Repo: DefaultRepo}, WithLogger(discardLogger()))
	next, err := l.refreshCache(t.Context(), prev)
	if _, ok := errors.AsType[*StaleMapError](err); !ok {
		t.Fatalf("transport-error refresh error = %v, want a *StaleMapError", err)
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 || next.Tag != "v2026.10.16" || next.RejectedRefreshes != 3 {
		t.Errorf("transport-error refresh = %+v, want the stale cache with its streak of 3", next)
	}
}

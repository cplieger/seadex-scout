package mapping

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// errTransport fails every request with a plain (non-transient) error, so
// httpx.RetryWithBackoff returns after the first attempt without sleeping.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("transport refused by test")
}

// TestLoader_refreshCache_transportErrorKeepsStale pins conditionalGet's
// http.Do error branch: a request that fails at the transport (no response at
// all, as opposed to buildRequest failing or an HTTP error status) degrades to
// the stale map with an error rather than losing the cached records.
func TestLoader_refreshCache_transportErrorKeepsStale(t *testing.T) {
	prev := &Cache{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Records:   []Record{{AniListID: 1, Type: "TV", TvdbID: 100}},
	}
	l := NewLoader(&http.Client{Transport: errTransport{}}, "http://unused.invalid", "", time.Hour, discardLogger())
	next, err := l.refreshCache(context.Background(), prev)
	if err == nil {
		t.Fatal("transport-error refresh returned nil error, want degraded error")
	}
	if len(next.Records) != 1 || next.Records[0].AniListID != 1 {
		t.Fatalf("transport-error refresh records = %+v, want stale record id 1", next.Records)
	}
}

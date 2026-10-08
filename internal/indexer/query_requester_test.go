package indexer

import (
	"net/url"
	"testing"
)

// TestRequesterOf pins which arr a request is attributed to: a category list
// wholly inside one arr's range names it, a mixed, out-of-range or malformed
// list names neither, and only a list naming nothing falls back to t.
func TestRequesterOf(t *testing.T) {
	tests := []struct {
		kind, cat string
		want      requester
	}{
		{kind: "search", cat: "5070,5000", want: requesterTV},
		{kind: "search", cat: " 5999 , 5000", want: requesterTV},
		{kind: "search", cat: "5070,", want: requesterTV},
		{kind: "search", cat: "2000", want: requesterMovies},
		{kind: "tvsearch", cat: "2040", want: requesterMovies},
		{kind: "search", cat: "2000,5070", want: requesterAny},
		{kind: "tvsearch", cat: "8000", want: requesterAny},
		{kind: "search", cat: "2040,garbage", want: requesterAny},
		{kind: "search", cat: "5070,garbage", want: requesterAny},
		{kind: "tvsearch", cat: "abc", want: requesterAny},
		{kind: "search", cat: "0,5070", want: requesterAny},
		{kind: "search", cat: "-5070", want: requesterAny},
		{kind: "tvsearch", cat: "", want: requesterTV},
		{kind: "tv-search", cat: " , ", want: requesterTV},
		{kind: "movie", cat: "", want: requesterMovies},
		{kind: "search", cat: "", want: requesterAny},
	}
	for _, tc := range tests {
		if got := requesterOf(url.Values{"t": {tc.kind}, "cat": {tc.cat}}); got != tc.want {
			t.Errorf("requesterOf(t=%q, cat=%q) = %v, want %v", tc.kind, tc.cat, got, tc.want)
		}
	}
}

// TestTwinsFor pins which of a release's items each requester gets: Sonarr
// the twin in place of the original and never a film's own title, Radarr
// never a twin, and an unattributed request every item.
func TestTwinsFor(t *testing.T) {
	tests := []struct {
		class                  requester
		hasTwin, sonarrOrig    bool
		wantOriginal, wantTwin bool
	}{
		{class: requesterTV, hasTwin: true, sonarrOrig: true, wantTwin: true},
		{class: requesterTV, hasTwin: true, sonarrOrig: false, wantTwin: true},
		{class: requesterTV, sonarrOrig: true, wantOriginal: true},
		{class: requesterTV, sonarrOrig: false},
		{class: requesterMovies, hasTwin: true, wantOriginal: true},
		{class: requesterMovies, wantOriginal: true},
		{class: requesterAny, hasTwin: true, wantOriginal: true, wantTwin: true},
		{class: requesterAny, wantOriginal: true},
	}
	for _, tc := range tests {
		original, twin := tc.class.twinsFor(tc.hasTwin, tc.sonarrOrig)
		if original != tc.wantOriginal || twin != tc.wantTwin {
			t.Errorf("requester(%v).twinsFor(twin %v, offered to Sonarr %v) = %v, %v, want %v, %v",
				tc.class, tc.hasTwin, tc.sonarrOrig, original, twin, tc.wantOriginal, tc.wantTwin)
		}
	}
}

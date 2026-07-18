package indexer

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestRenderFeed_usesStableGUIDFallback pins the documented GUID fallback
// order (explicit GUID -> info hash -> download URL) independently of the
// production guid() helper, which the round-trip fuzz oracle also calls.
func TestRenderFeed_usesStableGUIDFallback(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	tests := map[string]struct {
		want string
		item item
	}{
		"explicit GUID wins": {
			item: item{GUID: "explicit", InfoHash: hash, DownloadURL: "https://prowlarr.test/download/1"},
			want: "explicit",
		},
		"info hash is the first fallback": {
			item: item{InfoHash: hash, DownloadURL: "https://prowlarr.test/download/1"},
			want: hash,
		},
		"download URL is the final fallback": {
			item: item{DownloadURL: "https://prowlarr.test/download/1"},
			want: "https://prowlarr.test/download/1",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			parsed, err := parseTorznab([]byte(renderFeed([]item{tc.item})))
			if err != nil {
				t.Fatalf("parseTorznab(renderFeed(item)): %v", err)
			}
			if len(parsed) != 1 {
				t.Fatalf("parsed item count = %d, want 1", len(parsed))
			}
			if got := parsed[0].GUID; got != tc.want {
				t.Errorf("rendered GUID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteItemSaturatesPeerCount pins writeItem's overflow guard: attrInt
// accepts counts through math.MaxInt, so a malformed-but-valid upstream item
// with seeders and leechers both at math.MaxInt must render a peers attr
// saturated at math.MaxInt - never a wrapped negative value, which would
// contradict toItem's non-negative normalization.
func TestWriteItemSaturatesPeerCount(t *testing.T) {
	tests := map[string]struct {
		wantPeers         int
		seeders, leechers int
	}{
		"both at MaxInt saturate":        {seeders: math.MaxInt, leechers: math.MaxInt, wantPeers: math.MaxInt},
		"sum just over MaxInt saturates": {seeders: math.MaxInt - 1, leechers: 2, wantPeers: math.MaxInt},
		"ordinary counts sum exactly":    {seeders: 146, leechers: 3, wantPeers: 149},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			it := item{Title: "x", Seeders: tc.seeders, Leechers: tc.leechers}
			writeItem(&b, &it)
			out := b.String()
			want := `<torznab:attr name="peers" value="` + strconv.Itoa(tc.wantPeers) + `"/>`
			if !strings.Contains(out, want) {
				t.Errorf("rendered item missing %s:\n%s", want, out)
			}
			if strings.Contains(out, `value="-`) {
				t.Errorf("rendered a negative attribute value:\n%s", out)
			}
		})
	}
}

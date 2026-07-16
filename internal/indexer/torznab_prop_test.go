package indexer

import (
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// TestTorznabRenderParse_roundTripsItemsProperty is the every-PR randomized
// complement to FuzzParseTorznab's committed seeds: a rendered item with
// XML-escaped text, IDs, hashes, categories, dates, sizes, and peer counts must
// parse back identically.
func TestTorznabRenderParse_roundTripsItemsProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		token := rapid.StringMatching(`[A-Za-z0-9]{1,40}`).Draw(t, "token")
		special := rapid.SampledFrom([]string{"plain", "a&b", "<tag>", `"quoted"`, "'single'"}).Draw(t, "special")
		want := item{
			Title:       token + special,
			GUID:        "guid:" + token,
			InfoURL:     "https://example.test/info/" + token,
			DownloadURL: "https://example.test/download/" + token,
			InfoHash:    rapid.StringMatching(`[0-9a-f]{40}`).Draw(t, "info_hash"),
			Categories:  []int{catAnime},
			PubDate:     time.Unix(rapid.Int64Range(0, 4_000_000_000).Draw(t, "unix_seconds"), 0).UTC(),
			Size:        rapid.Int64Range(0, 1<<40).Draw(t, "size"),
			Seeders:     rapid.IntRange(1, 10_000).Draw(t, "seeders"),
			Leechers:    rapid.IntRange(0, 10_000).Draw(t, "leechers"),
		}

		gotItems, err := parseTorznab([]byte(renderFeed([]item{want})))
		if err != nil {
			t.Fatalf("parseTorznab(renderFeed(item)): %v", err)
		}
		if len(gotItems) != 1 {
			t.Fatalf("round-trip item count = %d, want 1", len(gotItems))
		}
		got := gotItems[0]
		if got.Title != want.Title || got.GUID != want.GUID || got.InfoURL != want.InfoURL || got.DownloadURL != want.DownloadURL {
			t.Fatalf("round-trip text fields = %#v, want %#v", got, want)
		}
		if got.InfoHash != want.InfoHash || !slices.Equal(got.Categories, want.Categories) {
			t.Fatalf("round-trip attrs = hash %q categories %v, want hash %q categories %v", got.InfoHash, got.Categories, want.InfoHash, want.Categories)
		}
		if got.Size != want.Size || got.Seeders != want.Seeders || got.Leechers != want.Leechers || !got.PubDate.Equal(want.PubDate) {
			t.Fatalf("round-trip numeric fields = %#v, want %#v", got, want)
		}
	})
}

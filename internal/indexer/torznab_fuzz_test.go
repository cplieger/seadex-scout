package indexer

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/runesafe"
)

// FuzzParseTorznab exercises the Prowlarr Torznab XML parser on arbitrary bytes.
// Prowlarr forwards a tracker's parsed response, so the title/URL/attr fields
// are ultimately tracker-controlled: the parser must never panic, and every item
// it accepts must survive a render->re-parse round trip with stable fields after
// the renderer's documented normalization (default anime category, seeders floor,
// and GUID fallback). This is the display/pass-through decode boundary the feed
// re-renders, so the invariant pins the fields Sonarr/Radarr consume rather than
// merely checking the item count.
func FuzzParseTorznab(f *testing.F) {
	f.Add(sampleFeed)
	f.Add(`<?xml version="1.0"?><rss><channel><item><title>x</title></item></channel></rss>`)
	f.Add(`<rss><channel><item><torznab:attr name="seeders" value="-5"/><torznab:attr name="peers" value="1"/></item></channel></rss>`)
	f.Add(`<rss><channel><item><size>notanumber</size><torznab:attr name="size" value="99"/></item></channel></rss>`)
	f.Add(`<rss><channel><item><title>a &amp; b "q"</title><guid>g</guid></item></channel></rss>`)
	f.Add(`<rss><channel><item><guid></guid><torznab:attr name="infohash" value="ABCDEF1234567890ABCDEF1234567890ABCDEF12"/></item></channel></rss>`)
	f.Add(`<rss><channel><item><enclosure url="http://prowlarr/download?id=1&amp;token=x" length="123" type="application/x-bittorrent"/><torznab:attr name="category" value="2000"/></item></channel></rss>`)
	f.Add(`<rss><channel><item><torznab:attr name="seeders" value="9223372036854775807"/><torznab:attr name="leechers" value="9223372036854775807"/></item></channel></rss>`)
	f.Add(`<?xml version="1.0"?><error code="100" description="Incorrect user credentials"/>`)
	f.Add("")
	f.Add("not xml at all")
	f.Fuzz(func(t *testing.T, body string) {
		items, err := parseTorznab([]byte(body))
		if err != nil {
			return // a decode error is a valid outcome for hostile input
		}
		rendered, _ := renderFeed(items)
		reparsed, err := parseTorznab([]byte(rendered))
		if err != nil {
			// An input accepted near the decode limits can re-render
			// slightly larger than it parsed (fixed rendered attr names,
			// the guid fallback duplicating a URL into a second element),
			// so a fail-closed limit rejection on re-parse is a valid
			// outcome. Any other re-parse failure is a fidelity bug.
			if _, ok := errors.AsType[*torznabLimitError](err); ok {
				return
			}
			t.Fatalf("re-parse of rendered feed failed: %v\nrendered: %s", err, rendered)
		}
		if len(reparsed) != len(items) {
			t.Fatalf("round-trip item count = %d, want %d\ninput: %q", len(reparsed), len(items), body)
		}
		for i := range items {
			want := normalizedRenderedItem(items[i])
			got := reparsed[i]
			if got.Title != want.Title {
				t.Errorf("item %d title = %q, want %q", i, got.Title, want.Title)
			}
			if got.GUID != want.GUID {
				t.Errorf("item %d guid = %q, want %q", i, got.GUID, want.GUID)
			}
			if got.InfoURL != want.InfoURL {
				t.Errorf("item %d infoURL = %q, want %q", i, got.InfoURL, want.InfoURL)
			}
			if got.DownloadURL != want.DownloadURL {
				t.Errorf("item %d downloadURL = %q, want %q", i, got.DownloadURL, want.DownloadURL)
			}
			if got.InfoHash != want.InfoHash {
				t.Errorf("item %d infoHash = %q, want %q", i, got.InfoHash, want.InfoHash)
			}
			if !slices.Equal(got.Categories, want.Categories) {
				t.Errorf("item %d categories = %v, want %v", i, got.Categories, want.Categories)
			}
			if got.Size != want.Size {
				t.Errorf("item %d size = %d, want %d", i, got.Size, want.Size)
			}
			if got.Seeders != want.Seeders {
				t.Errorf("item %d seeders = %d, want %d", i, got.Seeders, want.Seeders)
			}
			if got.Leechers != want.Leechers {
				t.Errorf("item %d leechers = %d, want %d", i, got.Leechers, want.Leechers)
			}
			if !got.PubDate.Equal(want.PubDate) {
				t.Errorf("item %d pubDate = %v, want %v", i, got.PubDate, want.PubDate)
			}
		}
	})
}

func normalizedRenderedItem(it item) item {
	it.GUID = it.guid()
	// escTo sanitizes every rendered text field (runesafe.Sanitize maps
	// C1/bidi/U+2028-class runes and DEL to spaces, invalid UTF-8 to
	// U+FFFD) and toItem re-trims on re-parse; and pubDate renders at
	// RFC1123Z second precision while parsePubDate's RFC3339 layout
	// accepts fractional seconds on input. Mirror both, or a valid XML
	// input carrying such runes (or a fractional pubDate) fails the
	// round trip spuriously.
	it.Title = strings.TrimSpace(runesafe.Sanitize(it.Title))
	it.GUID = strings.TrimSpace(runesafe.Sanitize(it.GUID))
	it.InfoURL = strings.TrimSpace(runesafe.Sanitize(it.InfoURL))
	it.DownloadURL = strings.TrimSpace(runesafe.Sanitize(it.DownloadURL))
	it.PubDate = it.PubDate.Truncate(time.Second)
	if len(it.Categories) == 0 {
		it.Categories = []int{catAnime}
	}
	it.Seeders = max(it.Seeders, 1)
	it.Leechers = max(it.Leechers, 0)
	// Mirror writeItem's peers saturation: the rendered peers attr is
	// seeders+leechers capped at math.MaxInt, and the re-parse derives
	// leechers from it, so leechers survive the round trip capped the same way.
	it.Leechers = min(it.Leechers, math.MaxInt-it.Seeders)
	return it
}

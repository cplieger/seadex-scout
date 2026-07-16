package indexer

import (
	"testing"

	"pgregory.net/rapid"
)

// TestExtractID_roundTripsNumericIDsProperty is the every-PR randomized
// complement to the fixed fuzz corpus: arbitrary-width numeric IDs followed by
// every supported delimiter must extract intact from Nyaa view URLs and the
// AnimeBytes /torrent/{id} permalink path. The torrentid= query form is
// component-aware (the id must be the whole parameter value), so only genuine
// URL-level terminators (a following param or a fragment) may trail it - a "?"
// or "/" inside a query value is literal content and must NOT yield an id.
func TestExtractID_roundTripsNumericIDsProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.StringMatching(`[0-9]{1,64}`).Draw(t, "id")
		delimiter := rapid.SampledFrom([]string{"?", "#", "/", "&"}).Draw(t, "delimiter")

		if got := nyaaID("https://nyaa.si/view/" + id + delimiter + "tail"); got != id {
			t.Fatalf("nyaaID id = %q, want %q", got, id)
		}
		if got := animeBytesID("https://animebytes.tv/torrent/" + id + delimiter + "tail"); got != id {
			t.Fatalf("animeBytesID permalink id = %q, want %q", got, id)
		}
		queryTail := rapid.SampledFrom([]string{"", "&next=1", "#frag"}).Draw(t, "queryTail")
		if got := animeBytesID("/torrents.php?id=1&torrentid=" + id + queryTail); got != id {
			t.Fatalf("animeBytesID query id = %q, want %q", got, id)
		}
		// A "?" or "/" after the id inside the query is literal value content
		// under component-aware parsing: no id may be extracted from it.
		literalTail := rapid.SampledFrom([]string{"?", "/"}).Draw(t, "literalTail")
		if got := animeBytesID("/torrents.php?id=1&torrentid=" + id + literalTail + "tail"); got != "" {
			t.Fatalf("animeBytesID query id with literal %q tail = %q, want empty", literalTail, got)
		}
	})
}

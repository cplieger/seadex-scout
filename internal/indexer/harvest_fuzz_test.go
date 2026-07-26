package indexer

import (
	"maps"
	"math"
	"strings"
	"testing"
)

// FuzzHarvestCheckpointCodec exercises the persisted harvest_cursor decoder
// on arbitrary snapshot strings (the codec's own contract covers hand-edited
// or corrupted snapshots). Invariants: decode never panics, always returns an
// allocated Pages map (callers write into it), every kept page is positive
// and below the offset-overflow bound (a poisoned page must never survive
// into the offset multiplication), and one decode/encode round is a fixpoint
// whenever the checkpoint is representable (Pages non-empty, or a Last that
// does not itself look like JSON - the one legacy-form ambiguity, unreachable
// for production "scope:alID" cursors).
func FuzzHarvestCheckpointCodec(f *testing.F) {
	f.Add("")
	f.Add("nyaa:1500")
	f.Add(`{"last":"nyaa:7","pages":{"nyaa:7":3}}`)
	f.Add(`{"last":"ab:9","pages":{"nyaa:7":0,"ab:3":-2,"nyaa:9":4}}`)
	f.Add(`{"pages": {"nyaa:7": `)
	f.Add(`{"last":"{sneaky"}`)
	f.Add(`{"pages":{"nyaa:7":92233720368547758}}`)
	f.Add("  {not json")
	f.Fuzz(func(t *testing.T, raw string) {
		cp := decodeHarvestCheckpoint(raw)
		if cp.Pages == nil {
			t.Fatalf("decodeHarvestCheckpoint(%q).Pages = nil, want an allocated map", raw)
		}
		maxSafePage := math.MaxInt/harvestPageSize - (harvestShowPageCap - 1)
		for key, page := range cp.Pages {
			if page <= 0 || page > maxSafePage {
				t.Fatalf("decodeHarvestCheckpoint(%q) kept page %q=%d outside (0, %d]",
					raw, key, page, maxSafePage)
			}
		}
		if len(cp.Pages) > 0 || !strings.HasPrefix(strings.TrimSpace(cp.Last), "{") {
			again := decodeHarvestCheckpoint(encodeHarvestCheckpoint(cp))
			if again.Last != cp.Last || !maps.Equal(again.Pages, cp.Pages) {
				t.Fatalf("codec not a fixpoint: decode(%q) = %+v, re-round-trip gives %+v", raw, cp, again)
			}
		}
	})
}

// FuzzMatchHarvest_cacheHygiene exercises the harvest's untrusted-response
// boundary: matchHarvest consumes Prowlarr-supplied titles, page URLs, and
// info hashes, and writes into the titles cache that is persisted verbatim
// into the snapshot and rendered into every RSS response. Invariants of the
// cache-admission contract: the returned count equals the number of NEW
// entries; an already-cached title is never overwritten or dropped (torrents
// are immutable, so the first harvested title stands); every admitted key is
// a pending journal key of the QUERIED scope (a foreign, cross-scope, or
// contradictory identity titles nothing); and every admitted value is exactly
// the trimmed upstream title, non-empty and within harvestMaxTitleLen. The
// second arm is the two-sided oracle: on a result whose identity is the
// canonical page URL of an indexed key, the title is admitted exactly when it
// is non-blank and within the bound.
func FuzzMatchHarvest_cacheHygiene(f *testing.F) {
	f.Add("Show S01 1080p BluRay [G]", "https://nyaa.si/view/42", "https://nyaa.si/view/42", "")
	f.Add("Tampered", "https://nyaa.si/view/42", "https://nyaa.si/view/43", "")
	f.Add("Cross scope", "https://animebytes.tv/torrent/300/group", "https://animebytes.tv/torrent/300/group", "")
	f.Add("   ", "https://nyaa.si/view/42", "", "")
	f.Add(strings.Repeat("A", harvestMaxTitleLen), "https://nyaa.si/view/42", "", "")
	f.Add(strings.Repeat("A", harvestMaxTitleLen+1), "https://nyaa.si/view/42", "", "")
	f.Add("Hash matched", "https://mirror.example/x", "https://mirror.example/x", "143ed15e5e3df072ae91adaeb149973a887590dd")
	f.Add("Already harvested", "https://nyaa.si/view/43", "", "")
	f.Add("Unknown id", "https://nyaa.si/view/999", "", "")
	f.Fuzz(func(t *testing.T, title, infoURL, guid, hash string) {
		const knownHash = "143ed15e5e3df072ae91adaeb149973a887590dd"
		index := map[string]string{
			"nyaa:42": "nyaa:42",
			"nyaa:43": "nyaa:43",
			"ab:300":  "ab:300",
			knownHash: "nyaa:42",
		}
		pending := map[string]bool{"nyaa:42": true, "nyaa:43": true, "ab:300": true}
		titles := map[string]string{"nyaa:43": "Already Harvested"}
		before := maps.Clone(titles)

		n, _ := matchHarvest([]item{{Title: title, InfoURL: infoURL, GUID: guid, InfoHash: hash}},
			upstreamNyaa, index, titles, "")

		if len(titles) < len(before) {
			t.Fatalf("matchHarvest dropped cached titles: %v, had %v", titles, before)
		}
		added := 0
		for k, v := range titles {
			if old, cached := before[k]; cached {
				if v != old {
					t.Fatalf("matchHarvest overwrote cached title %q: %q -> %q", k, old, v)
				}
				continue
			}
			added++
			if !pending[k] || !strings.HasPrefix(k, upstreamNyaa+":") {
				t.Fatalf("matchHarvest admitted key %q (title %q), want a pending %s key", k, v, upstreamNyaa)
			}
			if want := strings.TrimSpace(title); v != want {
				t.Fatalf("matchHarvest cached %q for key %q, want the trimmed upstream title %q", v, k, want)
			}
			if v == "" || len(v) > harvestMaxTitleLen {
				t.Fatalf("matchHarvest cached an out-of-contract title for %q: len %d", k, len(v))
			}
		}
		if added != n {
			t.Fatalf("matchHarvest = %d matches, but %d new titles entered the cache (%v)", n, added, titles)
		}

		trimmed := strings.TrimSpace(title)
		admissible := trimmed != "" && len(trimmed) <= harvestMaxTitleLen
		fresh := map[string]string{}
		got, _ := matchHarvest([]item{{Title: title, InfoURL: "https://nyaa.si/view/42"}}, upstreamNyaa, index, fresh, "")
		if admissible {
			if got != 1 || fresh["nyaa:42"] != trimmed {
				t.Fatalf("matchHarvest(canonical identity, title %q) = %d matches caching %q, want it admitted as %q",
					title, got, fresh["nyaa:42"], trimmed)
			}
		} else if got != 0 || len(fresh) != 0 {
			t.Fatalf("matchHarvest(canonical identity, blank/oversized title %q) = %d matches caching %v, want nothing admitted",
				title, got, fresh)
		}
	})
}

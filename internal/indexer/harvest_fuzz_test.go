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
		for key, page := range cp.Pages {
			if page <= 0 || page > math.MaxInt/harvestPageSize {
				t.Fatalf("decodeHarvestCheckpoint(%q) kept page %q=%d outside (0, %d]",
					raw, key, page, math.MaxInt/harvestPageSize)
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

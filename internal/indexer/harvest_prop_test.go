package indexer

import (
	"maps"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestMatchHarvestCacheHygieneProperty is the every-PR randomized
// complement to FuzzMatchHarvest_cacheHygiene. It exercises the persisted
// title-cache admission boundary across title limits, queried scopes,
// contradictory identities, unknown identities, hashes, and cached entries.
func TestMatchHarvestCacheHygieneProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		var title string
		switch rapid.IntRange(0, 4).Draw(t, "title kind") {
		case 0:
			title = rapid.StringMatching(`[A-Za-z0-9][A-Za-z0-9 ._]{0,79}`).Draw(t, "ordinary title")
		case 1:
			title = " \t "
		case 2:
			title = strings.Repeat("A", harvestMaxTitleLen)
		case 3:
			title = strings.Repeat("A", harvestMaxTitleLen+1)
		default:
			title = rapid.String().Draw(t, "arbitrary title")
		}

		const knownHash = "143ed15e5e3df072ae91adaeb149973a887590dd"
		identityKind := rapid.IntRange(0, 6).Draw(t, "identity kind")
		result := item{Title: title}
		switch identityKind {
		case 0:
			result.InfoURL = "https://nyaa.si/view/42"
		case 1:
			result.InfoURL = "https://nyaa.si/view/999"
		case 2:
			result.InfoURL = "https://animebytes.tv/torrent/300/group"
		case 3:
			result.InfoURL = "https://nyaa.si/view/42"
			result.GUID = "https://nyaa.si/view/43"
		case 4:
			result.InfoHash = knownHash
		case 5:
			result.InfoURL = "https://mirror.example/release"
		case 6:
			result.GUID = "https://nyaa.si/view/43"
		}

		index := map[string]string{
			"nyaa:42": "nyaa:42",
			"nyaa:43": "nyaa:43",
			"ab:300":  "ab:300",
			knownHash: "nyaa:42",
		}
		pending := map[string]bool{"nyaa:42": true, "nyaa:43": true, "ab:300": true}
		titles := map[string]string{"nyaa:43": "Already Harvested"}
		before := maps.Clone(titles)

		matched, rejected, pendingRejected := matchHarvest(
			[]item{result}, upstreamNyaa, index, titles, nil, []string{"nyaa:42", "nyaa:43"},
		)

		trimmed := strings.TrimSpace(title)
		admissible := trimmed != "" && len(trimmed) <= harvestMaxTitleLen
		wantMatched, wantRejected, wantPendingRejected := 0, 0, 0
		wantKey := ""
		if admissible {
			switch identityKind {
			case 0, 4:
				wantMatched, wantKey = 1, "nyaa:42"
			case 3:
				wantRejected, wantPendingRejected = 1, 1
			}
		}
		if matched != wantMatched || rejected != wantRejected || pendingRejected != wantPendingRejected {
			t.Fatalf("matchHarvest(kind=%d, title=%q) = (%d, %d, %d), want (%d, %d, %d)",
				identityKind, title, matched, rejected, pendingRejected,
				wantMatched, wantRejected, wantPendingRejected)
		}
		if pendingRejected > rejected {
			t.Fatalf("pending rejections %d exceed all rejections %d", pendingRejected, rejected)
		}
		if matched > 0 && pendingRejected > 0 {
			t.Fatalf("one result both matched %d times and caused %d pending rejections", matched, pendingRejected)
		}

		added := 0
		for key, gotTitle := range titles {
			if old, cached := before[key]; cached {
				if gotTitle != old {
					t.Fatalf("cached title %q changed from %q to %q", key, old, gotTitle)
				}
				continue
			}
			added++
			if !pending[key] || !strings.HasPrefix(key, upstreamNyaa+":") {
				t.Fatalf("admitted non-pending or cross-scope key %q", key)
			}
			if gotTitle != trimmed || gotTitle == "" || len(gotTitle) > harvestMaxTitleLen {
				t.Fatalf("admitted out-of-contract title %q for %q from %q", gotTitle, key, title)
			}
		}
		if added != matched {
			t.Fatalf("matched %d results but added %d cache entries", matched, added)
		}
		if wantKey != "" && titles[wantKey] != trimmed {
			t.Fatalf("title for %q = %q, want %q", wantKey, titles[wantKey], trimmed)
		}
	})
}

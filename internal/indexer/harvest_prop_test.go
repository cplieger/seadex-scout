package indexer

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/titlekey"
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

		matched, rejected, pendingRejected, _ := matchHarvest(
			[]item{result}, upstreamNyaa, index, titles, nil, []string{"nyaa:42", "nyaa:43"},
		)

		trimmed := strings.TrimSpace(title)
		admissible := trimmed != "" && len(trimmed) <= harvestMaxTitleLen
		wantMatched, wantRejected, wantPendingRejected := 0, 0, 0
		wantKey := ""
		switch identityKind {
		case 0, 4:
			if admissible {
				wantMatched, wantKey = 1, "nyaa:42"
			}
		case 3:
			// A contradictory identity is refused and counted whatever its
			// title carries: classification precedes the cache-admission gate,
			// so a tampered response cannot hide behind a blank or oversized
			// title.
			wantRejected, wantPendingRejected = 1, 1
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

// TestPreferredHarvestTitleYieldsAVocabularyCandidateProperty pins the two
// clauses of preferredHarvestTitle's contract across arbitrary alias sets: the
// winner is always ONE OF the candidates (matchHarvest caches the return value
// unchecked and never overwrites it, so a non-candidate or an empty string
// would be served for the item's whole journal window), and whenever any alias
// carries the show's own vocabulary the winner carries it too - never the
// ascii-count fallback.
func TestPreferredHarvestTitleYieldsAVocabularyCandidateProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// titlekey.Normalize is idempotent over [a-z0-9], so a key drawn in
		// that domain IS its own normalized form and needs no re-derivation.
		key := rapid.StringMatching(`[a-z0-9]{4,12}`).Draw(t, "normalized show title")
		candidates := rapid.SliceOfN(rapid.OneOf(
			rapid.Just("[Grp] "+strings.ToUpper(key)+" - S01 (BD Remux 1080p x265)"),
			rapid.Just("[Grp] \u846c\u9001\u306e\u30d5\u30ea\u30fc\u30ec\u30f3 - S01 (BD Remux 1080p x265)"),
			rapid.StringMatching(`\[Grp\] [A-Za-z]{1,10} - S0[1-9] \(BD 1080p\)`),
		), 1, 4).Draw(t, "aliases")
		showTitle := rapid.SampledFrom([]string{key, strings.ToUpper(key), "", "no such show at all"}).Draw(t, "show title")

		got := preferredHarvestTitle(candidates, showTitle)

		if !slices.Contains(candidates, got) {
			t.Fatalf("preferredHarvestTitle(%q, %q) = %q, want one of the candidates", candidates, showTitle, got)
		}
		if strings.ToLower(showTitle) != key {
			return
		}
		carries := false
		for _, c := range candidates {
			if titlekey.ContainsKey(c, key) {
				carries = true
				break
			}
		}
		if carries && !titlekey.ContainsKey(got, key) {
			t.Fatalf("preferredHarvestTitle(%q, %q) = %q, want the alias carrying the show vocabulary %q",
				candidates, showTitle, got, key)
		}
	})
}

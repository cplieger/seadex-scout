package indexer

import (
	"net/url"
	"strconv"
	"testing"
)

// FuzzCurationLookup_neverAdmitsCrossWiredIdentity exercises the request-side
// curation gate with arbitrary Prowlarr-controlled identity signals (the info
// hash plus the two page URLs). Against a fixed two-torrent curation model, an
// admitted item must carry at least one CURATED signal (a hash the set knows,
// or a scoped tracker key), must not name two
// different tracker keys, must not key outside the served scope, and must not
// pair one torrent's curated hash with another torrent's curated key (the
// persisted byPair co-membership relation) - the cross-wiring an untrusted
// Torznab response would use to attach a curated best/alt marker to a
// different torrent's download link. An info hash the set does not know is
// deliberately NOT a signal in either direction: it neither admits an item on
// its own nor vetoes a curated tracker key beside it (l-f30). The oracle is
// the fixed model plus the independently fuzzed identity extractors, never a
// reimplementation of lookup's own policy.
func FuzzCurationLookup_neverAdmitsCrossWiredIdentity(f *testing.F) {
	const hashA = "143ed15e5e3df072ae91adaeb149973a887590dd"
	const hashB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	set := &curation{
		byHash: map[string]bool{hashA: true, hashB: true},
		byKey:  map[string]bool{"nyaa:111": true, "nyaa:222": true},
		byPair: map[string]bool{
			pairKey(hashA, "nyaa:111"): true,
			pairKey(hashB, "nyaa:222"): true,
		},
	}
	f.Add(hashA, "https://nyaa.si/view/111", "https://nyaa.si/view/111")
	f.Add(hashA, "https://nyaa.si/view/222", "https://nyaa.si/view/222")
	f.Add(hashA, "https://nyaa.si/view/111", "https://nyaa.si/view/222")
	// An info hash the set does not know, beside a curated key: admitted (the
	// hash corroborates nothing and vetoes nothing).
	f.Add("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "https://nyaa.si/view/111", "")
	f.Add("", "https://animebytes.tv/torrent/111/group", "")
	f.Add("", "https://evil.example/view/111", "")
	f.Add("<redacted>", "", "")
	f.Fuzz(func(t *testing.T, hash, infoURL, guid string) {
		isBest, matched, conflict := set.lookup(upstreamNyaa, hash, infoURL, guid)
		if !matched {
			if isBest {
				t.Fatalf("lookup(%q, %q, %q) = (isBest=true, matched=false), want isBest false when unmatched", hash, infoURL, guid)
			}
			return
		}
		if conflict {
			t.Fatalf("lookup(%q, %q, %q) reported a conflict on an ADMITTED item", hash, infoURL, guid)
		}
		h := validInfoHash(hash)
		// Only a hash the curation set KNOWS is an identity signal: an unknown
		// hash corroborates nothing and cannot veto a curated page URL, so it
		// neither satisfies the has-a-signal check nor needs a pair proven.
		_, curatedHash := set.byHash[h]
		k1, k2 := trackerKeyFromURL(infoURL), trackerKeyFromURL(guid)
		key := k1
		if key == "" {
			key = k2
		}
		if !curatedHash && key == "" {
			t.Fatalf("lookup(%q, %q, %q) matched with no curated identity signal at all", hash, infoURL, guid)
		}
		if k1 != "" && k2 != "" && k1 != k2 {
			t.Fatalf("lookup matched an item naming two different tracker keys (%q, %q)", k1, k2)
		}
		if key != "" && scopeOfKey(key) != upstreamNyaa {
			t.Fatalf("lookup matched out-of-scope key %q on the nyaa endpoint", key)
		}
		if curatedHash && key != "" && !set.byPair[pairKey(h, key)] {
			t.Fatalf("lookup matched cross-wired identity: hash %q with key %q (no persisted co-membership)", h, key)
		}
	})
}

// FuzzUpstreamParams_limitIsAlwaysTheDecoderWindow exercises the search
// proxy's forwarded-limit rule with arbitrary client-controlled limit values.
// Invariant: no client value reaches the upstream as a limit - the forwarded
// window is always maxItems, the caps-advertised bound parseTorznab rejects a
// response above. This pins h-f12's contract in both directions: an
// over-contract limit can never turn a search into a rejected fetch, and a
// small client limit (Sonarr sends 100) can never truncate the upstream page
// before curation filters it, which used to hide a curated release sitting
// past the client's page size.
func FuzzUpstreamParams_limitIsAlwaysTheDecoderWindow(f *testing.F) {
	f.Add("100")
	f.Add("1000")
	f.Add("1001")
	f.Add("99999999999999999999")
	f.Add("-99999999999999999999")
	f.Add("0")
	f.Add("abc")
	f.Add("")
	f.Add("  2000  ")
	f.Fuzz(func(t *testing.T, limit string) {
		out := upstreamParams(url.Values{"t": {"search"}, "q": {"x"}, "limit": {limit}})
		if got := out.Get("limit"); got != strconv.Itoa(maxItems) {
			t.Fatalf("upstreamParams forwarded limit %q for client limit %q, want %d",
				got, limit, maxItems)
		}
	})
}

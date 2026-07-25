package indexer

import "testing"

// FuzzCurationLookup_neverAdmitsCrossWiredIdentity exercises the request-side
// curation gate with arbitrary Prowlarr-controlled identity signals (the info
// hash plus the two page URLs). Against a fixed two-torrent curation model, an
// admitted item must carry at least one curated signal, must not name two
// different tracker keys, must not key outside the served scope, and must not
// pair one torrent's curated hash with another torrent's curated key (the
// persisted byPair co-membership relation) - the cross-wiring an untrusted
// Torznab response would use to attach a curated best/alt marker to a
// different torrent's download link. The oracle is the fixed model plus the
// independently fuzzed identity extractors, never a reimplementation of
// lookup's own policy.
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
	f.Add("", "https://animebytes.tv/torrent/111/group", "")
	f.Add("", "https://evil.example/view/111", "")
	f.Add("<redacted>", "", "")
	f.Fuzz(func(t *testing.T, hash, infoURL, guid string) {
		isBest, matched := set.lookup(upstreamNyaa, hash, infoURL, guid)
		if !matched {
			if isBest {
				t.Fatalf("lookup(%q, %q, %q) = (isBest=true, matched=false), want isBest false when unmatched", hash, infoURL, guid)
			}
			return
		}
		h := validInfoHash(hash)
		k1, k2 := trackerKeyFromURL(infoURL), trackerKeyFromURL(guid)
		key := k1
		if key == "" {
			key = k2
		}
		if h == "" && key == "" {
			t.Fatalf("lookup(%q, %q, %q) matched with no identity signal at all", hash, infoURL, guid)
		}
		if k1 != "" && k2 != "" && k1 != k2 {
			t.Fatalf("lookup matched an item naming two different tracker keys (%q, %q)", k1, k2)
		}
		if key != "" && scopeOfKey(key) != upstreamNyaa {
			t.Fatalf("lookup matched out-of-scope key %q on the nyaa endpoint", key)
		}
		if h != "" && key != "" && !set.byPair[pairKey(h, key)] {
			t.Fatalf("lookup matched cross-wired identity: hash %q with key %q (no persisted co-membership)", h, key)
		}
	})
}

package indexer

// This file holds the fixture vocabulary for the persisted feed contract, so a
// test says what it is about (this entry owns this release; this identity was
// published) rather than hand-assembling the store.

// fixtureOwner is the synthetic AniList id fixture ownership is attributed to
// when a test does not care WHICH entry owns a release - it cares that the
// release projects into the search index. A real pass attributes to the entry
// that listed the torrent (ownershipOf).
const fixtureOwner = 1

// owns builds a one-entry ownership fact for a fixture snapshot.
func owns(rs ...ownedRelease) map[string][]ownedRelease {
	if len(rs) == 0 {
		return map[string][]ownedRelease{}
	}
	return map[string][]ownedRelease{ownerKey(fixtureOwner): rs}
}

// ownsBy builds a fixture ownership fact attributed to a specific AniList id,
// for the tests where WHICH entry owns a release is the point (a shared torrent,
// a per-owner isBest vote, a window replacing one owner's contribution).
func ownsBy(alID int, rs ...ownedRelease) map[string][]ownedRelease {
	return map[string][]ownedRelease{ownerKey(alID): rs}
}

// mergeOwners unions several ownership facts into one, so a fixture can compose
// per-entry contributions the way a catalogue pass does.
func mergeOwners(sets ...map[string][]ownedRelease) map[string][]ownedRelease {
	out := map[string][]ownedRelease{}
	for _, set := range sets {
		for id, rs := range set {
			out[id] = append(out[id], rs...)
		}
	}
	return out
}

// keyed is the common fixture release: a tracker key with no info hash (the
// AnimeBytes shape, and the shape most journal fixtures use).
func keyed(key string, isBest bool) ownedRelease {
	return ownedRelease{Key: key, IsBest: isBest}
}

// hashed is a fixture release identified by both signals, which is what makes
// the pair relation non-empty (a healthy Nyaa record).
func hashed(key, hash string, isBest bool) ownedRelease {
	return ownedRelease{Key: key, Hash: hash, IsBest: isBest}
}

// byHashOf / byKeyOf / byPairOf read the DERIVED search index off a persisted
// snapshot, which is how every assertion about search membership has to be
// expressed now: the maps are a projection of the ownership fact, not a
// persisted member (see projectCuration).
func byHashOf(snap *snapshot) map[string]bool { return projectCuration(snap.Owners).byHash }
func byKeyOf(snap *snapshot) map[string]bool  { return projectCuration(snap.Owners).byKey }
func byPairOf(snap *snapshot) map[string]bool { return projectCuration(snap.Owners).byPair }

// publishedSignals is the publication log a fixture seeds when it wants a
// release treated as already served.
func publishedSignals(ids ...string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

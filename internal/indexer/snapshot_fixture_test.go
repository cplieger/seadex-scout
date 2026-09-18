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
// persisted member (see projectCuration). The two identity maps are flattened to
// their best/alt vote, so a test about membership says nothing about the tvdb id
// (bestVotes is the inverse, for a hand-built projection).
func byHashOf(snap *snapshot) map[string]bool {
	return bestVoteOf(projectCuration(snap.Owners).byHash)
}

func byKeyOf(snap *snapshot) map[string]bool {
	return bestVoteOf(projectCuration(snap.Owners).byKey)
}

func byPairOf(snap *snapshot) map[string]bool { return projectCuration(snap.Owners).byPair }

// bestVoteOf flattens a projected identity map to its best/alt vote per signal.
func bestVoteOf(signals map[string]curatedSignal) map[string]bool {
	out := make(map[string]bool, len(signals))
	for id, sig := range signals {
		out[id] = sig.isBest
	}
	return out
}

// bestVotes builds a hand-written projection from per-signal best/alt votes, for
// the many curation fixtures that predate the tvdb id and are not about it.
func bestVotes(votes map[string]bool) map[string]curatedSignal {
	out := make(map[string]curatedSignal, len(votes))
	for id, best := range votes {
		out[id] = curatedSignal{isBest: best}
	}
	return out
}

// noInfo is the entry-info lookup for a fixture that carries no mapping facts:
// ownershipOf reads each entry's TVDB id through one, and production always hands
// it a non-nil func (entryInfoFunc).
func noInfo(int) EntryInfo { return EntryInfo{} }

// signalWithID is a hand-written projected signal carrying one holder's tvdb id,
// for the search-render fixtures that ARE about it.
func signalWithID(isBest bool, ids ...int) curatedSignal {
	sig := curatedSignal{isBest: isBest}
	for _, id := range ids {
		sig.vote.add(id)
	}
	return sig
}

// publishedSignals is the publication log a fixture seeds when it wants a
// release treated as already served.
func publishedSignals(ids ...string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

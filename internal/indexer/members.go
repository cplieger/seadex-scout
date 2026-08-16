package indexer

import (
	"maps"
	"strconv"
)

// This file is the persisted feed contract: ONE store, TWO facts, TWO
// projections, and one FIXED write rule per member.

// currentFeedVersion is the persisted feed.json schema version. It exists because
// the file carries members that ACCUMULATE state and cannot be re-derived from the
// catalogue: the publication log is permanent by design, and the journal renders
// carry FirstSeen plus harvested titles no SeaDex record holds. An older binary
// silently drops unknown fields and rewrites the file, which for those members is
// irreversible. A snapshot at any other version RE-BASELINES rather than being
// refused: the cost is one empty-RSS window, against a permanently mis-recorded
// publication log.
const currentFeedVersion = 2

// snapshotMember is one persisted top-level feed.json member. The values are the
// on-disk JSON keys, so the decoder's vocabulary and the file's are literally the
// same strings. Each member names the ONE fixed write rule it is written by, and
// the UNIT that rule's "what you evaluated" is measured in - naming the unit is
// what lets one rule cover both an entry-scoped and an item-scoped member.
type snapshotMember string

const (
	// memberVersion is the ENVELOPE, measured per snapshot: the schema version
	// itself, stamped by the writer and never carried from the loaded file. It is
	// not a fact about the catalogue.
	memberVersion snapshotMember = "version"
	// memberOwners is the PRESENT fact, measured per ENTRY: for every entry the
	// pass EVALUATED, that entry's contribution is set to what the pass observed.
	// Only a catalogue pass may DELETE an entry, because absence from a window
	// proves nothing (see upsertOwners).
	memberOwners snapshotMember = "owners"
	// memberPublished is the PAST fact, measured per RELEASE IDENTITY: recorded
	// when the fact occurs, never rewritten and never deleted at any scope. You
	// cannot un-serve something (see appendPublished).
	memberPublished snapshotMember = "published"
	// memberTitles CARRIES VALIDATED, measured per JOURNAL KEY: the form the
	// loader already VOUCHED rather than the raw decoded one, which is what stops
	// a value the ingress gate refused being re-persisted by every pass that does
	// not own it.
	memberTitles snapshotMember = "titles"
	// memberHarvestCursor CARRIES VALIDATED, measured per snapshot: the vouched
	// harvest rotation position, for the same reason as memberTitles.
	memberHarvestCursor snapshotMember = "harvest_cursor"
	// memberNyaaFeed is the MATERIALIZED PAST, measured per ITEM: append plus an
	// age-out whose criterion is the item's OWN FirstSeen rather than membership
	// of the pass's input, which is exactly why deleting from it is sound at ANY
	// scope.
	memberNyaaFeed snapshotMember = "nyaa_feed"
	// memberABFeed is the MATERIALIZED PAST, measured per ITEM: append plus an
	// age-out on the item's own FirstSeen, sound at any scope for the same reason
	// as memberNyaaFeed.
	memberABFeed snapshotMember = "ab_feed"
)

// allSnapshotMembers is the canonical member order: the order the decoder
// recognizes keys in, and the order buildSnapshot writes them in. It is the
// closed set the totality test compares the snapshot struct against.
var allSnapshotMembers = [...]snapshotMember{
	memberVersion, memberOwners, memberPublished,
	memberTitles, memberHarvestCursor, memberNyaaFeed, memberABFeed,
}

// passScope is the INPUT a pass holds, and it is the only difference between the
// reconcile and the tick: one pass, two scopes, the same member rules. scopeNever
// and scopeAny are not passes - they are the deletion authority a member's rule
// grants, in the same vocabulary.
type passScope int

const (
	// scopeCatalogue is the reconcile: the input IS the whole SeaDex catalogue, so
	// absence from it genuinely is absence from SeaDex and a DELETE is authorized.
	scopeCatalogue passScope = iota + 1
	// scopeWindow is the tick: the input is the recently-changed entries. An
	// empty window is legitimate, so absence from it proves nothing.
	scopeWindow
	// scopeNever authorizes no deletion at all (the publication log).
	scopeNever
	// scopeAny authorizes deletion from either pass, because the criterion is the
	// item's own fields rather than membership of the input (the journal age-out).
	scopeAny
)

// String returns the scope's name for a log line or a test failure.
func (s passScope) String() string {
	switch s {
	case scopeCatalogue:
		return "catalogue"
	case scopeWindow:
		return "window"
	case scopeNever:
		return "never"
	case scopeAny:
		return "any"
	}
	return "invalid"
}

// ownedRelease is one release an AniList entry contributes to the curation index:
// its tracker key, its info hash, and whether that entry votes the release BEST.
// Persisting the vote PER OWNER is what makes the isBest fold across owners
// recomputable exactly, so a best-to-alt demotion is representable from a window
// and the shared-torrent case (4.4% of torrents have several entries) is solved.
type ownedRelease struct {
	Key    string `json:"key,omitempty"`
	Hash   string `json:"hash,omitempty"`
	IsBest bool   `json:"best,omitempty"`
}

// ownerKey is an AniList entry id in its persisted map-key form: JSON object keys
// are strings, so the id is written as its decimal spelling.
func ownerKey(alID int) string { return strconv.Itoa(alID) }

// projectCuration derives the three search maps from the owner-keyed ownership
// fact. This is the PROJECTION half of "persist the fact, derive the projection":
// the maps are never persisted, so they cannot drift from the fact or be tampered
// with independently of it. The isBest fold is an OR across every owner, which is
// the same catalogue-scoped semantics the old persisted maps carried - only
// recomputed from the votes rather than accumulated destructively, which is what
// makes a demotion expressible. All three maps are always allocated, so the
// co-membership relation byPair records is never absent.
func projectCuration(owners map[string][]ownedRelease) curation {
	set := curation{
		byHash: make(map[string]bool, len(owners)),
		byKey:  make(map[string]bool, len(owners)),
		byPair: make(map[string]bool, len(owners)),
	}
	for _, releases := range owners {
		for _, r := range releases {
			if r.Hash != "" {
				set.byHash[r.Hash] = set.byHash[r.Hash] || r.IsBest
			}
			if r.Key != "" {
				set.byKey[r.Key] = set.byKey[r.Key] || r.IsBest
			}
			if r.Hash != "" && r.Key != "" {
				set.byPair[pairKey(r.Hash, r.Key)] = true
			}
		}
	}
	return set
}

// upsertOwners applies the PRESENT-fact rule to one pass's evaluation: every entry
// the pass evaluated has its contribution set to what the pass observed, and an
// entry it did NOT evaluate keeps its stored contribution unless the pass's scope
// authorizes a delete. At catalogue scope the evaluated set IS the catalogue, so
// that coincides with wholesale replacement; at window scope an out-of-window
// entry is untouched, because absence from a window is not evidence.
func upsertOwners(prev, evaluated map[string][]ownedRelease, scope passScope) map[string][]ownedRelease {
	out := make(map[string][]ownedRelease, len(evaluated))
	if scope != scopeCatalogue {
		maps.Copy(out, prev)
	}
	for id, releases := range evaluated {
		if len(releases) == 0 {
			// An entry the pass evaluated down to nothing contributes nothing: an empty
			// list would persist a member the projection cannot use.
			delete(out, id)
			continue
		}
		out[id] = releases
	}
	return out
}

// passWrites is what one pass PRODUCED, member by member. It exists so the persist
// path walks the members applying each rule instead of mutating a few and
// implicitly carrying the rest - the shape that let the tick get five members
// wrong by omission.
type passWrites struct {
	// evaluated is the ownership contribution of every entry this pass
	// evaluated, keyed by ownerKey.
	evaluated map[string][]ownedRelease
	// published is the identities that ENTERED a feed on this pass, plus the two
	// deliberate FORFEITURES that write it without publishing: the fresh-journal
	// baseline (so a new install starts quiet instead of broadcasting everything
	// SeaDex already lists) and a DISABLED scope, whose curated releases are
	// forfeited as they are seen so re-enabling cannot re-broadcast its catalogue.
	published map[string]bool
	// titles is the validated harvested-title cache, advanced by whatever this
	// pass's harvest earned.
	titles map[string]string
	// cursor is the validated harvest rotation position.
	cursor string
	// nyaa and ab are the journals after carry, age-out and growth.
	nyaa, ab []journalItem
	scope    passScope
}

// buildSnapshot folds one pass's writes onto the previous state, member by member
// in the canonical order, and is the only constructor of a persisted snapshot.
func buildSnapshot(prev *feedState, w *passWrites) (snapshot, error) {
	return snapshot{
		Version:       currentFeedVersion,
		Owners:        upsertOwners(prev.owners, w.evaluated, w.scope),
		Published:     appendPublished(prev.published, w.published),
		Titles:        w.titles,
		HarvestCursor: w.cursor,
		NyaaFeed:      w.nyaa,
		ABFeed:        w.ab,
	}, nil
}

// appendPublished applies the PAST-fact rule: the union of what was already
// recorded and what this pass published. Nothing is ever removed, so the only
// bound is the decode's cardinality cap and the catalogue re-derivation
// loadPrevious prescribes when it is crossed.
func appendPublished(prev, added map[string]bool) map[string]bool {
	out := make(map[string]bool, len(prev)+len(added))
	for id := range prev {
		out[id] = true
	}
	for id := range added {
		out[id] = true
	}
	return out
}

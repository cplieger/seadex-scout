package indexer

import (
	"fmt"
	"maps"
	"strconv"
)

// This file is the persisted feed contract: ONE store, TWO facts, TWO
// projections, and one FIXED write rule per member.
//
// The two facts are about different TIMELINES, which is why they cannot be one
// projection of one fact:
//
//   - FACT 1, the PRESENT: per-entry curation ownership (an AniList entry maps
//     to the set of {key, info hash, isBest} it contributes). It answers
//     is-this-curated-NOW, and it PROJECTS to the three search maps
//     (byHash/byKey/byPair), derived in memory on load rather than persisted.
//   - FACT 2, the PAST: the publication log - what was actually SERVED. It
//     answers what-is-new-since-you-last-looked, and it projects to the
//     novelty test plus, with the materialized renders, the two RSS journals.
//     The renders stay materialized because they are not reproducible from the
//     catalogue: a harvested Prowlarr title is not in the SeaDex record at all.
//
// WHICH RULE A MEMBER HAS FOLLOWS FROM WHICH FACT IT RECORDS - a fact about
// the present is upsert-what-you-evaluated, a fact about the past is
// append-only. It is NOT an append-or-replace choice made at write time, and
// that is the whole point: the rule is a PROPERTY OF THE MEMBER, so a
// rule-less field is impossible to add. buildSnapshot walks the members and
// REFUSES one with no rule; the totality test
// (TestEverySnapshotMemberHasAWriteRule) fails the gate for a member added to
// the struct but not to the table. Where each rule is APPLIED differs: the
// envelope, the ownership upsert and the publication append happen inside
// buildSnapshot, while the journal age-out and the two carried members are
// applied by the pass that produces them (pass.go's runPass), because they
// need inputs buildSnapshot does not take - the previous journals and the
// pass clock for the age-out, the harvest's earned titles for the carry.
//
// THE ONE LAW ALL FOUR RULES ARE CONSEQUENCES OF: a pass may act on evidence
// it HOLDS - the item's own fields, or the entries it actually evaluated - and
// may never act on ABSENCE from its own input unless that input is the whole
// catalogue. Every rule below is derivable from that sentence, and it is what
// a new member is forced to answer.

// currentFeedVersion is the persisted feed.json schema version. It exists
// because the file now carries members that ACCUMULATE state and cannot be
// re-derived from the catalogue: the publication log is permanent by design,
// and the journal renders carry FirstSeen plus harvested titles that no SeaDex
// record holds. An older binary silently drops unknown fields and rewrites the
// file, which for those members is irreversible.
//
// A snapshot at any other version RE-BASELINES rather than being refused (the
// settled no-rollback-no-migration call: this app supports no migration, and
// the release that introduces a version wipes the file). The cost of a
// re-baseline is one empty-RSS window, which is the intended fresh-install
// behaviour; the cost of a silent misread is a permanently mis-recorded
// publication log.
const currentFeedVersion = 2

// snapshotMember is one persisted top-level feed.json member. The values are
// the on-disk JSON keys, so the vocabulary the decoder dispatches on, the
// vocabulary the rule table is keyed by, and the vocabulary the file uses are
// literally the same strings.
type snapshotMember string

const (
	memberVersion       snapshotMember = "version"
	memberOwners        snapshotMember = "owners"
	memberPublished     snapshotMember = "published"
	memberTitles        snapshotMember = "titles"
	memberHarvestCursor snapshotMember = "harvest_cursor"
	memberNyaaFeed      snapshotMember = "nyaa_feed"
	memberABFeed        snapshotMember = "ab_feed"
)

// allSnapshotMembers is the canonical member order: the order the decoder
// recognizes keys in, and the order buildSnapshot applies rules in. It is the
// closed set the totality test compares the snapshot struct against.
var allSnapshotMembers = [...]snapshotMember{
	memberVersion, memberOwners, memberPublished,
	memberTitles, memberHarvestCursor, memberNyaaFeed, memberABFeed,
}

// writeRule is the one fixed rule a persisted member is written by. There are
// four, plus the envelope, and no member may have two.
type writeRule int

const (
	// ruleEnvelope is the schema version itself: stamped by the writer, never
	// carried from the loaded file. It is not a fact about the catalogue.
	ruleEnvelope writeRule = iota + 1
	// ruleUpsertEvaluated is the PRESENT-fact rule: for every unit the pass
	// EVALUATED, set that unit's contribution to what the pass observed. An
	// append is not a separate rule - it is the case where the unit's prior
	// contribution was empty, which is why a brand-new anime entry and a
	// changed one are the same write. DELETE of a unit is authorized only at
	// catalogue scope, because only there is absence from the input evidence
	// of absence from SeaDex.
	ruleUpsertEvaluated
	// ruleAppendOnly is the PAST-fact rule: recorded when the fact occurs,
	// never rewritten and never deleted. You cannot un-serve something.
	ruleAppendOnly
	// ruleAppendAndAgeOut is the MATERIALIZED PAST: append plus an age-out
	// whose criterion is the item's OWN FirstSeen rather than membership of
	// the pass's input, which is exactly why it is sound at ANY scope.
	ruleAppendAndAgeOut
	// ruleCarryValidated carries the form the loader already VOUCHED, not the
	// raw decoded one. It is what stops a value the ingress gate refused being
	// re-persisted by every pass that does not own it.
	ruleCarryValidated
)

// memberRule is one member's entry in the rule table: the rule, the UNIT the
// rule's "what you evaluated" is measured in (naming the unit is what lets ONE
// rule cover both an entry-scoped and an item-scoped member), and whether a
// deletion is authorized from a window.
//
// It deliberately carries no reversibility column: how long a wrong write
// survives differs per member (a wrong search key self-heals within one
// reconcile; a wrong journal item costs feedJournalMaxAge of a served release
// plus a permanent publication record), and that asymmetry is stated where it
// is read - the rule and deleteScope columns below, and the snapshotRules
// comment - rather than as a field nothing consults.
type memberRule struct {
	unit        string
	rule        writeRule
	deleteScope passScope
}

// snapshotRules is THE rule table. Every member of snapshot must appear here.
//
// curation ownership (present fact): UPSERT per evaluated entry, at ANY scope;
// DELETE an owner only at catalogue scope. This is what makes Rebuild's
// wholesale replacement need no special case - wholesale replacement IS
// upsert-everything-plus-delete-what-is-missing, expressed in the same
// vocabulary as the tick.
//
// publication log (past fact): APPEND only, at any scope.
//
// journal renders (materialized past): APPEND plus AGE-OUT, sound at any scope
// because the criterion is the item's own FirstSeen.
//
// Titles / HarvestCursor: carry the validated form. Note the asymmetry that is
// NOT in this table and is deliberate: retainTitles (pruning the harvest cache
// to the keys still journaled) is catalogue-scoped, because it reads the whole
// journal against the whole catalogue.
// The UNITS a rule's "what you evaluated" is measured in. Naming the unit is
// what lets ONE rule cover two members, so the vocabulary is closed and shared
// rather than spelled at each entry.
const (
	unitSnapshot = "snapshot"
	unitEntry    = "entry"
	unitIdentity = "release identity"
	unitItem     = "item"
	unitKey      = "journal key"
)

var snapshotRules = map[snapshotMember]memberRule{
	memberVersion: {
		rule: ruleEnvelope, unit: unitSnapshot,
		deleteScope: scopeCatalogue,
	},
	memberOwners: {
		rule: ruleUpsertEvaluated, unit: unitEntry,
		deleteScope: scopeCatalogue,
	},
	memberPublished: {
		rule: ruleAppendOnly, unit: unitIdentity,
		deleteScope: scopeNever,
	},
	memberNyaaFeed: {
		rule: ruleAppendAndAgeOut, unit: unitItem,
		deleteScope: scopeAny,
	},
	memberABFeed: {
		rule: ruleAppendAndAgeOut, unit: unitItem,
		deleteScope: scopeAny,
	},
	memberTitles: {
		rule: ruleCarryValidated, unit: unitKey,
		deleteScope: scopeCatalogue,
	},
	memberHarvestCursor: {
		rule: ruleCarryValidated, unit: unitSnapshot,
		deleteScope: scopeCatalogue,
	},
}

// passScope is the INPUT a pass holds, and it is the only difference between
// the reconcile and the tick: one pass, two scopes, the same member rules.
// scopeNever and scopeAny are not passes - they are the deletion authority a
// member's rule grants, expressed in the same vocabulary.
type passScope int

const (
	// scopeCatalogue is the reconcile: the input IS the whole SeaDex
	// catalogue, so absence from the input genuinely is absence from SeaDex
	// and a DELETE is authorized.
	scopeCatalogue passScope = iota + 1
	// scopeWindow is the tick: the input is the recently-changed entries. An
	// empty window is legitimate, so absence from it proves nothing.
	scopeWindow
	// scopeNever authorizes no deletion at all (the publication log).
	scopeNever
	// scopeAny authorizes deletion from either pass, because the criterion is
	// the item's own fields rather than membership of the input (the journal
	// age-out).
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

// authorizesDelete reports whether a pass at scope may DELETE a unit of a
// member whose rule grants deletion at grant. It is the one place the law is
// evaluated: a window may delete only where the criterion is the item's own
// evidence (scopeAny), never where the criterion is absence from the input.
func (s passScope) authorizesDelete(grant passScope) bool {
	switch grant {
	case scopeAny:
		return true
	case scopeCatalogue:
		return s == scopeCatalogue
	case scopeNever, scopeWindow:
		return false
	}
	return false
}

// --- Fact 1: per-entry curation ownership ---

// ownedRelease is one release an AniList entry contributes to the curation
// index: its tracker key, its info hash, and whether that entry votes the
// release BEST. Persisting the vote PER OWNER is what makes the isBest fold
// across owners recomputable exactly - drop this entry's votes, keep the other
// owners', recompute - so a best-to-alt demotion is representable from a
// window and the shared-torrent problem (4.4% of torrents are attached to
// several entries) is solved rather than worked around.
type ownedRelease struct {
	Key    string `json:"key,omitempty"`
	Hash   string `json:"hash,omitempty"`
	IsBest bool   `json:"best,omitempty"`
}

// ownerKey is an AniList entry id in its persisted map-key form. JSON object
// keys are strings, so the id is written as its decimal spelling.
func ownerKey(alID int) string { return strconv.Itoa(alID) }

// projectCuration derives the three search maps from the owner-keyed
// ownership fact. This is the PROJECTION half of "persist the fact, derive the
// projection": the maps are never persisted, so they cannot drift from the
// fact and cannot be tampered with independently of it.
//
// The isBest fold is an OR across every owner of a release, which is the
// catalogue-scoped semantics the old persisted by_hash/by_key carried - only
// now it is recomputed from the votes rather than accumulated destructively,
// which is what makes a demotion expressible. byPair records which
// (hash, key) combinations were observed on ONE AND THE SAME release, the
// cross-torrent gate lookup reads.
//
// All three maps are always allocated, including for an empty ownership fact:
// a nil byPair is what lookup treats as the absent co-membership relation, and
// deriving the relation from the fact means it is never absent.
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

// upsertOwners applies the PRESENT-fact rule to one pass's evaluation:
// every entry the pass evaluated has its contribution set to what the pass
// observed, and an entry the pass did NOT evaluate keeps its stored
// contribution unless the pass's scope authorizes a delete.
//
// At catalogue scope the evaluated set IS the catalogue, so "keep what was not
// evaluated" and "delete what is missing" coincide with wholesale replacement -
// which is why the reconcile needs no branch of its own here. At window scope
// the stored contribution of an out-of-window entry is untouched, because
// absence from a window is not evidence.
func upsertOwners(prev, evaluated map[string][]ownedRelease, scope passScope) map[string][]ownedRelease {
	out := make(map[string][]ownedRelease, len(evaluated))
	if !scope.authorizesDelete(snapshotRules[memberOwners].deleteScope) {
		maps.Copy(out, prev)
	}
	for id, releases := range evaluated {
		if len(releases) == 0 {
			// An entry the pass evaluated down to nothing contributes nothing.
			// Recording an empty list would persist a member the projection
			// cannot use and the decode budget still has to charge.
			delete(out, id)
			continue
		}
		out[id] = releases
	}
	return out
}

// --- The rule-driven write ---

// passWrites is what one pass PRODUCED, member by member. It exists so the
// persist path walks the members applying each rule instead of mutating a few
// and implicitly carrying the rest - the shape that let the tick get five
// members wrong by omission.
type passWrites struct {
	// evaluated is the ownership contribution of every entry this pass
	// evaluated, keyed by ownerKey.
	evaluated map[string][]ownedRelease
	// published is the identities that ENTERED a feed on this pass, plus the two
	// deliberate FORFEITURES that write it without publishing anything: the
	// fresh-journal baseline (the whole currently-curated catalogue, so a new
	// install starts quiet instead of broadcasting everything SeaDex already
	// lists), and a DISABLED scope, whose curated releases are forfeited as they
	// are seen (journalIfNew) so re-enabling a tracker cannot re-broadcast its
	// catalogue. Both are the operator's own switch acting, which is why they are
	// forfeitures rather than refusals - contrast a failed render on an ENABLED
	// tracker, which records nothing so a corrected upstream record can still
	// publish. The member's name is therefore narrower than its contents, and
	// that is settled rather than accidental: see `off-tracker-rss-backfill` in
	// this app's steering doc for the user's ruling and the reasoning.
	// Append-only.
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

// buildSnapshot folds one pass's writes onto the previous state, member by
// member in the canonical order, and is the only constructor of a persisted
// snapshot.
//
// It APPLIES three rules itself: ruleEnvelope (the version stamp),
// ruleUpsertEvaluated (upsertOwners, the one reader of a member's
// deleteScope) and ruleAppendOnly (appendPublished). ruleAppendAndAgeOut and
// ruleCarryValidated are applied by runPass before it calls here, so those
// four members are taken from w verbatim - a NEW member with either rule
// therefore needs its application wired in pass.go, not just a write below.
//
// It errors when a member has no rule. That is the totality gate at runtime;
// TestEverySnapshotMemberHasAWriteRule is the same gate at build time, and it
// is the reason "a member added without a rule must FAIL" is a property of the
// code rather than a convention someone has to remember.
func buildSnapshot(prev *feedState, w *passWrites) (snapshot, error) {
	var snap snapshot
	for _, member := range allSnapshotMembers {
		rule, known := snapshotRules[member]
		if !known {
			return snapshot{}, fmt.Errorf("indexer: persisted member %q has no write rule", member)
		}
		switch member {
		case memberVersion:
			snap.Version = currentFeedVersion
		case memberOwners:
			snap.Owners = upsertOwners(prev.owners, w.evaluated, w.scope)
		case memberPublished:
			snap.Published = appendPublished(prev.published, w.published)
		case memberNyaaFeed:
			snap.NyaaFeed = w.nyaa
		case memberABFeed:
			snap.ABFeed = w.ab
		case memberTitles:
			snap.Titles = w.titles
		case memberHarvestCursor:
			snap.HarvestCursor = w.cursor
		default:
			// Unreachable for a member in allSnapshotMembers, and that is the
			// point: a member added to the vocabulary and the table but not
			// given a write here cannot be persisted at all.
			return snapshot{}, fmt.Errorf("indexer: persisted member %q (rule %d) has no write", member, rule.rule)
		}
	}
	return snap, nil
}

// appendPublished applies the PAST-fact rule: the union of what was already
// recorded and what this pass published. Nothing is ever removed - you cannot
// un-serve something - so the only bound on it is the decode's cardinality cap
// and the catalogue re-derivation loadPrevious prescribes when it is crossed.
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

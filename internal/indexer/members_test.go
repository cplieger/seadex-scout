package indexer

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestEverySnapshotMemberIsInTheDecoderVocabulary is the TOTALITY GATE. The
// persisted feed contract's central promise is that a member the decoder cannot
// recognize is impossible to add, and Go cannot express that at compile time for
// a struct field, so it is expressed here instead: this test fails the build gate
// the moment a member is added to the snapshot struct without entering
// allSnapshotMembers, and the moment that list names a member the struct does not
// have.
//
// It walks the struct's JSON tags rather than a hand-written list on purpose. A
// hand-written list is the thing that drifts, and drift is the exact failure the
// vocabulary exists to prevent - the tick got five members wrong by omission
// because nothing forced it to answer for each one.
func TestEverySnapshotMemberIsInTheDecoderVocabulary(t *testing.T) {
	structMembers := map[snapshotMember]struct{}{}
	for field := range reflect.TypeFor[snapshot]().Fields() {
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			t.Errorf("snapshot field %s has no json tag; a persisted member must name its on-disk key", field.Name)
			continue
		}
		structMembers[snapshotMember(tag)] = struct{}{}
	}

	for member := range structMembers {
		if !slices.Contains(allSnapshotMembers[:], member) {
			t.Errorf("persisted member %q is not in allSnapshotMembers, so the decoder will never recognize it", member)
		}
	}
	for _, member := range allSnapshotMembers {
		if _, ok := structMembers[member]; !ok {
			t.Errorf("allSnapshotMembers names %q, which is not a field of snapshot", member)
		}
	}
}

// TestPublicationLogIsNeverDeletable is the one rule the app's correctness under
// a permanent record depends on: you cannot un-serve something, so nothing may
// ever delete from the publication log - at any scope. appendPublished is the
// log's only write, and it is a union.
func TestPublicationLogIsNeverDeletable(t *testing.T) {
	prev := publishedSignals("nyaa:1", "ab:2")
	want := publishedSignals("nyaa:1", "ab:2", "nyaa:3")
	if got := appendPublished(prev, publishedSignals("nyaa:3")); !maps.Equal(got, want) {
		t.Errorf("appendPublished(%v, {nyaa:3}) = %v, want %v", prev, got, want)
	}
	for _, scope := range []passScope{scopeCatalogue, scopeWindow} {
		t.Run(scope.String(), func(t *testing.T) {
			snap := buildSnapshot(&feedState{published: prev}, &passWrites{scope: scope})
			for id := range prev {
				if !snap.Published[id] {
					t.Errorf("a %v pass deleted %q from the publication log; it must never be able to", scope, id)
				}
			}
		})
	}
}

// TestPublicationLogCapRefusesTheWriteAndKeepsThePast pins the append-only rule
// where TestPublicationLogIsNeverDeletable cannot reach it: the rule table
// declares the log non-deletable, and the CAP path used to violate that
// declaration directly by assigning baselinePublications(current catalogue) over
// snap.Published at catalogue scope. That deleted every publication whose
// release had since left SeaDex, so a release that later returned read as new
// and was broadcast to the arrs a second time - the re-grab the permanent log
// exists to prevent.
//
// The contract now: an over-cap log fails the pass at BOTH scopes, the built
// snapshot is left untouched (so the last-good feed.json survives - run returns
// this error before persist), and recovery is an explicit operator re-baseline.
func TestPublicationLogCapRefusesTheWriteAndKeepsThePast(t *testing.T) {
	// One past publication that is NOT in any current catalogue, plus enough
	// per-entry-valid bulk to cross the aggregate byte cap (the only cap a test
	// can cross without materializing 250k entries).
	const pastIdentity = "nyaa:h:0f1e2d3c4b5a69788796a5b4c3d2e1f001020304"
	published := map[string]bool{pastIdentity: true}
	bulk := strings.Repeat("k", maxPersistedFieldBytes-8)
	for i := 0; len(published) < 1+(maxPublicationLogBytes/maxPersistedFieldBytes)+2; i++ {
		published[fmt.Sprintf("%s%05d", bulk, i)] = true
	}
	if publicationLogWithinLimits(published) {
		t.Fatalf("fixture publication log of %d entries is still within its caps; the cap path is unreachable", len(published))
	}
	for _, scope := range []passScope{scopeCatalogue, scopeWindow} {
		t.Run(scope.String(), func(t *testing.T) {
			snap := snapshot{Owners: owns(), Published: maps.Clone(published)}
			w := NewFeedWriter(&FeedWriterConfig{Path: filepath.Join(t.TempDir(), "feed.json")}, nil, nil)
			if err := w.publicationLogPersistable(&snap, scope); err == nil {
				t.Fatal("publicationLogPersistable = nil, want an error: an over-cap log must fail the pass rather than be rewritten")
			}
			if !snap.Published[pastIdentity] {
				t.Error("the cap path deleted a past publication; the log is a fact about what was SERVED and may never be rewritten")
			}
			if !maps.Equal(snap.Published, published) {
				t.Errorf("publication log mutated by the cap check: %d entries, want the %d it was handed", len(snap.Published), len(published))
			}
		})
	}
}

// TestOnlyCatalogueScopeDeletesAnOwner is the LAW, expressed on the member it
// bites hardest: a window may upsert what it evaluated and may never delete on
// absence from its own input.
func TestOnlyCatalogueScopeDeletesAnOwner(t *testing.T) {
	prev := ownsBy(10, keyed("nyaa:1", true))
	evaluated := ownsBy(20, keyed("nyaa:2", false))
	if _, still := upsertOwners(prev, evaluated, scopeCatalogue)[ownerKey(10)]; still {
		t.Error("the catalogue pass cannot delete an owner; wholesale replacement depends on it")
	}
	if _, still := upsertOwners(prev, evaluated, scopeWindow)[ownerKey(10)]; !still {
		t.Error("a window pass deleted an owner; absence from a window is not evidence")
	}
}

// TestUpsertOwnersIsAppendForANewEntryAndReplaceForAChangedOne pins the
// vocabulary correction the ratified note makes: add-a-new-anime and
// change-an-existing-anime's-torrents are the SAME write, because the map is
// keyed by OWNING ENTRY. An append is upsert with an empty prior.
func TestUpsertOwnersIsAppendForANewEntryAndReplaceForAChangedOne(t *testing.T) {
	prev := mergeOwners(
		ownsBy(10, keyed("nyaa:1", true)),
		ownsBy(20, keyed("nyaa:2", false)),
	)
	// A window evaluated entry 20 (its torrent changed) and entry 30 (brand new).
	evaluated := mergeOwners(
		ownsBy(20, keyed("nyaa:9", true)),
		ownsBy(30, keyed("nyaa:3", false)),
	)
	got := upsertOwners(prev, evaluated, scopeWindow)
	want := mergeOwners(
		ownsBy(10, keyed("nyaa:1", true)),  // untouched: not evaluated
		ownsBy(20, keyed("nyaa:9", true)),  // replaced wholesale
		ownsBy(30, keyed("nyaa:3", false)), // appended
	)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upsert at window scope = %v, want %v", got, want)
	}
}

// TestCatalogueUpsertDeletesAnEntryMissingFromItsInput: wholesale replacement IS
// upsert-everything-plus-delete-what-is-missing, so the reconcile needs no
// special case and keeps identical behaviour in the tick's vocabulary.
func TestCatalogueUpsertDeletesAnEntryMissingFromItsInput(t *testing.T) {
	prev := mergeOwners(ownsBy(10, keyed("nyaa:1", true)), ownsBy(20, keyed("nyaa:2", false)))
	evaluated := ownsBy(20, keyed("nyaa:2", false))
	got := upsertOwners(prev, evaluated, scopeCatalogue)
	if _, still := got[ownerKey(10)]; still {
		t.Error("entry 10 survived a catalogue pass that did not list it; the catalogue IS the evidence of absence")
	}
	if !reflect.DeepEqual(got, evaluated) {
		t.Errorf("catalogue upsert = %v, want exactly the evaluated set %v", got, evaluated)
	}
}

// TestPerOwnerVotesMakeADemotionRepresentable is the reason ownership is
// persisted per owner rather than as an OR-accumulated map. Two entries share a
// torrent, one votes it best; when that entry demotes it, the projection must
// recompute to alt. Under the old accumulated by_hash/by_key an `=` overwrite
// would have lost the other owner's vote and a `||` merge could never lower a
// stored true, which is why the note deletes the never-lower rule outright.
func TestPerOwnerVotesMakeADemotionRepresentable(t *testing.T) {
	const key, hash = "nyaa:42", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	owners := mergeOwners(
		ownsBy(10, hashed(key, hash, true)),
		ownsBy(20, hashed(key, hash, false)),
	)
	if !projectCuration(owners).byKey[key] {
		t.Fatal("a best vote from ANY owner must project as best")
	}
	// Entry 10 demotes it. A window that evaluated entry 10 replaces exactly
	// entry 10's contribution and leaves entry 20's alone.
	demoted := upsertOwners(owners, ownsBy(10, hashed(key, hash, false)), scopeWindow)
	set := projectCuration(demoted)
	if set.byKey[key] {
		t.Error("the release still projects as best after its only best-voting owner demoted it")
	}
	if !set.byKey[key] && !set.byPair[pairKey(hash, key)] {
		t.Error("the demoted release left the index entirely; a demotion changes the marker, not membership")
	}
}

// TestProjectionAlwaysAllocatesThePairRelation: the pair relation used to be a
// persisted map whose nil-ness was a legacy sentinel searches failed closed on,
// which cost an upgrade window of empty Nyaa results. Deriving it from the
// ownership fact means it can never be absent.
func TestProjectionAlwaysAllocatesThePairRelation(t *testing.T) {
	set := projectCuration(nil)
	if set.byHash == nil || set.byKey == nil || set.byPair == nil {
		t.Fatalf("projection of an empty ownership fact left a nil map: %+v", set)
	}
}

// TestBuildSnapshotStampsTheCurrentVersion: the envelope is stamped by the
// writer and never carried from the loaded file, so a pass cannot re-persist a
// foreign version it happened to read.
func TestBuildSnapshotStampsTheCurrentVersion(t *testing.T) {
	prev := feedState{}
	snap := buildSnapshot(&prev, &passWrites{scope: scopeCatalogue})
	if snap.Version != currentFeedVersion {
		t.Errorf("version = %d, want %d", snap.Version, currentFeedVersion)
	}
	if snap.Owners == nil || snap.Published == nil {
		t.Error("buildSnapshot left a required fact nil; the decode gate would refuse its own output")
	}
}

// TestBuildSnapshotNeverShrinksThePublicationLog: APPEND is the log's only rule,
// so a pass that publishes nothing still carries every prior record.
func TestBuildSnapshotNeverShrinksThePublicationLog(t *testing.T) {
	prev := feedState{published: publishedSignals("nyaa:1", "ab:2")}
	snap := buildSnapshot(&prev, &passWrites{scope: scopeWindow, published: publishedSignals("nyaa:3")})
	want := publishedSignals("nyaa:1", "ab:2", "nyaa:3")
	if !maps.Equal(snap.Published, want) {
		t.Errorf("publication log = %v, want %v", snap.Published, want)
	}
}

// TestSnapshotRoundTripsThroughTheBoundedDecoder is the wire-shape assertion for
// the new contract: the members a pass writes are exactly the members the
// bounded decoder recognizes, keys included.
func TestSnapshotRoundTripsThroughTheBoundedDecoder(t *testing.T) {
	in := snapshot{
		Version:   currentFeedVersion,
		Owners:    mergeOwners(ownsBy(7, hashed("nyaa:7", strings.Repeat("b", 40), true))),
		Published: publishedSignals("nyaa:7"),
		Titles:    map[string]string{"nyaa:7": "Show - S01"},
	}
	data, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _, reason, err := decodeSnapshot(data)
	if err != nil || reason != "" {
		t.Fatalf("decodeSnapshot: reason=%q err=%v", reason, err)
	}
	if !reflect.DeepEqual(got.Owners, in.Owners) {
		t.Errorf("owners round-trip = %v, want %v", got.Owners, in.Owners)
	}
	if !maps.Equal(got.Published, in.Published) {
		t.Errorf("publication log round-trip = %v, want %v", got.Published, in.Published)
	}
	if !strings.Contains(string(data), `"owners"`) {
		t.Error("the persisted document does not name the owners member")
	}
	if !strings.Contains(string(data), `"published"`) || !strings.Contains(string(data), `"version"`) {
		t.Error("the persisted document is missing the publication log or the version envelope")
	}
	if strings.Contains(string(data), `"by_hash"`) || strings.Contains(string(data), `"seen"`) {
		t.Error("the persisted document still carries a retired member; the search index is derived, not stored")
	}
}

// TestDecodeSnapshotReBaselinesAForeignVersion: a document that IDENTIFIES its
// version is re-baselined, not refused. The members that cannot be re-derived are
// exactly the ones a differently-versioned binary may have written in a shape
// this one misreads, so every member is zeroed and only the observed version is
// reported back. An ABSENT version is a different case and is structural - see
// TestDecodeSnapshotRefusesAnUnidentifiableDocument.
func TestDecodeSnapshotReBaselinesAForeignVersion(t *testing.T) {
	for _, version := range []int{1, currentFeedVersion + 1} {
		data, err := json.Marshal(&snapshot{
			Version:   version,
			Owners:    owns(keyed("nyaa:1", true)),
			Published: publishedSignals("nyaa:1"),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, _, reason, err := decodeSnapshot(data)
		if err != nil {
			t.Fatalf("version %d: decodeSnapshot: %v", version, err)
		}
		if reason != "" {
			t.Errorf("version %d: reason = %q, want empty (a version skew is not a fault)", version, reason)
		}
		if got.supportedVersion() {
			t.Errorf("version %d: reported as supported", version)
		}
		if len(got.Owners) != 0 || len(got.Published) != 0 {
			t.Errorf("version %d: members survived the re-baseline: %+v", version, got)
		}
		if got.Version != version {
			t.Errorf("version %d: observed version reported as %d", version, got.Version)
		}
	}
}

// TestDecodeSnapshotRefusesAMissingFact: the two facts are what the whole
// contract is; a document naming neither is structurally invalid, exactly as a
// missing curation map used to be.
func TestDecodeSnapshotRefusesAMissingFact(t *testing.T) {
	cases := map[string]string{
		"no owners":          `{"version":2,"published":{}}`,
		"no publication log": `{"version":2,"owners":{}}`,
	}
	assertStructural(t, cases)
}

// TestDecodeSnapshotRefusesAnUnidentifiableDocument keeps a version SKEW and
// CORRUPTION apart, which matters because the two get opposite treatment.
//
// A document carrying no version at all does not identify itself: `null`, `{}`, a
// truncated write and a retired pre-version file are all this shape. Reporting it
// structural is what stops the READER blanking a live feed over it - it keeps the
// last-good snapshot - whereas the re-baseline arm installs an empty one. The
// writer treats both as a baseline either way, so only the reader's posture
// differs, and for the reader "I cannot identify this file" is not a reason to
// stop serving what it already has.
func TestDecodeSnapshotRefusesAnUnidentifiableDocument(t *testing.T) {
	assertStructural(t, map[string]string{
		"null document":       `null`,
		"empty object":        `{}`,
		"members, no version": `{"owners":{},"published":{}}`,
	})
}

// assertStructural asserts each document is reported as a structural violation
// rather than accepted or errored.
func assertStructural(t *testing.T, cases map[string]string) {
	t.Helper()
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, reason, err := decodeSnapshot([]byte(doc))
			if err != nil {
				t.Fatalf("decodeSnapshot: %v", err)
			}
			if reason == "" {
				t.Error("accepted a structurally invalid snapshot")
			}
		})
	}
}

// TestOwnershipOfUnionsDuplicateRelationRows: the contribution of entry X is
// everything the pass evaluated for X, so two occurrences under one AniList id
// must union rather than the second overwriting the first.
//
// The reachable duplicate is a repeated `trs` relation row on ONE record, which
// is upstream data this app does not control. Two catalogue RECORDS sharing an
// alID is deliberately NOT the case under test, because it cannot arrive:
// seadexapi.validatePageIdentities fails the whole fetch on a repeated alID at
// window scope exactly as at catalogue scope (see its own tests). The union is
// still the safe fail direction for a hand-fed Advance caller, which the second
// case below covers without claiming the client can produce it.
func TestOwnershipOfUnionsDuplicateRelationRows(t *testing.T) {
	t.Parallel()
	for name, entries := range map[string][]seadex.Entry{
		"one record, duplicated relation row": {{
			AniListID: 5,
			Torrents: []seadex.Torrent{
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
				{Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
			},
		}},
		"hand-fed duplicate records (not reachable through the client)": {
			{AniListID: 5, Torrents: []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}}},
			{AniListID: 5, Torrents: []seadex.Torrent{{Tracker: "Nyaa", URL: "https://nyaa.si/view/2"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := ownershipOf(entries)
			if len(got[ownerKey(5)]) != 2 {
				t.Errorf("entry 5 owns %d releases, want 2 (both occurrences unioned): %v",
					len(got[ownerKey(5)]), got)
			}
			set := projectCuration(got)
			for _, key := range []string{"nyaa:1", "nyaa:2"} {
				if _, ok := set.byKey[key]; !ok {
					t.Errorf("key %q missing from the projection", key)
				}
			}
		})
	}
}

// TestOwnershipOfKeepsAnEvaluatedEntryWithNoReleases: an entry evaluated down to
// nothing must still APPEAR in the evaluated set, or upsertOwners cannot clear a
// stored contribution that is no longer curated.
func TestOwnershipOfKeepsAnEvaluatedEntryWithNoReleases(t *testing.T) {
	got := ownershipOf([]seadex.Entry{{AniListID: 9}})
	if _, present := got[ownerKey(9)]; !present {
		t.Fatal("an entry with no curated releases vanished from the evaluated set")
	}
	cleared := upsertOwners(ownsBy(9, keyed("nyaa:1", true)), got, scopeWindow)
	if _, still := cleared[ownerKey(9)]; still {
		t.Error("entry 9's stored contribution survived a pass that evaluated it down to nothing")
	}
}

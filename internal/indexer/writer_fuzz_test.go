package indexer

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// total sums one correction across feeds. It lives with the fuzz target that asserts
// on it: no production consumer wants the aggregate - the server WARNs per tracker
// feed so an operator can tell WHICH journal was tampered with, and the writer ignores
// the counts entirely (its rebuild re-persists the scrubbed form regardless).
func (s snapshotScrub) total() int {
	n := 0
	for _, c := range s.blankedInfoURLs {
		n += c
	}
	return n
}

// FuzzDecodeSnapshot is the coverage-guided complement of the unit tests over
// the ONE persisted-snapshot decode gate both consumers share (the writer's
// loadPrevious and the server's readSnapshot): /config/feed.json is a
// hand-editable, corruptible boundary, and every downstream guarantee rests
// on this one function - the curation maps are indexed unconditionally, the
// persisted-item limits keep renderFeed's XML escaping bounded, and the info
// hash is read as torrent identity by both the served infohash attr and the
// writer's warning-retraction gates. Invariants: a rejected snapshot returns
// zero data (never partially materialized state), a rejection is reported
// exactly once (an error or a reason, never both), an accepted snapshot
// carries non-nil curation maps, only within-limit items, and canonical info
// hashes and download-volume-factor markers, and re-decoding an accepted snapshot's own re-encoding is accepted
// unchanged (the gate never rejects what a rebuild would persist, and the
// canonical re-encoding is byte-identical, so no field, map entry, item, or
// ordering shifts on the second pass and nothing further is blanked).
func FuzzDecodeSnapshot(f *testing.F) {
	f.Add([]byte(emptyLedgerJSON))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"by_hash":{},"by_key":{}}`))
	f.Add([]byte(`{"by_hash":{"ABCDEF1234567890abcdef1234567890abcdef12":true},"by_key":{"nyaa:1":true},"seen":{},"nyaa_feed":[{"Title":"Show - S01","GUID":"https://nyaa.si/view/1","InfoHash":"ABCDEF1234567890ABCDEF1234567890ABCDEF12","Key":"nyaa:1","Categories":[5070]}]}`))
	f.Add([]byte(`{"by_hash":{},"by_key":{},"seen":{},"ab_feed":[{"Title":"x","Size":-1}]}`))
	f.Add([]byte(`{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Title":"x","Categories":[0]}]}`))
	f.Add([]byte(`{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Title":"x","Key":"nyaa:1","GUID":"https://nyaa.si/view/1","DownloadVolumeFactor":"0"}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		snap, scrub, reason, err := decodeSnapshot(data)
		if err != nil || reason != "" {
			if err != nil && reason != "" {
				t.Errorf("decodeSnapshot reported both a decode error (%v) and a structural reason (%q)", err, reason)
			}
			if !reflect.DeepEqual(snap, snapshot{}) {
				t.Errorf("rejected snapshot (reason=%q err=%v) returned non-zero data: %+v", reason, err, snap)
			}
			if n := scrub.total(); n != 0 {
				t.Errorf("rejected snapshot (reason=%q err=%v) reported %d blanked info URLs, want 0 (nothing was materialized)", reason, err, n)
			}
			return
		}
		if snap.ByHash == nil || snap.ByKey == nil {
			t.Fatal("accepted snapshot carries nil curation maps (both consumers index them unconditionally)")
		}
		if !validFeedItems(snap.NyaaFeed, snap.ABFeed) {
			t.Error("accepted snapshot carries an item past the persisted-item limits")
		}
		for name, feed := range map[string][]journalItem{"nyaa_feed": snap.NyaaFeed, "ab_feed": snap.ABFeed} {
			for i := range feed {
				if h := feed[i].InfoHash; h != validInfoHash(h) {
					t.Errorf("%s[%d] accepted with non-canonical info hash %q (the served infohash attr and the writer's carry gates both read it as identity)", name, i, h)
				}
				if m := feed[i].DownloadVolumeFactor; m != validMarker(m) {
					t.Errorf("%s[%d] accepted with out-of-vocabulary download-volume-factor %q (writeItem renders it as the arr's freeleech accounting input, and carryStoredItem re-persists it verbatim)", name, i, m)
				}
			}
		}
		encoded, mErr := json.Marshal(&snap)
		if mErr != nil {
			t.Fatalf("re-encode of an accepted snapshot failed: %v", mErr)
		}
		round, roundScrub, roundReason, roundErr := decodeSnapshot(encoded)
		if roundErr != nil || roundReason != "" {
			t.Fatalf("re-decode of an accepted snapshot rejected it (reason=%q err=%v)", roundReason, roundErr)
		}
		if n := roundScrub.total(); n != 0 {
			t.Errorf("re-decode blanked %d further info URLs, want 0 (the first pass already produced the canonical form)", n)
		}
		// Compare the canonical encodings rather than the structs: json.Marshal
		// sorts map keys and renders times canonically, so byte equality pins
		// every field, map entry, item, and ordering without tripping over a
		// decoded time's fresh fixed-zone Location pointer.
		reEncoded, rErr := json.Marshal(&round)
		if rErr != nil {
			t.Fatalf("re-encode of the re-decoded snapshot failed: %v", rErr)
		}
		if !bytes.Equal(encoded, reEncoded) {
			t.Errorf("re-decode changed an accepted snapshot:\n first: %s\nsecond: %s", encoded, reEncoded)
		}
	})
}

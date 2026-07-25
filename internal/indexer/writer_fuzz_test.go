package indexer

import (
	"encoding/json"
	"testing"
)

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
// hashes, and re-decoding an accepted snapshot's own re-encoding is accepted
// unchanged (the gate never rejects what a rebuild would persist).
func FuzzDecodeSnapshot(f *testing.F) {
	f.Add([]byte(emptyLedgerJSON))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"by_hash":{},"by_key":{}}`))
	f.Add([]byte(`{"by_hash":{"ABCDEF1234567890abcdef1234567890abcdef12":true},"by_key":{"nyaa:1":true},"seen":{},"nyaa_feed":[{"Title":"Show - S01","GUID":"https://nyaa.si/view/1","InfoHash":"ABCDEF1234567890ABCDEF1234567890ABCDEF12","Key":"nyaa:1","Categories":[5070]}]}`))
	f.Add([]byte(`{"by_hash":{},"by_key":{},"seen":{},"ab_feed":[{"Title":"x","Size":-1}]}`))
	f.Add([]byte(`{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[{"Title":"x","Categories":[0]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		snap, _, reason, err := decodeSnapshot(data)
		if err != nil || reason != "" {
			if err != nil && reason != "" {
				t.Errorf("decodeSnapshot reported both a decode error (%v) and a structural reason (%q)", err, reason)
			}
			if snap.ByHash != nil || snap.ByKey != nil || len(snap.NyaaFeed) != 0 || len(snap.ABFeed) != 0 {
				t.Errorf("rejected snapshot (reason=%q err=%v) returned non-zero data: %+v", reason, err, snap)
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
			}
		}
		encoded, mErr := json.Marshal(&snap)
		if mErr != nil {
			t.Fatalf("re-encode of an accepted snapshot failed: %v", mErr)
		}
		round, _, roundReason, roundErr := decodeSnapshot(encoded)
		if roundErr != nil || roundReason != "" {
			t.Fatalf("re-decode of an accepted snapshot rejected it (reason=%q err=%v)", roundReason, roundErr)
		}
		if len(round.NyaaFeed) != len(snap.NyaaFeed) || len(round.ABFeed) != len(snap.ABFeed) {
			t.Errorf("re-decode changed feed lengths: nyaa %d -> %d, ab %d -> %d",
				len(snap.NyaaFeed), len(round.NyaaFeed), len(snap.ABFeed), len(round.ABFeed))
		}
	})
}

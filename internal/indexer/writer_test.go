package indexer

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

// TestRebuildWarnsWhenABPasskeyMissing pins the operator nudge: a rebuild
// journaling AnimeBytes releases with no configured passkey still writes the
// snapshot (Nyaa unaffected) and logs ONE warning carrying the skip count, so
// the operator learns why the AB RSS feed has nothing grabbable. The logger is
// injected via NewFeedWriter, so no slog.Default swap is needed.
func TestRebuildWarnsWhenABPasskeyMissing(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{
			{
				Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
		},
	}}
	w := NewFeedWriter(&FeedWriterConfig{Path: path, ABTorznabURL: "http://prowlarr/2/api"}, Deps{Logger: log})
	if err := w.Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("missing passkey warning not logged; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
	skipped := int64(-1)
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "ab_releases_skipped" {
				skipped = a.Value.Int64()
			}
			return true
		})
	}
	if skipped != 1 {
		t.Errorf("warning does not carry ab_releases_skipped=1 (got %d); log output:\n%s", skipped, strings.Join(rec.Messages(), "\n"))
	}
	snap := readSnapshotFile(t, path)
	if len(snap.NyaaFeed) != 1 {
		t.Errorf("nyaa feed = %d items, want 1 (Nyaa unaffected by the AB skip)", len(snap.NyaaFeed))
	}
}

// TestRebuildNoPasskeyWarnWithoutABIntent pins the WARN gate: a deployment with
// no AB Torznab URL (a Nyaa-only operator) skips the missing-passkey nudge even
// though newly curated AB releases were skipped, so the per-cycle log does not
// nag about a tracker the operator opted out of.
func TestRebuildNoPasskeyWarnWithoutABIntent(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rec.Contains("ab RSS feed empty of grabbable links") {
		t.Errorf("passkey warning logged without AB intent; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildUnconfiguredABPersistsNoABFeed pins the write side of the
// README's per-tracker off switch: with AnimeBytes unconfigured (an empty
// ab_torznab_url) but a passkey still set, a rebuild must persist NO
// AnimeBytes feed - the passkey must not land on disk in synthesized download
// links for a tracker the operator turned off - while the curation set and the
// Nyaa feed are unaffected. The construction-time WARN names the mismatched
// fields so the half-configured intent surfaces.
func TestRebuildUnconfiguredABPersistsNoABFeed(t *testing.T) {
	log, rec := capture.New()
	path := filepath.Join(t.TempDir(), "feed.json")
	seedEmptyLedger(t, path)
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{
			{
				Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
			{
				Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
				Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
			},
		},
	}}
	if err := NewFeedWriter(&FeedWriterConfig{Path: path, ABPasskey: "SECRETPASSKEY"}, Deps{Logger: log}).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if bytes.Contains(data, []byte("SECRETPASSKEY")) {
		t.Error("snapshot persists the passkey for an unconfigured AB tracker")
	}
	snap := readSnapshotFile(t, path)
	if len(snap.ABFeed) != 0 {
		t.Errorf("ab_feed = %d items, want 0 (unconfigured tracker's feed must not be built)", len(snap.ABFeed))
	}
	if len(snap.NyaaFeed) != 1 {
		t.Errorf("nyaa_feed = %d items, want 1 (the configured tracker is unaffected)", len(snap.NyaaFeed))
	}
	if len(snap.ByKey) == 0 {
		t.Error("curation set empty: the search index must still cover AB releases (search rides Prowlarr, no passkey)")
	}
	if !snap.Seen["ab:123"] {
		t.Errorf("seen ledger missing the skipped AB identity (it must not journal later as new): %v", snap.Seen)
	}
	if !rec.Contains("indexer.ab_passkey is set but indexer.ab_torznab_url is empty") {
		t.Errorf("half-configured AB intent not warned at construction; log output:\n%s", strings.Join(rec.Messages(), "\n"))
	}
}

// TestRebuildReportsWriteError pins the write-failure path: when the snapshot
// cannot be persisted (here the target's parent is a regular file, a root-safe
// ENOTDIR injection - which the previous-snapshot read classifies as absent,
// so the failure surfaces at the write), Rebuild returns a wrapped error
// naming the path rather than logging success.
func TestRebuildReportsWriteError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	path := filepath.Join(blocker, "feed.json")
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{}).Rebuild(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Rebuild with an unwritable path returned nil, want error")
	}
	if !strings.Contains(err.Error(), "write feed snapshot") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it wrapped as a feed snapshot write failure naming %q", err, path)
	}
}

// TestRebuildFailsOnUnreadablePreviousSnapshot pins the transient-read
// posture: a previous snapshot that stats fine but cannot be read (here a
// directory, a root-safe EISDIR injection) must FAIL the rebuild - never
// re-baseline and blank a live journal over a transient fault - so the
// last-good snapshot stays served and the next cycle retries.
func TestRebuildFailsOnUnreadablePreviousSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir over snapshot path: %v", err)
	}
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{}).Rebuild(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Rebuild with an unreadable previous snapshot returned nil, want error")
	}
	if !strings.Contains(err.Error(), "read previous feed snapshot") {
		t.Errorf("error = %q, want it wrapped as a previous-snapshot read failure", err)
	}
}

// TestRebuildRejectsOversizedSnapshot pins the write-side size bound: a
// snapshot that marshals past maxFeedBytes (which Indexer.reload would refuse)
// is rejected BEFORE the atomic write, returning a size error naming actual and
// maximum bytes, and the previous last-good snapshot stays in place readable.
func TestRebuildRejectsOversizedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	previous := []byte(`{"by_hash":{},"by_key":{},"seen":{},"nyaa_feed":[],"ab_feed":[]}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatalf("seed previous snapshot: %v", err)
	}
	// A file-less torrent's feed title falls back to the release group, so an
	// oversized group inflates the marshaled snapshot past maxFeedBytes without
	// regex-scanning a huge file name.
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "Nyaa", URL: "https://nyaa.si/view/42", IsBest: true,
			ReleaseGroup: strings.Repeat("a", maxFeedBytes+1),
		}},
	}}
	err := NewFeedWriter(&FeedWriterConfig{Path: path}, Deps{}).Rebuild(context.Background(), entries, nil)
	if err == nil {
		t.Fatal("Rebuild with an oversized snapshot returned nil, want size error")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("error = %q, want a size-cap error naming the max", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("previous snapshot unreadable after rejection: %v", readErr)
	}
	if !bytes.Equal(got, previous) {
		t.Error("previous snapshot replaced despite size rejection")
	}
}

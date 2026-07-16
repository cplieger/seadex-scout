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
)

// TestRebuildWarnsWhenABPasskeyMissing pins the operator nudge: a rebuild whose
// catalogue carries AnimeBytes releases but no configured passkey still writes
// the snapshot (Nyaa unaffected) and logs ONE warning carrying the skip count,
// so the operator learns why the AB RSS feed has nothing grabbable. The logger
// is injected via NewFeedWriter, so no slog.Default swap is needed.
func TestRebuildWarnsWhenABPasskeyMissing(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	path := filepath.Join(t.TempDir(), "feed.json")
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
	if err := NewFeedWriter("", true, path, log).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ab RSS feed empty of grabbable links") {
		t.Errorf("missing passkey warning not logged; log output:\n%s", out)
	}
	if !strings.Contains(out, "ab_releases_skipped=1") {
		t.Errorf("warning does not carry ab_releases_skipped=1; log output:\n%s", out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("snapshot not written despite AB skip: %v", err)
	}
}

// TestRebuildNoPasskeyWarnWithoutABIntent pins the WARN gate: a deployment with
// no AB Torznab URL (abConfigured=false, a Nyaa-only operator) skips the
// missing-passkey nudge even though the catalogue carries AB releases, so the
// per-cycle log does not nag about a tracker the operator opted out of.
func TestRebuildNoPasskeyWarnWithoutABIntent(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	path := filepath.Join(t.TempDir(), "feed.json")
	entries := []seadex.Entry{{
		AniListID: 9,
		Torrents: []seadex.Torrent{{
			Tracker: "AB", URL: "/torrents.php?id=1&torrentid=123", IsBest: true,
			Files: []seadex.File{{Length: 1, Name: "Show - S01E01 (1080p) [G].mkv"}},
		}},
	}}
	if err := NewFeedWriter("", false, path, log).Rebuild(context.Background(), entries, nil); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if out := buf.String(); strings.Contains(out, "ab RSS feed empty of grabbable links") {
		t.Errorf("passkey warning logged without AB intent; log output:\n%s", out)
	}
}

// TestRebuildReportsWriteError pins the write-failure path: when the snapshot
// cannot be persisted (here the target's parent is a regular file, a root-safe
// ENOTDIR injection), Rebuild returns a wrapped error naming the path rather
// than logging success.
func TestRebuildReportsWriteError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	path := filepath.Join(blocker, "feed.json")
	err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Rebuild with an unwritable path returned nil, want error")
	}
	if !strings.Contains(err.Error(), "write feed snapshot") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it wrapped as a feed snapshot write failure naming %q", err, path)
	}
}

// TestRebuildRejectsOversizedSnapshot pins the write-side size bound: a
// snapshot that marshals past maxFeedBytes (which Indexer.reload would refuse)
// is rejected BEFORE the atomic write, returning a size error naming actual and
// maximum bytes, and the previous last-good snapshot stays in place readable.
func TestRebuildRejectsOversizedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feed.json")
	previous := []byte(`{"by_hash":{},"by_key":{},"nyaa_feed":[],"ab_feed":[]}`)
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
	err := NewFeedWriter("", false, path, nil).Rebuild(context.Background(), entries, nil)
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

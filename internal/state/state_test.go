package state

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStoreLoadMissingReturnsEmptyState(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	st, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load missing state returned error: %v", err)
	}
	if st.Baselined || len(st.Library.Items) != 0 || len(st.Mapping.Records) != 0 || len(st.Memo.Entries) != 0 || len(st.Findings) != 0 {
		t.Errorf("Load missing state = %+v, want zero state", st)
	}
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewStore(filepath.Join(t.TempDir(), "nested", "state.json"), testLogger())
	want := &State{
		Library: library.Snapshot{
			TakenAt: now,
			Items: []library.Item{{
				SeasonGroups: map[int][]string{1: {"subsplease"}},
				Arr:          library.ArrSonarr,
				Title:        "Frieren",
				Groups:       []string{"subsplease"},
				ArrID:        7,
				TvdbID:       123,
				Year:         2023,
				HasFile:      true,
			}},
		},
		Mapping: mapping.Cache{
			FetchedAt: now,
			ETag:      "etag-1",
			Records:   []mapping.Record{{AniListID: 154587, Type: "TV", TvdbID: 123, SeasonTvdb: 1}},
		},
		Memo: match.Memo{Entries: map[int]match.MemoEntry{
			154587: {Titles: []string{"Frieren"}, Format: "TV", Year: 2023},
		}},
		Findings: map[string]report.Alerted{
			"dedupe": {
				AlertedAt: now,
				Finding: compare.Finding{
					Title:     "Frieren",
					Arr:       library.ArrSonarr,
					DedupeKey: "dedupe",
					Status:    compare.StatusBetter,
					AniListID: 154587,
				},
			},
		},
		Baselined: true,
	}

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after Save returned error: %v", err)
	}
	if !got.Baselined {
		t.Error("Baselined = false, want true")
	}
	if len(got.Library.Items) != 1 || got.Library.Items[0].Title != "Frieren" || got.Library.Items[0].SeasonGroups[1][0] != "subsplease" {
		t.Errorf("Library round trip = %+v, want Frieren with season group", got.Library)
	}
	if len(got.Mapping.Records) != 1 || got.Mapping.Records[0].AniListID != 154587 || !got.Mapping.FetchedAt.Equal(now) {
		t.Errorf("Mapping round trip = %+v, want AniList 154587 fetched at %s", got.Mapping, now)
	}
	if got.Memo.Entries[154587].Year != 2023 {
		t.Errorf("Memo year = %d, want 2023", got.Memo.Entries[154587].Year)
	}
	alert, ok := got.Findings["dedupe"]
	if !ok || alert.Finding.Title != "Frieren" || !alert.AlertedAt.Equal(now) {
		t.Errorf("Findings round trip = %+v, want preserved dedupe alert", got.Findings)
	}
}

func TestStoreLoadCorruptReturnsDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	_, err := NewStore(path, testLogger()).Load(context.Background())
	if err == nil {
		t.Fatal("Load corrupt state returned nil error, want decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error = %q, want decode context", err.Error())
	}
}

func TestStoreLoadOversizedReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create oversized state: %v", err)
	}
	if err := f.Truncate(maxStateBytes + 1); err != nil {
		t.Fatalf("truncate oversized state: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close oversized state: %v", err)
	}
	_, err = NewStore(path, testLogger()).Load(context.Background())
	if err == nil {
		t.Fatal("Load oversized state returned nil error, want bounded-read error")
	}
}

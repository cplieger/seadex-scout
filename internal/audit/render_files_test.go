package audit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWriteFilesWritesTimestampedPair pins the on-disk report contract the
// README documents: a report-<UTC timestamp>.md + .json pair (colon-free,
// sortable, second precision) written into the report dir, which is created
// when missing, with the JSON round-tripping back to the report.
func TestWriteFilesWritesTimestampedPair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows:        []Row{{Title: "Frieren", Arr: "sonarr", Verdict: VerdictBest}},
	}

	if err := r.WriteFiles(context.Background(), dir, slog.Default()); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	base := filepath.Join(dir, "report-2026-07-11T15-04-05Z")
	md, err := os.ReadFile(base + ".md")
	if err != nil {
		t.Fatalf("markdown not written at the timestamped path: %v", err)
	}
	if !strings.Contains(string(md), "Frieren") {
		t.Error("written markdown is missing the row title")
	}
	data, err := os.ReadFile(base + ".json")
	if err != nil {
		t.Fatalf("json not written at the timestamped path: %v", err)
	}
	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("written json does not parse: %v", err)
	}
	if len(back.Rows) != 1 || back.Rows[0].Title != "Frieren" || back.Rows[0].Verdict != VerdictBest {
		t.Errorf("json rows = %+v, want the one Frieren have_best row", back.Rows)
	}
}

func TestWriteFilesReportsJSONWriteError(t *testing.T) {
	parent := t.TempDir()
	dirAsFile := filepath.Join(parent, "reports")
	if err := os.WriteFile(dirAsFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{},
	}

	err := r.WriteFiles(context.Background(), dirAsFile, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("WriteFiles must fail when the report dir path is an existing file")
	}
	// JSON is written first, so the dir-as-file failure surfaces on it.
	if !strings.Contains(err.Error(), "write json") {
		t.Errorf("error = %q, want it wrapped with the write-json context", err)
	}
}

// TestWriteFilesJSONFailureSkipsMarkdown pins one half of the deliberate
// json-then-md write order: when the JSON half cannot be committed, the
// Markdown half is never attempted, so a failed run cannot leave a dangling
// .md without its machine-readable pair.
func TestWriteFilesJSONFailureSkipsMarkdown(t *testing.T) {
	dir := t.TempDir()
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{},
	}
	// Occupy the deterministic .json target with a directory so the JSON
	// commit fails first.
	jsonPath := filepath.Join(dir, "report-2026-07-11T15-04-05Z.json")
	if err := os.MkdirAll(jsonPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := r.WriteFiles(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("WriteFiles must fail when the JSON target cannot be committed")
	}
	if !strings.Contains(err.Error(), "write json") {
		t.Errorf("error = %q, want it wrapped with the write-json context", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "report-2026-07-11T15-04-05Z.md")); statErr == nil {
		t.Error("the markdown half must not be written when the JSON half failed (json-then-md order)")
	}
}

// TestWriteFilesWritesJSONBeforeMarkdown pins the other half of the write
// order: when the Markdown half fails, the JSON half was already committed, so
// an interrupted run leaves a .json without .md but never the reverse.
func TestWriteFilesWritesJSONBeforeMarkdown(t *testing.T) {
	dir := t.TempDir()
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{},
	}
	// Occupy the deterministic .md target with a directory so the Markdown
	// commit fails after the JSON write.
	mdPath := filepath.Join(dir, "report-2026-07-11T15-04-05Z.md")
	if err := os.MkdirAll(mdPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := r.WriteFiles(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("WriteFiles must fail when the Markdown target cannot be committed")
	}
	if !strings.Contains(err.Error(), "write markdown") {
		t.Errorf("error = %q, want it wrapped with the write-markdown context", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "report-2026-07-11T15-04-05Z.json")); statErr != nil {
		t.Errorf("the JSON half must be committed before the Markdown failure: %v", statErr)
	}
}

// TestAcquireReportLockRefusesConcurrentRun pins the concurrency refusal: a
// second acquire while the lock is held returns ErrReportRunning with the
// exact message the report subcommand surfaces, and never blocks.
func TestAcquireReportLockRefusesConcurrentRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	release, err := AcquireReportLock(dir)
	if err != nil {
		t.Fatalf("first AcquireReportLock: %v", err)
	}

	_, err = AcquireReportLock(dir)
	if !errors.Is(err, ErrReportRunning) {
		t.Fatalf("second AcquireReportLock = %v, want ErrReportRunning", err)
	}
	if err.Error() != "another report is already running" {
		t.Errorf("refusal message = %q, want %q", err.Error(), "another report is already running")
	}

	release()
	release2, err := AcquireReportLock(dir)
	if err != nil {
		t.Fatalf("AcquireReportLock after release = %v, want success", err)
	}
	release2()
}

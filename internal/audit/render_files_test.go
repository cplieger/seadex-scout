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

	if err := r.WriteFiles(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
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

// TestWriteFilesMarkdownFailureLeavesJSONAndWrapsError pins the other arm of
// the documented JSON-first ordering: when the Markdown half fails after the
// JSON half has already committed, WriteFiles surfaces the wrapped
// write-markdown error while the machine-readable JSON half survives
// parseable on disk (never a dangling .md without its .json, and never a
// swallowed error).
func TestWriteFilesMarkdownFailureLeavesJSONAndWrapsError(t *testing.T) {
	dir := t.TempDir()
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{},
		Rows:        []Row{{Title: "Frieren", Arr: "sonarr", Verdict: VerdictBest}},
	}
	base := filepath.Join(dir, "report-2026-07-11T15-04-05Z")
	if err := os.Symlink("missing-target", base+".md"); err != nil {
		t.Fatal(err)
	}

	err := r.WriteFiles(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("WriteFiles must fail when the Markdown target is a symlink")
	}
	if !strings.Contains(err.Error(), "write markdown") {
		t.Errorf("error = %q, want it wrapped with the write-markdown context", err)
	}
	data, readErr := os.ReadFile(base + ".json")
	if readErr != nil {
		t.Fatalf("JSON half must remain after the Markdown write fails: %v", readErr)
	}
	var back Report
	if unmarshalErr := json.Unmarshal(data, &back); unmarshalErr != nil {
		t.Fatalf("surviving JSON half does not parse: %v", unmarshalErr)
	}
	if len(back.Rows) != 1 || back.Rows[0].Title != "Frieren" {
		t.Errorf("surviving JSON rows = %+v, want the Frieren row", back.Rows)
	}
}

// TestWriteFilesRedactsArrURLCredentials pins the persistence-sink URL
// sanitization: a credentialed arr deep-link (URL userinfo password plus a
// credential-like query token) never lands in either half of the owner-only
// report pair, while the clean host/path link survives clickable and the canonical
// report stays unmutated.
func TestWriteFilesRedactsArrURLCredentials(t *testing.T) {
	dir := t.TempDir()
	credURL := "https://admin:hunter2@sonarr.example/series/frieren?apikey=tok3n"
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows: []Row{{
			Title:   "Frieren",
			Arr:     "sonarr",
			Verdict: VerdictBest,
			ArrURL:  credURL,
		}},
	}

	if err := r.WriteFiles(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	base := filepath.Join(dir, "report-2026-07-11T15-04-05Z")
	for _, ext := range []string{".json", ".md"} {
		data, err := os.ReadFile(base + ext)
		if err != nil {
			t.Fatalf("read %s half: %v", ext, err)
		}
		for _, secret := range []string{"hunter2", "tok3n"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s report contains credential %q", ext, secret)
			}
		}
		if !strings.Contains(string(data), "https://sonarr.example/series/frieren") {
			t.Errorf("%s report is missing the sanitized host/path deep-link", ext)
		}
	}
	if r.Rows[0].ArrURL != credURL {
		t.Errorf("WriteFiles mutated the canonical report's ArrURL to %q", r.Rows[0].ArrURL)
	}
}

// TestWriteFilesSurfacesProbePathError pins reportPairStem's error contract:
// a non-NotExist stat error while probing the pair stem (here ENOTDIR from a
// dir path that is an existing file) is surfaced instead of risking an
// overwrite, and nothing is written.
func TestWriteFilesSurfacesProbePathError(t *testing.T) {
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
	if !strings.Contains(err.Error(), "probe report path") {
		t.Errorf("error = %q, want it wrapped with the probe-report-path context", err)
	}
}

// TestWriteFilesJSONFailureSkipsMarkdown pins one half of the deliberate
// json-then-md write order plus the write-json error wrap: when the JSON half
// cannot be committed, the Markdown half is never attempted, so a failed run
// cannot leave a dangling .md without its machine-readable pair. A dangling
// JSON symlink is a root-safe deterministic failure: os.Stat treats the
// missing target as free during reportPairStem, then atomicfile refuses the
// symlink target — so this also runs in root-run containers where a chmod
// 0555 dir would not produce EACCES.
func TestWriteFilesJSONFailureSkipsMarkdown(t *testing.T) {
	dir := t.TempDir()
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{},
	}
	base := filepath.Join(dir, "report-2026-07-11T15-04-05Z")
	if err := os.Symlink("missing-target", base+".json"); err != nil {
		t.Fatal(err)
	}

	err := r.WriteFiles(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("WriteFiles must fail when the JSON target is a symlink")
	}
	if !strings.Contains(err.Error(), "write json") {
		t.Errorf("error = %q, want it wrapped with the write-json context", err)
	}
	if _, statErr := os.Stat(base + ".md"); statErr == nil {
		t.Error("the markdown half must not be written when the JSON half failed (json-then-md order)")
	}
}

// TestWriteFilesProbesSuffixWhenEitherHalfExists pins the stem probe's
// either-half rule: a pre-existing .md half at the deterministic stem (e.g.
// left by an interrupted earlier run) pushes the whole new pair to the -2
// suffix, leaving the existing file untouched and never pairing a fresh .json
// with a stale .md.
func TestWriteFilesProbesSuffixWhenEitherHalfExists(t *testing.T) {
	dir := t.TempDir()
	r := &Report{
		GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC),
		Totals:      map[string]int{},
	}
	mdPath := filepath.Join(dir, "report-2026-07-11T15-04-05Z.md")
	if err := os.WriteFile(mdPath, []byte("stale half"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.WriteFiles(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}

	stale, err := os.ReadFile(mdPath)
	if err != nil || string(stale) != "stale half" {
		t.Errorf("pre-existing md half must be untouched, got %q, %v", stale, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "report-2026-07-11T15-04-05Z.json")); statErr == nil {
		t.Error("the new json must not land beside the stale md at the unsuffixed stem")
	}
	for _, ext := range []string{".json", ".md"} {
		path := filepath.Join(dir, "report-2026-07-11T15-04-05Z-2"+ext)
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("suffixed pair half %s missing: %v", path, statErr)
		}
	}
}

// TestWriteFilesSameSecondRerunKeepsBothPairs pins the README's
// never-overwrite contract at second granularity: two strictly-sequential
// reports sharing the same UTC-second GeneratedAt produce two complete pairs
// (the second at the -2 suffix) instead of the second silently replacing the
// first.
func TestWriteFilesSameSecondRerunKeepsBothPairs(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	stamp := time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC)
	first := &Report{GeneratedAt: stamp, Totals: map[string]int{}, Rows: []Row{{Title: "First", Arr: "sonarr", Verdict: VerdictBest}}}
	second := &Report{GeneratedAt: stamp, Totals: map[string]int{}, Rows: []Row{{Title: "Second", Arr: "sonarr", Verdict: VerdictBest}}}

	if err := first.WriteFiles(context.Background(), dir, log); err != nil {
		t.Fatalf("first WriteFiles: %v", err)
	}
	if err := second.WriteFiles(context.Background(), dir, log); err != nil {
		t.Fatalf("second WriteFiles: %v", err)
	}

	base := filepath.Join(dir, "report-2026-07-11T15-04-05Z")
	for _, path := range []string{base + ".json", base + ".md", base + "-2.json", base + "-2.md"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected complete pair half %s: %v", path, err)
		}
	}
	md1, _ := os.ReadFile(base + ".md")
	if !strings.Contains(string(md1), "First") {
		t.Error("first report's markdown was overwritten by the same-second rerun")
	}
	md2, _ := os.ReadFile(base + "-2.md")
	if !strings.Contains(string(md2), "Second") {
		t.Error("second report's markdown missing from the suffixed pair")
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

func TestAcquireReportLockReportsMkdirError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "reports")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := AcquireReportLock(filepath.Join(blocker, "sub"))

	if err == nil {
		t.Fatal("AcquireReportLock must fail when the report dir cannot be created")
	}
	if !strings.Contains(err.Error(), "create report dir") {
		t.Errorf("error = %q, want it wrapped with the create-report-dir context", err)
	}
	if errors.Is(err, ErrReportRunning) {
		t.Error("a mkdir failure must not be reported as a concurrent-run refusal")
	}
}

func TestAcquireReportLockReportsOpenError(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, reportLockName)
	if err := os.Mkdir(lockPath, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := AcquireReportLock(dir)

	if err == nil {
		t.Fatal("AcquireReportLock must fail when report.lock is a directory")
	}
	if !strings.Contains(err.Error(), "report lock") {
		t.Errorf("error = %q, want it wrapped with the report-lock context", err)
	}
	if errors.Is(err, ErrReportRunning) {
		t.Error("an open failure must not be reported as a concurrent-run refusal")
	}
}

// TestWriteFilesAlreadyCanceledWritesNothing pins WriteFiles' first
// cancellation checkpoint: an already-canceled context returns the
// report-write stage error before the stale-temp cleanup, the stem probe, or
// any write runs, so nothing is created on disk.
func TestWriteFilesAlreadyCanceledWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Report{GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC), Totals: map[string]int{}}

	err := r.WriteFiles(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFiles error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "report write interrupted") {
		t.Errorf("error = %q, want the report-write stage context", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("report dir stat = %v, want absent (nothing written before the first checkpoint)", statErr)
	}
}

// TestReportPairStemAlreadyCanceledStopsProbe pins reportPairStem's in-loop
// cancellation checkpoint: a canceled report-wide context stops the directory
// scan on the first probe round with the stem-probe stage error and an empty
// stem, keeping a routine SIGTERM off main's ERROR alert.
func TestReportPairStemAlreadyCanceledStopsProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stem, err := reportPairStem(ctx, t.TempDir(), time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reportPairStem error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "report stem probe interrupted") {
		t.Errorf("error = %q, want the stem-probe stage context", err)
	}
	if stem != "" {
		t.Errorf("stem = %q, want empty on interruption", stem)
	}
}

// TestReportPairStemSkipsMultipleOccupiedSuffixes pins the repeated suffix
// probe: when both the deterministic stem and its -2 suffix are occupied
// (either half), the probe advances one suffix at a time and selects -3 —
// the README's deterministic -2/-3/... contract. A regression that advances
// the suffix by two would keep every single-collision test green but pick -4
// here.
func TestReportPairStemSkipsMultipleOccupiedSuffixes(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Date(2026, time.July, 11, 15, 4, 5, 0, time.UTC)
	base := filepath.Join(dir, "report-2026-07-11T15-04-05Z")
	if err := os.WriteFile(base+".json", []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+"-2.md", []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := reportPairStem(t.Context(), dir, stamp)
	if err != nil {
		t.Fatalf("reportPairStem: %v", err)
	}
	if want := base + "-3"; got != want {
		t.Errorf("reportPairStem() = %q, want %q", got, want)
	}
}

// pathExistsCancelCtx is a context whose Err flips to context.Canceled once
// the watched path exists, deterministically landing a cancellation after the
// JSON half commits (atomicfile's own ctx polls all happen before the final
// rename, so the JSON write itself succeeds) but before the Markdown half
// renders. The checkpoints poll Err via interrupted and never select on
// Done; context.Cause falls back to Err for a non-cancelCtx context.
type pathExistsCancelCtx struct {
	context.Context
	path string
}

func (c *pathExistsCancelCtx) Err() error {
	if _, err := os.Stat(c.path); err == nil {
		return context.Canceled
	}
	return nil
}

// TestWriteFilesCanceledAfterJSONSkipsMarkdown pins the markdown-render
// cancellation checkpoint (the one mid-pipeline stage no existing test
// reaches): a cancellation observed after the JSON half is committed stops
// the pipeline with the markdown-render stage error, leaving the
// machine-readable .json on disk and never writing the .md - the
// cancellation arm of the deliberate json-then-md ordering.
func TestWriteFilesCanceledAfterJSONSkipsMarkdown(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "report-2026-07-11T15-04-05Z")
	ctx := &pathExistsCancelCtx{Context: context.Background(), path: base + ".json"}
	r := &Report{GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC), Totals: map[string]int{}}

	err := r.WriteFiles(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFiles error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "report markdown render interrupted") {
		t.Errorf("error = %q, want the markdown-render stage context", err)
	}
	if _, statErr := os.Stat(base + ".json"); statErr != nil {
		t.Errorf("JSON half must be committed before the cancellation checkpoint fires: %v", statErr)
	}
	if _, statErr := os.Stat(base + ".md"); statErr == nil {
		t.Error("markdown half must not be written after a post-JSON cancellation")
	}
}

// countingCancelCtx is a context whose Err flips to context.Canceled from the
// after-th Err call onward, deterministically landing a cancellation at a
// specific interrupted checkpoint. WriteFiles polls Err via interrupted and
// never selects on Done; context.Cause falls back to Err for a non-cancelCtx
// context (the same contract pathExistsCancelCtx relies on).
type countingCancelCtx struct {
	context.Context
	after int
	calls int
}

func (c *countingCancelCtx) Err() error {
	c.calls++
	if c.calls >= c.after {
		return context.Canceled
	}
	return nil
}

// TestWriteFilesCanceledBeforeJSONRenderWritesNothing pins the report-render
// cancellation checkpoint (the one WriteFiles stage no existing test
// reaches): a cancellation observed after the stem probe but before the JSON
// half is rendered stops the pipeline with the report-render stage error and
// writes nothing - the report dir is never created. Err call #1 is the
// report-write checkpoint, #2 the single stem-probe round (empty dir), so
// flipping at call 3 lands exactly on the report-render checkpoint.
func TestWriteFilesCanceledBeforeJSONRenderWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	ctx := &countingCancelCtx{Context: context.Background(), after: 3}
	r := &Report{GeneratedAt: time.Date(2026, 7, 11, 15, 4, 5, 0, time.UTC), Totals: map[string]int{}}

	err := r.WriteFiles(ctx, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFiles error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "report render interrupted") {
		t.Errorf("error = %q, want the report-render stage context", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("report dir stat = %v, want absent (nothing written before the JSON render)", statErr)
	}
}

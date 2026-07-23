package audit

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReportPathsRedactedFromLogsAndErrors pins the report pipeline's
// credential posture: report.dir is a secret-capable config value (config
// expansion can place an allowlisted ${SEADEX_SCOUT_*} secret in any string
// field), so neither the pipeline's slog records (shipped to Loki) nor its
// returned errors (logged by main) may carry the configured directory value.
// Filesystem calls keep the real path — the report pair is still written to
// the configured directory — only the diagnostics are redacted.
func TestReportPathsRedactedFromLogsAndErrors(t *testing.T) {
	const sentinel = "sekret-passkey-sentinel"

	t.Run("successful write logs no report path", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), sentinel)
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		r := &Report{GeneratedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}

		if err := r.WriteFiles(t.Context(), dir, log); err != nil {
			t.Fatalf("WriteFiles: %v", err)
		}

		if got := buf.String(); strings.Contains(got, sentinel) {
			t.Errorf("report logs leak the report.dir value: %s", got)
		}
		if !strings.Contains(buf.String(), "report written") {
			t.Errorf("missing the report-written success record: %s", buf.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "report-2026-07-01T12-00-00Z.md")); err != nil {
			t.Errorf("report markdown not written to the real configured dir: %v", err)
		}
	})

	t.Run("write failure error and logs carry no report path", func(t *testing.T) {
		// The configured dir is a regular file: cleanup, stem probing, and
		// the writes all fail with *os.PathError values embedding the path.
		blocker := filepath.Join(t.TempDir(), sentinel)
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		r := &Report{GeneratedAt: time.Now().UTC()}

		err := r.WriteFiles(t.Context(), blocker, log)

		if err == nil {
			t.Fatal("WriteFiles(dir is a regular file) = nil, want error")
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("WriteFiles error leaks the report.dir value: %v", err)
		}
		if got := buf.String(); strings.Contains(got, sentinel) {
			t.Errorf("report logs leak the report.dir value on failure: %s", got)
		}
	})

	t.Run("lock failure error carries no report path", func(t *testing.T) {
		// MkdirAll fails on the sentinel-named intermediate component, so the
		// *os.PathError carries an ancestor of dir rather than dir itself;
		// the ancestor redaction must still mask it.
		blocker := filepath.Join(t.TempDir(), sentinel)
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := AcquireReportLock(filepath.Join(blocker, "reports"))

		if err == nil {
			t.Fatal("AcquireReportLock(parent is a regular file) = nil, want error")
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("AcquireReportLock error leaks the report.dir value: %v", err)
		}
	})
}

// TestRedactPathErrRedactsMessageAndPreservesCause pins redactPathErr's
// documented errors.Is/As contract, not just its rendered text: the redacted
// wrapper must keep the original cause reachable so shutdown/errno
// classification survives the masking. A future simplification that preserves
// the redacted message but drops the cause would keep the rendered-text
// redaction tests green while silently breaking errors.Is(os.ErrPermission)
// and errors.As(*os.PathError) for every report consumer.
func TestRedactPathErrRedactsMessageAndPreservesCause(t *testing.T) {
	const dir = "/config/sekret-passkey/reports"
	cause := &os.PathError{Op: "open", Path: dir + "/report.json", Err: os.ErrPermission}

	got := redactPathErr(dir, cause)

	if got == nil {
		t.Fatal("redactPathErr() = nil, want a wrapped error")
	}
	if strings.Contains(got.Error(), "sekret-passkey") {
		t.Errorf("redactPathErr() leaked report.dir in %q", got)
	}
	if !strings.Contains(got.Error(), redactedPath) {
		t.Errorf("redactPathErr() = %q, want the %q marker", got, redactedPath)
	}
	if !errors.Is(got, os.ErrPermission) {
		t.Errorf("errors.Is(redactPathErr(), os.ErrPermission) = false")
	}
	var pathErr *os.PathError
	if !errors.As(got, &pathErr) || pathErr != cause {
		t.Errorf("errors.As(redactPathErr(), *os.PathError) = %v, want original cause %v", pathErr, cause)
	}
	if redactPathErr(dir, nil) != nil {
		t.Error("redactPathErr(nil) must remain nil")
	}
	clean := errors.New("clean diagnostic")
	if unchanged := redactPathErr(dir, clean); unchanged != clean {
		t.Errorf("redactPathErr(clean error) = %v, want the original error identity", unchanged)
	}
}

package cycle

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTryReportLockRefusesConcurrentRun pins the concurrency refusal: a second
// acquire while the lock is held returns ErrReportRunning with the exact
// message the report subcommand surfaces, and never blocks.
func TestTryReportLockRefusesConcurrentRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	release, err := TryReportLock(dir)
	if err != nil {
		t.Fatalf("first TryReportLock: %v", err)
	}

	_, err = TryReportLock(dir)
	if !errors.Is(err, ErrReportRunning) {
		t.Fatalf("second TryReportLock = %v, want ErrReportRunning", err)
	}
	if err.Error() != "another report is already running" {
		t.Errorf("refusal message = %q, want %q", err.Error(), "another report is already running")
	}

	release()
	release2, err := TryReportLock(dir)
	if err != nil {
		t.Fatalf("TryReportLock after release = %v, want success", err)
	}
	release2()
}

// TestTryReportLockCreatesOwnerOnlyDir pins the created report dir's mode: the
// reports it will hold enumerate the operator's library and can carry
// private-tracker page links, so the dir the lock creates is owner-only, the
// same mode the report writer applies (least privilege, CWE-732).
func TestTryReportLockCreatesOwnerOnlyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	release, err := TryReportLock(dir)
	if err != nil {
		t.Fatalf("TryReportLock: %v", err)
	}
	defer release()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat report dir: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("report dir mode = %o, want 0700", got)
	}
}

func TestTryReportLockReportsMkdirError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "reports")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := TryReportLock(filepath.Join(blocker, "sub"))

	if err == nil {
		t.Fatal("TryReportLock must fail when the report dir cannot be created")
	}
	if !strings.Contains(err.Error(), "create report dir") {
		t.Errorf("error = %q, want it wrapped with the create-report-dir context", err)
	}
	if errors.Is(err, ErrReportRunning) {
		t.Error("a mkdir failure must not be reported as a concurrent-run refusal")
	}
}

func TestTryReportLockReportsOpenError(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, reportLockName)
	if err := os.Mkdir(lockPath, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := TryReportLock(dir)

	if err == nil {
		t.Fatal("TryReportLock must fail when report.lock is a directory")
	}
	if !strings.Contains(err.Error(), "report lock") {
		t.Errorf("error = %q, want it wrapped with the report-lock context", err)
	}
	if errors.Is(err, ErrReportRunning) {
		t.Error("an open failure must not be reported as a concurrent-run refusal")
	}
}

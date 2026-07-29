package cycle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cplieger/scheduler/v2"
)

// reportLockName is the flock target inside the report dir that serializes
// report runs (see TryReportLock).
const reportLockName = "report.lock"

// ErrReportRunning is returned by TryReportLock when another report run already
// holds the report lock. The report subcommand refuses to run rather than
// racing the other run onto the same timestamped filename pair; main classifies
// it as the designed coalescing outcome (WARN, exit 0), beside the queued/
// skipped outcomes RunOnce and RunLoop report.
var ErrReportRunning = errors.New("another report is already running")

// TryReportLock takes an exclusive, non-blocking flock on report.lock in dir
// (creating dir as needed, owner-only like the report pairs written into it)
// and returns a release func. It is held for a report run's whole
// generate+write, so two concurrent report runs - which could finish within the
// same UTC second and target the same report-<timestamp>.{md,json} pair -
// cannot interleave: the second run gets ErrReportRunning and refuses (never
// blocks or waits). A strictly-sequential same-second rerun does not overwrite
// either: the writer probes a deterministic -2/-3/... suffix for its pair stem
// while the lock is held. The flock rides scheduler.TryLock: not-acquired is
// reported without error (mapped to ErrReportRunning here), the kernel releases
// the lock if the process dies (no stale-lock state), and the lock file is left
// in place on release (unlinking it would open a window where two runs flock
// different inodes and both proceed) holding only the current holder's
// acquisition timestamp.
//
// It lives here rather than beside the report generator because exclusive-run
// coordination is this package's concern: every entry point (the daemon tick,
// an exec'd poll, a report run) now takes its exclusive lock and reads its
// run-outcome vocabulary from one home, and the report generator stays a
// read-only renderer with no process-lifecycle knowledge (and no scheduler
// dependency).
//
// The returned errors carry the real dir: it is the secret-capable report.dir
// config value, and the caller applies the report package's redaction policy
// (audit.RedactReportDirErr) before the error reaches a log. That direction was
// chosen over injecting a redaction func here, so the policy stays with the
// package that owns report.dir instead of being re-stated by a coordination
// leaf.
func TryReportLock(dir string) (func(), error) {
	if err := makeReportDir(dir); err != nil {
		return nil, fmt.Errorf("create report dir: %w", err)
	}
	path := filepath.Join(dir, reportLockName)
	lock, ok, err := scheduler.TryLock(path)
	if err != nil {
		return nil, fmt.Errorf("report lock %s: %w", reportLockName, err)
	}
	if !ok {
		return nil, ErrReportRunning
	}
	return lock.Unlock, nil
}

// makeReportDir creates dir owner-only, with the mode set EXPLICITLY rather
// than left to whatever os.MkdirAll's perm argument survives: MkdirAll's mode
// is filtered by the process umask and, on a filesystem carrying a default ACL
// on the parent, by the inherited ACL mask - which is how a dir requested at
// 0700 lands at 0770 (observed on this repo's own volume, where a t.TempDir()
// under it produces a group-readable dir). A report dir holds enumerations of
// the operator's whole library plus private-tracker page links, so the created
// mode is pinned with an explicit Chmod that no umask or inherited ACL can
// widen (least privilege, CWE-732).
//
// A PRE-EXISTING dir is deliberately left alone: /config/reports may be an
// operator-created directory (or a bind-mounted volume) whose mode is theirs to
// choose - possibly deliberately group-readable so another container can ship
// the reports - and silently narrowing it on every report run would break that
// deployment. So only the dir this call creates gets its mode pinned.
func makeReportDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), dirMode); err != nil {
		return err
	}
	switch err := os.Mkdir(dir, dirMode); {
	case err == nil:
		return os.Chmod(dir, dirMode)
	case errors.Is(err, fs.ErrExist):
		return nil
	default:
		return err
	}
}

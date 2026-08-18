package cycle

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/seadex-scout/internal/reportfs"
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
// and returns a release func.
func TryReportLock(dir string) (func(), error) {
	if err := reportfs.MakeDir(dir); err != nil {
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

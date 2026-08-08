// Package reportfs owns the on-disk privacy rule for the report artifacts: the
// directory and the pairs written into it are owner-only, with the created mode
// pinned by an explicit Chmod that no process umask and no inherited default
// ACL can widen (least privilege, CWE-732). A PRE-EXISTING directory is
// deliberately left alone.
//
// It is a stdlib-only leaf because the report directory has two creators - the
// report lock (internal/cycle, which must create the dir before flocking inside
// it) and the report writer (internal/audit) - and neither may import the
// other: cycle keeps process-lifecycle knowledge and the scheduler dependency
// out of the report generator. Stating the rule in both places is what let the
// weaker mechanism (MkdirAll's umask-filtered perm argument) survive on one of
// the two paths.
package reportfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// DirMode is the mode a report directory this app creates is pinned to.
	DirMode = 0o700
	// FileMode is the mode every written report half carries.
	FileMode = 0o600
)

// MakeDir creates dir owner-only, with the mode set EXPLICITLY rather than left
// to whatever os.MkdirAll's perm argument survives: MkdirAll's mode is filtered
// by the process umask and, on a filesystem carrying a default ACL on the
// parent, by the inherited ACL mask - which is how a dir requested at 0700
// lands at 0770 (observed on this repo's own volume, where a t.TempDir() under
// it produces a group-readable dir). A report dir holds enumerations of the
// operator's whole library plus private-tracker page links, so the created mode
// is pinned with an explicit Chmod that no umask or inherited ACL can widen
// (least privilege, CWE-732).
//
// A PRE-EXISTING dir is deliberately left alone: /config/reports may be an
// operator-created directory (or a bind-mounted volume) whose mode is theirs to
// choose - possibly deliberately group-readable so another container can ship
// the reports - and silently narrowing it on every report run would break that
// deployment. So only the dir this call creates gets its mode pinned.
func MakeDir(dir string) error {
	// Clean FIRST: a configured report.dir carrying a trailing separator
	// ("/config/reports/") makes filepath.Dir return the report dir ITSELF, so
	// MkdirAll creates the leaf at the umask/ACL-filtered mode and the os.Mkdir
	// below then reports fs.ErrExist - skipping the explicit Chmod that is this
	// function's whole point. Cleaning restores the parent/leaf split the two
	// calls assume.
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(filepath.Dir(dir), DirMode); err != nil {
		return err
	}
	switch err := os.Mkdir(dir, DirMode); {
	case err == nil:
		return os.Chmod(dir, DirMode)
	case errors.Is(err, fs.ErrExist):
		return nil
	default:
		return err
	}
}

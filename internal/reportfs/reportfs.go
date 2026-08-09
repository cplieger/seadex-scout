// Package reportfs owns the on-disk privacy rule for the report artifacts: the
// directory and the pairs written into it are owner-only, with the created mode
// pinned by an explicit Chmod so it is EXACT rather than whatever a process
// umask or an inherited default ACL leaves of MkdirAll's requested mode. Such
// filtering only ever removes bits, so the risk it guards is a directory
// narrowed past traversability, not a widened one (see MakeDir); least privilege
// (CWE-732) is the other half. A PRE-EXISTING directory is deliberately left
// alone.
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
// to whatever os.MkdirAll's perm argument survives. MkdirAll's mode is a
// REQUEST that the process umask (and, on a filesystem carrying a default ACL on
// the parent, the inherited ACL mask) filters, and filtering only ever REMOVES
// bits: measured on this toolchain, MkdirAll(0700) under umask 0000 yields 0700
// and never 0770, so a report dir cannot be silently widened this way.
//
// What it CAN be is narrowed past usefulness: the same measurement under umask
// 0177 yields 0600, a directory with no execute bit and therefore no traversal,
// which fails every later report write. The explicit Chmod is what makes the
// created mode EXACT in both directions - the guarantee neither MkdirAll's
// contract nor atomicfile's WithMkdirMode offers, since both pass the mode to
// MkdirAll and inherit its filtering. A report dir holds enumerations of the
// operator's whole library plus private-tracker page links, so pinning it is
// least privilege (CWE-732) plus a working directory.
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

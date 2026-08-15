// Package reportfs owns the on-disk privacy rule for the report artifacts: the
// directory and the pairs written into it are owner-only, with the created
// directory's mode ENFORCED - set on an open handle and then read back from that
// same handle - so it is what the filesystem stored rather than what the process
// asked for. Both directions are live: umask and default-ACL filtering only ever
// REMOVE bits (a report dir narrowed past traversability), while an inheritable
// ACE ADDS them (a report dir readable by a group that should not see it, least
// privilege / CWE-732). A PRE-EXISTING directory is deliberately left alone.
package reportfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/cplieger/atomicfile/v2"
)

const (
	// DirMode is the mode a report directory this app creates is pinned to.
	DirMode = 0o700
	// FileMode is the mode every written report half carries.
	FileMode = 0o600
)

// MakeDir creates dir owner-only, with the mode ENFORCED on the directory this
// call creates rather than left to whatever os.MkdirAll's perm argument
// survives. MkdirAll's mode is a REQUEST, and so is a plain os.Chmod: both hand
// the mode to the kernel and neither looks at the result.
func MakeDir(dir string) error {
	// Clean FIRST: a configured report.dir carrying a trailing separator
	// ("/config/reports/") makes filepath.Dir return the report dir ITSELF, so
	// MkdirAll creates the leaf at the umask/ACL-filtered mode and the os.Mkdir
	// below then reports fs.ErrExist - skipping the mode enforcement that is
	// this function's whole point.
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(filepath.Dir(dir), DirMode); err != nil {
		return err
	}
	switch err := os.Mkdir(dir, DirMode); {
	case err == nil:
		return enforceDirMode(dir)
	case errors.Is(err, fs.ErrExist):
		return nil
	default:
		return err
	}
}

// enforceDirMode pins the mode of the report directory THIS call created and
// proves the filesystem stored it, returning atomicfile's ErrModeNotStored
// (naming both modes) when it kept something else.
func enforceDirMode(dir string) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = atomicfile.EnforceMode(f, DirMode)
	return err
}

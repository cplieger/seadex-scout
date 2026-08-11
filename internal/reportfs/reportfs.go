// Package reportfs owns the on-disk privacy rule for the report artifacts: the
// directory and the pairs written into it are owner-only, with the created
// directory's mode ENFORCED - set on an open handle and then read back from that
// same handle - so it is what the filesystem stored rather than what the process
// asked for. Both directions are live: umask and default-ACL filtering only ever
// REMOVE bits (a report dir narrowed past traversability), while an inheritable
// ACE ADDS them (a report dir readable by a group that should not see it, least
// privilege / CWE-732). A PRE-EXISTING directory is deliberately left alone.
//
// It is a near-leaf - the stdlib plus atomicfile, which the app already depends
// on for its state file - because the report directory has two creators - the
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
//
// The request is filtered in one direction and overridden in the other, so
// pinning it needs a read-back rather than a second request. Filtering only ever
// REMOVES bits: measured on this toolchain, MkdirAll(0700) under umask 0177
// yields 0600 - a directory with no execute bit and therefore no traversal,
// which fails every later report write. Widening comes from an inheritable ACE
// the mode argument cannot speak to: measured on a ZFS nfs4acl dataset, a 0o700
// mkdir stores 0770, and on Linux a directory created under a setgid parent
// inherits setgid nobody asked for. A report dir holds enumerations of the
// operator's whole library plus private-tracker page links, so a silently
// group-readable one is a disclosure (least privilege, CWE-732) that the old
// unverified Chmod reported as success.
//
// A PRE-EXISTING dir is deliberately left alone: /config/reports may be an
// operator-created directory (or a bind-mounted volume) whose mode is theirs to
// choose - possibly deliberately group-readable so another container can ship
// the reports - and silently narrowing it on every report run would break that
// deployment. So only the dir this call creates reaches enforceDirMode.
func MakeDir(dir string) error {
	// Clean FIRST: a configured report.dir carrying a trailing separator
	// ("/config/reports/") makes filepath.Dir return the report dir ITSELF, so
	// MkdirAll creates the leaf at the umask/ACL-filtered mode and the os.Mkdir
	// below then reports fs.ErrExist - skipping the mode enforcement that is
	// this function's whole point. Cleaning restores the parent/leaf split the
	// two calls assume.
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
//
// It takes a HANDLE, and that is the substance rather than a detail: a chmod of
// the pathname followed by a stat of the pathname can pin one directory and
// certify another if the name is swapped in between, whereas the fchmod(2) and
// fstat(2) atomicfile.EnforceMode issues ride one descriptor that no rename can
// redirect. Refusing is the right end state for this app: a report dir whose
// mode the filesystem will not store is one whose contents - the operator's
// whole library enumeration plus private-tracker links - would be readable by
// somebody else, and the report run that would have filled it is cheap to fail.
//
// The open flags carry the rest of the guarantee. O_NOFOLLOW has the KERNEL
// refuse a symlink planted at the final component instead of following it into a
// chmod of another directory, which no check-then-open sequence can do without a
// race; O_DIRECTORY refuses a non-directory occupant; O_NONBLOCK is what keeps a
// planted FIFO from stalling this call indefinitely inside open(2).
func enforceDirMode(dir string) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = atomicfile.EnforceMode(f, DirMode)
	return err
}

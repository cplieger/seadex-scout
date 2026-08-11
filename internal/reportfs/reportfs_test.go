package reportfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMakeDirEnforcesTheModeOfTheDirectoryItCreated pins that the created
// report directory's mode is what the filesystem STORED, not what MakeDir
// asked for. The distinction is the whole reason the chmod became an
// atomicfile.EnforceMode: a mode argument is a request, and mkdir(2), chmod(2)
// and open(2) all report success having stored something else.
//
// The widening here is REAL rather than mocked: Linux propagates S_ISGID from a
// setgid parent to a new subdirectory, so os.Mkdir(dir, 0o700) genuinely stores
// a mode nobody asked for, and a report directory carrying setgid hands its
// contents - the operator's whole library enumeration plus private-tracker page
// links - to whoever is in the parent's group. The witness below SKIPS the test
// as invalid rather than letting it pass vacuously if the kernel stops doing
// that, because on a filesystem that honours every mode request there is
// nothing here for enforcement to correct.
func TestMakeDirEnforcesTheModeOfTheDirectoryItCreated(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}

	witness := filepath.Join(parent, "witness")
	if err := os.Mkdir(witness, DirMode); err != nil {
		t.Fatal(err)
	}
	wfi, err := os.Lstat(witness)
	if err != nil {
		t.Fatal(err)
	}
	if wfi.Mode()&os.ModeSetgid == 0 {
		t.Skipf("kernel did not widen a %#o mkdir under a setgid parent (got %v); "+
			"this filesystem stores every requested mode, so the test cannot tell "+
			"an enforced mode from a requested one", os.FileMode(DirMode), wfi.Mode())
	}

	dir := filepath.Join(parent, "reports")
	if err := MakeDir(dir); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fi.Mode(), os.ModeDir|DirMode; got != want {
		t.Fatalf("created report dir mode = %v, want %v: the mode it created was not enforced",
			got, want)
	}
}

// TestMakeDirLeavesAWiderPreExistingDirectoryAlone pins the deliberate
// asymmetry this app needs and atomicfile.EnsurePrivateDir would break:
// /config/reports may be an operator-created directory or a bind-mounted
// volume, possibly group-readable on purpose so another container can ship the
// reports, and MakeDir must accept it AS IS. Enforcement applies only to a
// directory this call created.
func TestMakeDirLeavesAWiderPreExistingDirectoryAlone(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "reports")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Mkdir applies the umask; force the wide mode explicitly.
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0o750 {
		t.Fatalf("INVALID setup: pre-existing dir mode = %v, want 0750", before.Mode().Perm())
	}

	if err := MakeDir(dir); err != nil {
		t.Fatalf("MakeDir refused a wider pre-existing report dir: %v", err)
	}
	after, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Mode().Perm(); got != 0o750 {
		t.Fatalf("pre-existing report dir mode = %v, want 0750 left untouched", got)
	}
}

// TestMakeDirCreatesOwnerOnly is the plain create path on a filesystem that
// stores what it is asked, where enforcement is a no-op and the outcome is
// still the contract.
func TestMakeDirCreatesOwnerOnly(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "reports")
	if err := MakeDir(dir); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatal("not a directory")
	}
	if got := fi.Mode().Perm(); got != DirMode {
		t.Fatalf("mode = %v, want %v", got, os.FileMode(DirMode))
	}
}

// TestMakeDirCleansATrailingSeparator pins the reason for the leading
// filepath.Clean: without it filepath.Dir returns the report dir itself,
// MkdirAll creates the leaf at the filtered mode, and the os.Mkdir that follows
// reports fs.ErrExist - taking the pre-existing arm and skipping enforcement
// for a directory this call actually created.
func TestMakeDirCleansATrailingSeparator(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir := filepath.Join(base, "reports")
	if err := MakeDir(dir + string(os.PathSeparator)); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != DirMode {
		t.Fatalf("mode = %v, want %v: the trailing separator skipped enforcement",
			got, os.FileMode(DirMode))
	}
}

// TestEnforceDirModeRefusesASymlinkInsteadOfChmodingItsTarget pins the half of
// the change the mode assertions cannot see. os.Chmod resolves the pathname, so
// a symlink planted where the report directory should be made the old code
// chmod whatever it pointed at - 0700 on another principal's directory, applied
// by the report run. The handle is opened O_NOFOLLOW|O_DIRECTORY, so the kernel
// refuses the name outright and the victim keeps its mode.
//
// MakeDir itself reaches this only for a directory its own os.Mkdir just
// created, so the refusal guards a swap racing that step rather than a shape the
// call path can reach on its own; the helper is where it is testable.
func TestEnforceDirModeRefusesASymlinkInsteadOfChmodingItsTarget(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	victim := filepath.Join(base, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "reports")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	if err := enforceDirMode(link); err == nil {
		t.Fatal("a symlink at the report dir name was accepted; want the kernel refusal")
	}
	fi, err := os.Lstat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Fatalf("victim mode = %v, want 0755 untouched: the enforcement followed the symlink", got)
	}
}

// Package state persists seadex-scout's cross-cycle cache as a single JSON file
// written atomically: the last library snapshot (for diffing), the cached Fribb
// map plus its HTTP validators, the AniList fallback memo, and the degradation
// streak counters. It holds NO finding state - internal/notify reports findings
// as STATE and rebuilds its whole set from each pass. A missing file loads as
// an empty state (a cold start), never an error.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/jsoncap/v2"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
)

const (
	// maxStateBytes bounds the state file on read AND write (Save refuses to
	// persist what Load would reject).
	maxStateBytes = 32 << 20
	// stateSizeWarnBytes is the pre-cliff warning threshold (80% of maxStateBytes):
	// crossing the bound refuses every subsequent Save, so writeState warns earlier.
	stateSizeWarnBytes = maxStateBytes / degradation.SizeWarnDenominator * degradation.SizeWarnNumerator
	// dirMode / fileMode are applied to the created state directory and file.
	dirMode  = 0o700
	fileMode = 0o600
)

// SchemaVersion is the schema version Save stamps into State.Version on every
// write. Bump it when a persisted member moves or is renamed incompatibly, so
// a future loader can detect the old shape and migrate (or refuse) explicitly
// instead of silently zero-loading it. A file whose version field is absent or
// zero is a legacy envelope written before versioning and loads unchanged.
const SchemaVersion = 1

// State is the persisted cross-cycle cache.
//
// It carries NO finding state.
type State struct {
	// The four streak counters below are persisted DATA, not policy.
	Memo    match.Memo    `json:"anilist_memo"`
	Mapping mapping.Cache `json:"mapping"`
	// ShrunkWalksByArr counts, PER ARR, consecutive reconciles the library shrink guard
	// judged that arr's fresh item count a suspicious truncation and carried its prior
	// items forward instead.
	ShrunkWalksByArr map[string]int   `json:"shrunk_walks_by_arr,omitempty"`
	Library          library.Snapshot `json:"library"`
	// SeadexFailures counts consecutive cycles whose SeaDex fetch failed, whichever
	// pre-compare gate closed the cycle: the scout records the fetch outcome ahead of
	// gate selection, so a coinciding failure cannot hide the outage.
	SeadexFailures int `json:"seadex_failures,omitempty"`
	// AniListDegraded counts consecutive COMPLETED cycles whose matching left
	// AniList lookups incomplete (match.Result.Degraded), preserving the
	// affected entries' prior findings. Owner: recordAniListDegradation.
	AniListDegraded int `json:"anilist_degraded,omitempty"`
	// PartialWalks counts consecutive COMPLETED cycles whose library walk came
	// back partial (per-series episode-fetch failures left Failed placeholder
	// items the compare excluded).
	PartialWalks int `json:"partial_walks,omitempty"`
	// Version is the persisted envelope's schema version, stamped with
	// SchemaVersion by every Save (on the shallow copy it writes; the
	// caller's State is never mutated).
	Version int `json:"version,omitempty"`
}

// Store loads and saves the state file at a fixed path.
//
// A Store is NOT safe for concurrent use.
type Store struct {
	log  *slog.Logger
	path string
	// unsupportedVersion remembers a newer-than-supported schema version the
	// last Load found at the live path.
	unsupportedVersion int
	// loadFailed remembers that the last Load did not leave the live path in a state
	// Save may replace: it either failed WITHOUT classifying the file, or classified an
	// over-cap/corrupt payload it could not preserve. While set, Save is refused,
	// because the unread bytes may be recoverable and an unpreserved corrupt file is the
	// only forensic evidence. It clears as soon as a Load succeeds.
	loadFailed bool
	readOnly   bool
}

// NewStore returns a Store for the given state-file path. logger may be nil.
func NewStore(path string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{log: logger, path: path}
}

// NewReadOnlyStore returns a Store for flows documented read-only on the
// state file (the one-shot report): Load reports corruption without
// quarantining, leaving the file in place for the daemon's own Load to
// quarantine and surface on the container's log stream.
func NewReadOnlyStore(path string, logger *slog.Logger) *Store {
	st := NewStore(path, logger)
	st.readOnly = true
	return st
}

// staleTempMaxAge is how old an orphaned atomic-write temp must be before Load
// reaps it. A live pending temp is seconds old (Save encodes and commits in one
// pass), so an hour cannot race a concurrent writer in another process.
const staleTempMaxAge = time.Hour

// schemaVersion independently decodes the persisted envelope's schema version
// discriminator straight from the wire bytes, reporting the effective (last)
// decoded version - zero when the key is absent, which the envelope's
// contract treats as the legacy pre-version shape (see SchemaVersion) -
// and any wire-level failure.
func schemaVersion(data []byte) (version int, err error) {
	dec := jsoncap.NewDecoder(bytes.NewReader(data), 0)
	err = dec.Object(func(key string) error {
		if !strings.EqualFold(key, "version") {
			return dec.Skip()
		}
		var raw json.RawMessage
		if decodeErr := dec.Decode(&raw); decodeErr != nil {
			return decodeErr
		}
		var decoded *int
		if unmarshalErr := json.Unmarshal(raw, &decoded); unmarshalErr != nil {
			return unmarshalErr
		}
		if decoded == nil {
			return errors.New("schema version must be an integer")
		}
		if *decoded < 0 {
			// The documented legacy envelope's version is absent or zero, and
			// Save only ever stamps SchemaVersion - a negative occurrence can
			// only be corruption or tampering.
			return fmt.Errorf("invalid negative schema version %d", *decoded)
		}
		version = *decoded
		return nil
	})
	if err != nil {
		return 0, err
	}
	if endErr := dec.End(); endErr != nil {
		return 0, endErr
	}
	return version, nil
}

// Load reads and decodes the state file. A missing file returns a zero State
// and no error (cold start); a present but corrupt or oversized file is
// quarantined where possible - see maybeQuarantine for the rename-failed
// fallback, which blocks Save instead - and returns the error so the caller
// can decide (the scout logs it and starts cold).
func (s *Store) Load(ctx context.Context) (State, error) {
	// ONE os.Root spans the whole read -> classify -> preserve decision.
	root, err := os.OpenRoot(filepath.Dir(s.path))
	if root != nil {
		defer func() {
			if clErr := root.Close(); clErr != nil {
				s.log.Warn("could not close state directory handle", "dir", filepath.Dir(s.path), "error", clErr)
			}
		}()
	}
	// Sweep stale temps through the SAME root the read -> classify -> preserve
	// decision is pinned to.
	if err == nil && !s.readOnly {
		// Unlike the ambient variant this reports counts instead of logging its
		// own Info summary, so Load narrates the reclaim itself.
		sweep, cleanErr := atomicfile.CleanupStaleTempsInRoot(ctx, root, staleTempMaxAge, atomicfile.WithLogger(s.log))
		if cleanErr != nil {
			s.log.Warn("could not clean stale atomic-write temp files", "dir", filepath.Dir(s.path), "error", cleanErr)
		}
		if sweep.Removed > 0 || sweep.Failed > 0 {
			s.log.Info("reclaimed stale atomic-write temp files", "dir", filepath.Dir(s.path),
				"removed", sweep.Removed, "failed", sweep.Failed)
		}
	}
	var data []byte
	if err == nil {
		data, err = s.readState(ctx, root)
	}
	if err != nil {
		// A cold start (a missing file or directory) is reported as a nil
		// error, so it returns the zero State exactly like a successful read
		// of an empty state.
		return State{}, s.classifyReadFailure(root, err)
	}
	s.loadFailed = false
	st, err := s.decode(root, data)
	if err != nil {
		return State{}, err
	}
	s.unsupportedVersion = 0
	attrs := []any{
		"path", s.path,
		"library_items", len(st.Library.Items),
		"mapping_records", st.Mapping.IndexedRecords(),
		"memo_entries", len(st.Memo.Entries),
	}
	if !st.Library.TakenAt.IsZero() {
		// Surface the persisted snapshot's age: the indexer feed's title synthesis reads
		// this snapshot, so diagnosing a stale title needs to see how old it is.
		age := max(time.Since(st.Library.TakenAt), 0)
		attrs = append(attrs, "library_age", age.Round(time.Second).String())
	}
	s.log.Info("state loaded", attrs...)
	return st, nil
}

// classifyReadFailure applies Load's read-failure policy to a failed open or
// read and returns the error Load reports: nil for the cold start a missing
// file (or a missing state DIRECTORY, which os.OpenRoot reports the same way)
// is, otherwise the wrapped failure.
func (s *Store) classifyReadFailure(root *os.Root, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		s.unsupportedVersion = 0
		s.loadFailed = false
		s.log.Info("no state file, starting cold", "path", s.path)
		return nil
	}
	if errors.Is(err, atomicfile.ErrFileTooLarge) || errors.Is(err, atomicfile.ErrNotRegular) {
		// Save enforces maxStateBytes and only ever writes a regular file, so an
		// oversized or non-regular file can only be foreign or corrupt; preserve it.
		s.maybeQuarantine(root)
		return fmt.Errorf("state: read %s: %w", s.path, err)
	}
	if s.foreignInode(root) {
		// A non-regular inode the confined open could not report as ErrNotRegular:
		// os.Root refuses an escaping symlink with an error carrying no sentinel.
		s.maybeQuarantine(root)
		return fmt.Errorf("state: read %s: %w", s.path, err)
	}
	// An UNCLASSIFIED read failure (EACCES, EIO, a cancelled read): the bytes at the live
	// path may be fully recoverable, so they are preserved like every classified failure
	// preserves its evidence.
	s.loadFailed = true
	return fmt.Errorf("state: read %s: %w", s.path, err)
}

// readState reads the state file through the os.Root Load holds open for the
// whole read/classify/preserve decision: the open is symlink-confined and
// O_NONBLOCK, so a redirected or blocking special file at the state path is an
// error Load can classify rather than an uninterruptible open
// (atomicfile.ReadBounded uses os.Open, which follows a symlink and blocks on
// a FIFO with no writer past both of its context checks). The caller owns the
// root's lifetime.
func (s *Store) readState(ctx context.Context, root *os.Root) ([]byte, error) {
	return atomicfile.ReadBoundedInRoot(ctx, root, filepath.Base(s.path), maxStateBytes)
}

// foreignInode reports whether the state path holds an inode the confined
// read can never turn into state: a non-regular inode, or a symlink that
// escapes the state directory. An in-root symlink to a regular file is NOT
// foreign - the confined read follows it - so a failure over one keeps the
// recoverable classification.
func (s *Store) foreignInode(root *os.Root) bool {
	if root == nil {
		return false
	}
	info, err := root.Lstat(filepath.Base(s.path))
	if err != nil {
		return false
	}
	if info.Mode().IsRegular() {
		return false
	}
	// A symlink that stays INSIDE the state directory is followed by the confined read
	// exactly like a regular file, so it is NOT an inode Save cannot replace. Resolving
	// through the root separates the cases: only an escaping link reports foreign.
	resolved, err := root.Stat(filepath.Base(s.path))
	if err != nil {
		return true
	}
	return !resolved.Mode().IsRegular()
}

// decode applies Load's corruption and schema-version policy to the raw state
// bytes, quarantining a corrupt payload (or, for a newer-schema file, setting
// the Save block instead) before returning the error. It preserves through the
// root Load read the bytes with, so the rename cannot land in a directory
// swapped in after the read.
func (s *Store) decode(root *os.Root, data []byte) (State, error) {
	// Save always emits valid UTF-8 JSON.
	if !utf8.Valid(data) {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: invalid UTF-8", s.path)
	}
	// Require a JSON object envelope before unmarshalling: json.Unmarshal accepts a
	// literal null into a struct, so a corrupt file holding "null" would load as a
	// silently-empty state instead of surfacing the corruption.
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: not a JSON object", s.path)
	}
	// The wire discriminator is decoded independently BEFORE the State unmarshal, on
	// every load: State.Version is never trusted, since Unmarshal may populate it from an
	// earlier duplicate key and accepts null into an int silently.
	wireVersion, err := schemaVersion(data)
	if err != nil {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: %w", s.path, err)
	}
	// An absent version key decodes as zero, which is below SchemaVersion,
	// so the legacy envelope takes the ordinary load path here.
	if wireVersion > SchemaVersion {
		// A file stamped by a newer binary (an image rollback): its members may have
		// moved, so field-by-field zero-loading is exactly the silent discard
		// SchemaVersion exists to prevent.
		s.unsupportedVersion = wireVersion
		return State{}, fmt.Errorf("state: decode %s: schema version %d is newer than this binary supports (%d)", s.path, wireVersion, SchemaVersion)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: %w", s.path, err)
	}
	return st, nil
}

// maybeQuarantine preserves a corrupt state file unless this Store belongs
// to a read-only flow, which must leave the live path untouched so the
// daemon's own Load detects and reports the corruption.
func (s *Store) maybeQuarantine(root *os.Root) {
	// Load positively classified the live file as corrupt, so a newer-schema block
	// remembered from an earlier Load no longer describes the file at the live path;
	// clear it so the next Save is judged against reality.
	s.unsupportedVersion = 0
	if s.readOnly {
		// Leaving the file in place IS the read-only flow's preservation, and
		// such a Store never saves anyway.
		s.log.Warn("corrupt state file left in place for the daemon to quarantine", "path", s.path)
		s.loadFailed = false
		return
	}
	s.loadFailed = !s.quarantine(root)
}

// quarantine preserves a corrupt state file beside the original so the decode
// failure can be examined after the next successful Save atomically replaces
// state.json, and reports whether that preservation succeeded. A repeat
// corruption overwrites the previous .corrupt copy (latest wins).
func (s *Store) quarantine(root *os.Root) bool {
	dir, base := filepath.Dir(s.path), filepath.Base(s.path)
	// Preserve through the very root Load's read was resolved against, never a freshly
	// opened one: the rename acts on the open directory handle that held the bytes just
	// read, so a component redirected after that open cannot move them elsewhere.
	if root == nil {
		s.log.Warn("could not open state directory to preserve corrupt state file", "dir", dir)
		return false
	}
	if err := root.Rename(base, base+".corrupt"); err != nil {
		s.log.Warn("could not preserve corrupt state file", "path", s.path, "error", err)
		return false
	}
	s.log.Warn("corrupt state file preserved for inspection", "path", s.path+".corrupt")
	return true
}

// Save atomically writes the state file, creating the parent directory if
// needed. It returns an error only when the data did not reach disk; a
// non-durable (unsynced) write is logged, not failed.
func (s *Store) Save(ctx context.Context, st *State) error {
	sanitized, err := s.prepareSave(ctx, st)
	if err != nil {
		return err
	}
	return s.writeState(ctx, &sanitized)
}

// ErrSavePreserved marks a Save that deliberately REFUSED to write in order to
// preserve the bytes already on disk: the newer-schema block (an image
// rollback must not discard state a later version wrote), the
// unclassified-read-failure block (a read that failed without classifying the
// file must not be overwritten by a cold envelope), and classified corruption
// the load could NOT preserve (the quarantine rename failed, so the live file
// is still the only forensic copy).
var ErrSavePreserved = errors.New("state preserved; save refused")

// prepareSave validates whether this Store may write (nil state, read-only,
// cancelled context, the newer-schema and unclassified-read-failure Save
// blocks - see Save's doc) and returns the sanitized, version-stamped shallow
// copy Save persists; the caller's State is never mutated.
func (s *Store) prepareSave(ctx context.Context, st *State) (State, error) {
	if st == nil {
		return State{}, fmt.Errorf("state: save %s: nil state (Save never writes a non-object state file)", s.path)
	}
	if s.readOnly {
		return State{}, fmt.Errorf("state: save %s: store is read-only", s.path)
	}
	if err := ctx.Err(); err != nil {
		return State{}, fmt.Errorf("state: save %s: %w", s.path, err)
	}
	if s.unsupportedVersion != 0 {
		return State{}, fmt.Errorf("state: save %s: blocked after loading newer schema version %d (supported %d): %w", s.path, s.unsupportedVersion, SchemaVersion, ErrSavePreserved)
	}
	if s.loadFailed {
		return State{}, fmt.Errorf("state: save %s: blocked after an unclassified read failure, or after corruption the load could not preserve (check for a blocked %s.corrupt); the on-disk state is preserved until a load can classify and preserve it: %w", s.path, s.path, ErrSavePreserved)
	}
	sanitized := *st
	sanitized.Version = SchemaVersion
	return sanitized, nil
}

// writeState owns Save's persistence phase: the pending-file lifecycle,
// encoding under the size bound, the atomic commit, and durability reporting.
func (s *Store) writeState(ctx context.Context, st *State) error {
	pf, err := atomicfile.NewPendingFile(ctx, s.path,
		atomicfile.WithLogger(s.log),
		atomicfile.WithMkdirMode(dirMode),
		atomicfile.WithMode(fileMode),
		// One byte beyond maxStateBytes for json.Encoder's trailing
		// newline, truncated away in encodeState (see its doc).
		atomicfile.WithMaxBytes(maxStateBytes+1))
	if err != nil {
		return fmt.Errorf("state: write %s: %w", s.path, err)
	}
	// Cleanup is a no-op after Commit (success or failure), so deferring it
	// covers every mid-write error path and a panic without double-removal.
	defer func() {
		if clErr := pf.Cleanup(); clErr != nil {
			s.log.Warn("could not remove pending state temp file", "path", pf.Name(), "error", clErr)
		}
	}()
	if encErr := encodeState(pf, st, s.path); encErr != nil {
		return encErr
	}
	if staged := pf.BytesWritten(); staged > stateSizeWarnBytes {
		s.log.Warn("state file approaching the size limit; a Save that exceeds it is refused and the persisted cache freezes",
			"path", s.path, "bytes", staged, "limit", maxStateBytes)
	}
	res, err := pf.Commit(ctx)
	if err != nil {
		return fmt.Errorf("state: write %s: %w", s.path, err)
	}
	if !res.Durable {
		s.log.Warn("state written but not durable", "path", s.path)
	}
	return nil
}

// encodeState serializes st into the pending temp file under Load's size
// bound and drops the encoder's trailing newline.
func encodeState(pf *atomicfile.PendingFile, st *State, path string) error {
	if encErr := json.NewEncoder(pf).Encode(st); encErr != nil {
		if errors.Is(encErr, atomicfile.ErrFileTooLarge) {
			return fmt.Errorf("state: encode %s: encoded state exceeds the %d-byte load limit (%w); keeping previous state file", path, maxStateBytes, encErr)
		}
		return fmt.Errorf("state: encode %s: %w", path, encErr)
	}
	if truncErr := pf.Truncate(pf.BytesWritten() - 1); truncErr != nil {
		return fmt.Errorf("state: write %s: %w", path, truncErr)
	}
	return nil
}

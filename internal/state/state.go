// Package state persists seadex-scout's cross-cycle cache as a single JSON file
// written atomically: the last library snapshot (for diffing), the cached Fribb
// map plus its HTTP validators, the AniList fallback memo, the finding dedupe
// records, and the flags marking that (and how completely) the dedupe table has
// been seeded. A missing file loads as an empty state (a cold start), never an
// error.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
)

const (
	// maxStateBytes bounds the state file on read AND write (Save refuses to
	// persist what Load would reject). An honest state file (library snapshot
	// + mapping cache + memo + dedupe records) runs ~10-20 MB, so 32 MB keeps
	// real headroom while fitting the 256 MiB deployment container: Load
	// holds the raw JSON and the decoded State simultaneously, so the cap
	// must leave room for both — a larger bound would let a valid at-cap file
	// OOM-kill the container during Load instead of degrading to the intended
	// clean cold start.
	maxStateBytes = 32 << 20
	// dirMode / fileMode are applied to the created state directory and file.
	dirMode  = 0o755
	fileMode = 0o644
)

// SchemaVersion is the schema version Save stamps into State.Version on every
// write. Bump it when a persisted member moves or is renamed incompatibly, so
// a future loader can detect the old shape and migrate (or refuse) explicitly
// instead of silently zero-loading it. A file whose version field is absent or
// zero is a legacy envelope written before versioning and loads unchanged.
const SchemaVersion = 1

// State is the persisted cross-cycle cache. Findings is keyed by dedupe key.
// Baselined records that the first successful compare has seeded the finding
// dedupe table, so a cold start (a fresh install or a lost cache) baselines the
// pre-existing backlog silently instead of alerting on every misaligned title
// at once.
type State struct {
	Findings map[string]report.Alerted `json:"findings,omitempty"`
	Memo     match.Memo                `json:"anilist_memo"`
	Mapping  mapping.Cache             `json:"mapping"`
	Library  library.Snapshot          `json:"library"`
	// ShrunkWalks counts consecutive cycles the scout's library shrink guard
	// rejected a fully-successful walk (an item count below half the prior
	// snapshot's) in favour of preserving findings. It persists across cycles
	// and restarts, resets to 0 on any walk that passes the guard, and mirrors
	// mapping.Cache.RejectedRefreshes so the scout can escalate its single
	// shrunk-walk log site after a sustained streak.
	ShrunkWalks int `json:"shrunk_walks,omitempty"`
	// SeadexFailures counts consecutive cycles the scout's upstream gate
	// skipped the compare after a failed SeaDex fetch, preserving findings.
	// It persists across cycles and restarts, resets to 0 on any successful
	// fetch, and mirrors ShrunkWalks (and mapping.Cache.RejectedRefreshes) so
	// the scout can escalate its single seadex-fetch-failed log site after a
	// sustained outage instead of degrading at WARN forever.
	SeadexFailures int `json:"seadex_failures,omitempty"`
	// Version is the persisted envelope's schema version, stamped with
	// SchemaVersion by every Save (on the shallow copy it writes; the
	// caller's State is never mutated). A file with the field absent or zero
	// loads as a legacy pre-version envelope, exactly like any other missing
	// field; a version NEWER than SchemaVersion is refused by Load (an image
	// rollback must not silently zero-load moved members and then overwrite
	// the newer-schema file); and a future member move or rename bumps
	// SchemaVersion so the old shape can be migrated (or refused) explicitly
	// instead of silently zero-loaded.
	Version   int  `json:"version,omitempty"`
	Baselined bool `json:"baselined,omitempty"`
	// BaselineIncomplete marks a baseline seeded from an incomplete cycle: a
	// partial walk (Failed placeholder items were excluded from the compare)
	// or an AniList-degraded match (transiently unresolved entries were
	// missing from the seed), so the seed covers only the items that walked
	// cleanly and mapped completely. While set, every successful cycle keeps
	// seeding silently instead of reporting - otherwise the affected items'
	// pre-existing backlog would burst as fresh notifications when they recover
	// - until the first complete cycle seeds the whole library and clears the
	// flag. It distinguishes an incomplete baseline (both flags set) from a
	// complete one (Baselined alone) and from a legacy pre-flag state file
	// (findings present, no flags), which must stay on the normal Report path.
	BaselineIncomplete bool `json:"baseline_incomplete,omitempty"`
}

// Store loads and saves the state file at a fixed path.
type Store struct {
	log      *slog.Logger
	path     string
	readOnly bool
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

// Load reads and decodes the state file. A missing file returns a zero State
// and no error (cold start); a present but corrupt or oversized file is
// quarantined and returns the error so the caller can decide (the scout logs
// it and starts cold).
func (s *Store) Load(ctx context.Context) (State, error) {
	// CleanupStaleTemps maps a missing dir to (0, nil) and logs its own
	// removal summary at Info through the supplied logger, so Load only
	// surfaces a readdir failure.
	if _, cleanErr := atomicfile.CleanupStaleTemps(filepath.Dir(s.path), staleTempMaxAge, atomicfile.WithLogger(s.log)); cleanErr != nil {
		s.log.Warn("could not clean stale atomic-write temp files", "dir", filepath.Dir(s.path), "error", cleanErr)
	}
	data, err := atomicfile.ReadBounded(ctx, s.path, maxStateBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.log.Info("no state file, starting cold", "path", s.path)
			return State{}, nil
		}
		if errors.Is(err, atomicfile.ErrFileTooLarge) {
			// Save enforces maxStateBytes, so an oversized file can only be
			// foreign or corrupt; preserve it like any other corruption.
			s.maybeQuarantine()
		}
		return State{}, fmt.Errorf("state: read %s: %w", s.path, err)
	}
	// Require a JSON object envelope before unmarshalling: json.Unmarshal
	// accepts a literal null into a struct, so a corrupt file holding "null"
	// would otherwise load as a silently-empty state (a fake cold start that
	// baselines findings and discards every cache) instead of surfacing the
	// corruption. Save can never produce anything but an object.
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		s.maybeQuarantine()
		return State{}, fmt.Errorf("state: decode %s: not a JSON object", s.path)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		s.maybeQuarantine()
		return State{}, fmt.Errorf("state: decode %s: %w", s.path, err)
	}
	if st.Version > SchemaVersion {
		// A file stamped by a newer binary (an image rollback): its members may
		// have moved, so field-by-field zero-loading is exactly the silent
		// discard SchemaVersion exists to prevent. Preserve the file and start
		// cold; rolling forward again finds it at .corrupt (latest wins).
		s.maybeQuarantine()
		return State{}, fmt.Errorf("state: decode %s: schema version %d is newer than this binary supports (%d)", s.path, st.Version, SchemaVersion)
	}
	attrs := []any{
		"path", s.path,
		"library_items", len(st.Library.Items),
		"mapping_records", len(st.Mapping.Records),
		"memo_entries", len(st.Memo.Entries),
		"findings", len(st.Findings),
	}
	if !st.Library.TakenAt.IsZero() {
		// Surface the persisted snapshot's age: the indexer feed's title
		// synthesis reads this snapshot (arr-independent, never a fresh
		// walk), so diagnosing a stale synthesized title needs to see how old
		// the snapshot backing it is. A legacy or walk-less state carries the
		// zero TakenAt and skips the attribute rather than logging a
		// nonsensical multi-century age.
		attrs = append(attrs, "library_age", time.Since(st.Library.TakenAt).Round(time.Second).String())
	}
	s.log.Debug("state loaded", attrs...)
	return st, nil
}

// maybeQuarantine preserves a corrupt state file unless this Store belongs
// to a read-only flow, which must leave the live path untouched so the
// daemon's own Load detects and reports the corruption.
func (s *Store) maybeQuarantine() {
	if s.readOnly {
		s.log.Warn("corrupt state file left in place for the daemon to quarantine", "path", s.path)
		return
	}
	s.quarantine()
}

// quarantine preserves a corrupt state file beside the original so the decode
// failure can be examined after the next successful Save atomically replaces
// state.json. Best-effort: a rename failure is logged at Warn, never escalated,
// and a repeat corruption overwrites the previous .corrupt copy (latest wins).
func (s *Store) quarantine() {
	dst := s.path + ".corrupt"
	if err := os.Rename(s.path, dst); err != nil {
		s.log.Warn("could not preserve corrupt state file", "path", s.path, "error", err)
		return
	}
	s.log.Warn("corrupt state file preserved for inspection", "path", dst)
}

// errStateTooLarge marks a Save encoding that would exceed maxStateBytes. It
// is returned by boundedWriter's Write and detected by Save (errors.Is) to
// produce the size-cap rejection while the previous state file stays intact.
var errStateTooLarge = errors.New("state: encoded state exceeds size limit")

// boundedWriter passes writes through to w while enforcing limit, so Save can
// refuse to persist a file Load is contractually unable to read before any
// byte reaches the pending temp. (encoding/json's Encoder still buffers the
// complete encoding internally before its single Write, so peak encode memory
// is unchanged from the json.Marshal it replaced; the buffer is pooled and
// released after Encode rather than held across the atomic replacement.) A
// write that would cross the limit is rejected
// whole — before any byte reaches w — with attempted recording the total the
// encoder tried to produce, so the temp never holds an over-cap prefix.
type boundedWriter struct {
	w         io.Writer
	limit     int64
	written   int64
	attempted int64
}

func (bw *boundedWriter) Write(p []byte) (int, error) {
	if bw.written+int64(len(p)) > bw.limit {
		bw.attempted = bw.written + int64(len(p))
		return 0, errStateTooLarge
	}
	n, err := bw.w.Write(p)
	bw.written += int64(n)
	return n, err
}

// Save atomically writes the state file, creating the parent directory if
// needed. It returns an error only when the data did not reach disk; a
// non-durable (unsynced) write is logged, not failed. Save owns the
// sanitize-on-persist invariant: the library snapshot is passed through
// SanitizedForStorage here, at the persistence boundary, so a credentialed
// ArrURL can never land in state.json regardless of which caller saves
// (SafeLogURL is idempotent, so an already-sanitized snapshot is unchanged).
// Save also stamps SchemaVersion into the envelope's version field. Both
// happen on a shallow copy, so the caller's State is never mutated.
// A context already cancelled on entry fails fast — before the sanitize and
// encode work — so scout.save's detached shutdown retry runs immediately
// instead of after a doomed full serialization of the same state.
func (s *Store) Save(ctx context.Context, st *State) error {
	if st == nil {
		return errors.New("state: encode: nil state (Save never writes a non-object state file)")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("state: save %s: %w", s.path, err)
	}
	sanitized := *st
	sanitized.Library = st.Library.SanitizedForStorage()
	sanitized.Version = SchemaVersion
	pf, err := atomicfile.NewPendingFile(ctx, s.path,
		atomicfile.WithMkdirMode(dirMode),
		atomicfile.WithMode(fileMode))
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
	// Enforce the reader's bound on write too: persisting a file Load is
	// contractually unable to consume would silently discard the whole cache
	// next cycle (fail-open). The encoder writes into the pending temp
	// through the bounded writer, which rejects an over-cap encoding before
	// it lands; Cleanup discards the temp on any encode failure, so the
	// last readable state file stays intact until Commit replaces it.
	//
	// The limit admits ONE byte beyond maxStateBytes for the trailing newline
	// json.Encoder.Encode appends (json.Marshal produces none): a state whose
	// json.Marshal encoding is exactly maxStateBytes must stay accepted, and
	// the newline is truncated away below so the persisted file never exceeds
	// what Load can read. The over-cap error subtracts that byte so the
	// reported count is the JSON size, comparable to the limit it names.
	bw := &boundedWriter{w: pf, limit: maxStateBytes + 1}
	if encErr := json.NewEncoder(bw).Encode(&sanitized); encErr != nil {
		if errors.Is(encErr, errStateTooLarge) {
			return fmt.Errorf("state: encode: %d bytes exceeds the %d-byte load limit; keeping previous state file", bw.attempted-1, maxStateBytes)
		}
		return fmt.Errorf("state: encode: %w", encErr)
	}
	// Drop the encoder's guaranteed trailing newline so the persisted size
	// matches the json.Marshal encoding Load's bound is defined against.
	if truncErr := pf.Truncate(bw.written - 1); truncErr != nil {
		return fmt.Errorf("state: write %s: %w", s.path, truncErr)
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

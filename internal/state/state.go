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

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
)

const (
	// maxStateBytes bounds the state file on read AND write (Save refuses to
	// persist what Load would reject). An honest state file (library snapshot
	// + mapping cache + memo) runs ~10-20 MB, so 32 MB keeps
	// real headroom while fitting the 256 MiB deployment container: Load
	// holds the raw JSON and the decoded State simultaneously, so the cap
	// must leave room for both — a larger bound would let a valid at-cap file
	// OOM-kill the container during Load instead of degrading to the intended
	// clean cold start.
	maxStateBytes = 32 << 20
	// stateSizeWarnBytes is the pre-cliff warning threshold (80% of
	// maxStateBytes): crossing the bound refuses every subsequent Save and
	// freezes the persisted cache, so writeState warns while there is still
	// headroom to act. The 80% fraction is internal/degradation's app-wide
	// persisted-file policy, shared with the indexer feed snapshot's
	// feedSizeWarnBytes.
	stateSizeWarnBytes = maxStateBytes / degradation.SizeWarnDenominator * degradation.SizeWarnNumerator
	// dirMode / fileMode are applied to the created state directory and file.
	// The file holds the operator's library snapshot, mapping cache, AniList
	// memo, and degradation streaks, so it stays owner-only (least
	// privilege). dirMode matches every other
	// creator of this same directory - it is config.DefaultConfigDir, also
	// created by main.go's starter-config write, cycle.NewExclusive's cycle
	// lock and the indexer feed snapshot, all at 0o700 - so which writer
	// reaches a fresh volume first cannot change the mode /config lands at.
	dirMode  = 0o700
	fileMode = 0o600
)

// SchemaVersion is the schema version Save stamps into State.Version on every
// write. Bump it when a persisted member moves or is renamed incompatibly, so
// a future loader can detect the old shape and migrate (or refuse) explicitly
// instead of silently zero-loading it. A file whose version field is absent or
// zero is a legacy envelope written before versioning and loads unchanged.
//
// Cross-version coupling with maxStateBytes: the newer-schema preservation
// guarantee (Load refuses the file but keeps it at the live path with Save
// blocked) can only hold for a file that passes the bounded read. An
// over-cap file fails ReadBounded before the version discriminator can be
// inspected and is quarantined as foreign/corrupt (renamed to .corrupt), so
// a future schema bump must not grow the persisted state past the
// maxStateBytes of any binary it may be rolled back to - or must teach the
// over-cap read path to stream-scan the version discriminator before
// choosing quarantine over preservation.
const SchemaVersion = 1

// State is the persisted cross-cycle cache.
//
// It carries NO finding state. Findings used to persist here as a dedupe table
// (Findings/Baselined/BaselineIncomplete) so each one was emitted exactly once,
// ever - which is precisely what made a notification lost anywhere downstream
// permanent. internal/notify now reports findings as STATE, re-emitting the
// current set every pass and holding it in memory, refilled by the compare
// pass that runs at startup. Nothing about a finding survives a restart, and
// nothing needs to: a completed pass reconstructs the whole set.
type State struct {
	// The four streak counters below are persisted DATA, not policy. Their
	// thresholds and the advance/reset rule live in internal/degradation
	// (TickEscalationThreshold, ReconcileEscalationThreshold,
	// ShrunkWalkAcceptThreshold, Advance), and each streak's OWNING site in
	// internal/scout documents when it advances, resets, and what the remedy
	// is. This envelope deliberately does not restate that lifecycle: it cannot
	// enforce it, and a copy of a rule the persistence layer has no say over is
	// exactly the drift h-f23 was raised about. What belongs here is the wire
	// shape and anything about the FIELD a reader of state.json needs.
	Memo    match.Memo    `json:"anilist_memo"`
	Mapping mapping.Cache `json:"mapping"`
	// ShrunkWalksByArr counts, PER ARR, consecutive reconciles the scout's
	// library shrink guard judged that arr's fresh item count a suspicious
	// truncation (below half its OWN prior count, degradation.Shrunk) and
	// carried that side's prior items forward instead of accepting them. Keyed
	// by the library.Arr name ("sonarr"/"radarr"). A side whose walk passes the
	// guard has its entry DELETED (a passing side costs no bytes), and so does a
	// side whose streak reaches degradation.ShrunkWalkAcceptThreshold, where the
	// smaller library is accepted as the new shape - so an entry is bounded by
	// that threshold rather than growing forever.
	//
	// It is per-arr because the aggregate count it replaced could not see one
	// arr emptying while the other kept the total above half: the guard never
	// fired, the compare ran against a library missing that whole side, and
	// every finding for it silently resolved.
	//
	// It is also a NEW key rather than a re-typed `shrunk_walks`, deliberately.
	// Load decodes with json.Unmarshal, which FAILS on a scalar-into-map type
	// mismatch, and a decode error there quarantines the live file and discards
	// the operator's AniList memo (a measured ~25-minute cold reconcile). An
	// unknown key is ignored instead, so an older file's scalar `shrunk_walks`
	// is dropped silently - it is a transient counter, so the cost is at most
	// one extra cycle of tolerance, which is exactly what the app's
	// no-rollback-no-migration decision covers.
	ShrunkWalksByArr map[string]int   `json:"shrunk_walks_by_arr,omitempty"`
	Library          library.Snapshot `json:"library"`
	// SeadexFailures counts consecutive cycles whose SeaDex fetch failed (so the
	// compare was skipped, so findings were not re-reported), whichever
	// pre-compare gate closed the cycle - the scout records the fetch outcome
	// ahead of gate selection, so a coinciding walk/mapping failure cannot hide
	// the outage from the streak. Owner: recordSeaDexFetch.
	SeadexFailures int `json:"seadex_failures,omitempty"`
	// AniListDegraded counts consecutive COMPLETED cycles whose matching left
	// AniList lookups incomplete (match.Result.Degraded), preserving the
	// affected entries' prior findings. Owner: recordAniListDegradation.
	AniListDegraded int `json:"anilist_degraded,omitempty"`
	// PartialWalks counts consecutive COMPLETED cycles whose library walk came
	// back partial (per-series episode-fetch failures left Failed placeholder
	// items the compare excluded). A single permanently failing series holds
	// Snapshot.Partial true forever, which is why notify.Report carries that
	// item's rows forward rather than dropping them (its absence from a pass is
	// missing data, not alignment). Owner: recordPartialWalk.
	PartialWalks int `json:"partial_walks,omitempty"`
	// Version is the persisted envelope's schema version, stamped with
	// SchemaVersion by every Save (on the shallow copy it writes; the
	// caller's State is never mutated). A file with the field absent or zero
	// loads as a legacy pre-version envelope, exactly like any other missing
	// field; a version NEWER than SchemaVersion is refused by Load (an image
	// rollback must not silently zero-load moved members and then overwrite
	// the newer-schema file); and a future member move or rename bumps
	// SchemaVersion so the old shape can be migrated (or refused) explicitly
	// instead of silently zero-loaded.
	Version int `json:"version,omitempty"`
}

// Store loads and saves the state file at a fixed path.
//
// A Store is NOT safe for concurrent use. Load and Save communicate through
// unsynchronized fields (unsupportedVersion, loadFailed) that carry one call's
// classification of the on-disk file into the next, and those fields exist to
// make Save REFUSE: a Save racing a Load can read a stale verdict and
// overwrite bytes the Load meant to preserve (an unclassified read failure's
// possibly-recoverable state, or a newer-schema envelope an image rollback
// must leave intact). Confine each Store to one goroutine - the cycle body,
// which the cross-process cycle lock already serializes - the same contract
// atomicfile.PendingFile states for its own lifecycle state.
type Store struct {
	log  *slog.Logger
	path string
	// unsupportedVersion remembers a newer-than-supported schema version the
	// last Load found at the live path. While non-zero, Save is refused: the
	// newer-schema file must stay in place so rolling forward to the image
	// that wrote it consumes it again, instead of this older binary
	// overwriting it with a fresh cold-start envelope.
	unsupportedVersion int
	// loadFailed remembers that the last Load did not leave the live path in
	// a state Save may replace: either it failed WITHOUT classifying the
	// file (an EACCES/EIO-style read error or a read cut short by context
	// cancellation - not absence, not a newer schema, which sets
	// unsupportedVersion), or it classified an over-cap/corrupt payload but
	// could NOT preserve it (the quarantine rename failed, so the live file
	// is still the only copy). While set, Save is refused - the unread bytes
	// may be fully recoverable (a permissions mistake, a transient I/O
	// fault), and an unpreserved corrupt file is the only forensic evidence,
	// so it must be preserved like every classified failure preserves its
	// evidence, instead of the cold-started cycle overwriting it at its end.
	// The scout loads at the start of
	// every cycle, so the block clears as soon as a Load succeeds, or
	// classifies AND preserves the file.
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
// quarantine and surface on the container's log stream. Save is refused, so
// the read-only contract is enforced by the type rather than relied on from
// callers.
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
// decoded version, whether the key was present, and any wire-level failure.
// It is the single source of truth for the discriminator on BOTH the clean
// and error decode paths: decode must never read it from State.Version. Go
// documents that json.Unmarshal may populate fields before returning a type
// error, and it also deliberately accepts JSON null into an int WITHOUT an
// error - so {"version":null} would otherwise pass as legacy version zero,
// and {"version":99,"version":null} would leave the stale earlier 99 in
// State.Version and be preserved forever as newer-schema state. Save can
// never produce either payload; both violate the documented integer
// discriminator contract. The streaming decode below validates EVERY
// case-insensitive occurrence of the key, explicitly rejecting null (via the
// *int decode) and any negative value (the documented discriminator domain is
// non-negative), so a payload like {"version":"bad","Version":99} or
// {"version":-1,"version":99} - corrupt for this binary AND for a
// roll-forward binary reading the same integer discriminator - errors instead
// of reading as newer-schema 99. Any error (a non-object, a malformed member,
// a null, negative, or non-integer version occurrence, trailing data) sends
// the caller to the quarantine path.
func schemaVersion(data []byte) (version int, found bool, err error) {
	dec := bounded.NewDecoder(bytes.NewReader(data), 0)
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
			// only be corruption or tampering. Validate the domain while each
			// occurrence is still visible: checking only the final
			// accumulated value would let a payload like
			// {"version":-1,"version":99} shed its invalid earlier occurrence
			// and read as preserved newer-schema state (blocking Save every
			// cycle) instead of quarantining as corruption.
			return fmt.Errorf("invalid negative schema version %d", *decoded)
		}
		version, found = *decoded, true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	if endErr := dec.End(); endErr != nil {
		return 0, false, endErr
	}
	return version, found, nil
}

// Load reads and decodes the state file. A missing file returns a zero State
// and no error (cold start); a present but corrupt or oversized file is
// quarantined where possible - see maybeQuarantine for the rename-failed
// fallback, which blocks Save instead - and returns the error so the caller
// can decide (the scout logs it and starts cold). A valid file stamped by a
// NEWER binary (an image rollback) is NOT quarantined: it stays at the live
// path and this Store refuses every subsequent Save, so rolling forward to the
// newer image finds its state intact instead of a freshly-overwritten older
// envelope.
func (s *Store) Load(ctx context.Context) (State, error) {
	// ONE os.Root spans the whole read -> classify -> preserve decision.
	// os.Root only confines what happens after the OpenRoot, so reopening the
	// directory by AMBIENT PATH once the read has already failed would leave a
	// window in which a redirected directory component sends the quarantine
	// rename into a REPLACEMENT directory, moving a file this Load never read.
	// Holding the read's own root open across the classification closes it:
	// every inode probe and the quarantine rename resolve against the exact
	// directory handle the state bytes were read through.
	//
	// A missing state DIRECTORY surfaces here as fs.ErrNotExist and is
	// classified as the cold start it is, exactly like a missing file; any
	// other open failure reaches the unclassified read-fault arm below, where
	// a nil root reports no foreign inode and cannot preserve anything - the
	// same conservative outcome the reopen-on-demand shape had.
	root, err := os.OpenRoot(filepath.Dir(s.path))
	if root != nil {
		defer func() {
			if clErr := root.Close(); clErr != nil {
				s.log.Warn("could not close state directory handle", "dir", filepath.Dir(s.path), "error", clErr)
			}
		}()
	}
	// Sweep stale temps through the SAME root the read -> classify -> preserve
	// decision is pinned to. The ambient CleanupStaleTemps rebuilds each
	// candidate path with filepath.Join and unlinks it with os.Remove, which
	// atomicfile documents as unsafe for a directory a co-mounting writer can
	// modify underneath - the very threat model this root exists for, and the
	// only unpinned filesystem write Load performed. A missing state directory
	// needs no special case any more: OpenRoot already reported it as
	// fs.ErrNotExist and classifyReadFailure treats it as the cold start it is.
	// Read-only stores (the one-shot report) still skip state-directory
	// maintenance entirely: that flow is documented read-only, and removing
	// files here would also risk unlinking a stalled concurrent daemon Save's
	// still-open temp.
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
		"mapping_records", len(st.Mapping.Records),
		"memo_entries", len(st.Memo.Entries),
	}
	if !st.Library.TakenAt.IsZero() {
		// Surface the persisted snapshot's age: the indexer feed's title
		// synthesis reads this snapshot (arr-independent, never a fresh
		// walk), so diagnosing a stale synthesized title needs to see how old
		// the snapshot backing it is. A legacy or walk-less state carries the
		// zero TakenAt and skips the attribute rather than logging a
		// nonsensical multi-century age. A future TakenAt (a backward host
		// clock step, or a syntactically valid state file with a future
		// timestamp) is clamped to zero rather than logging a misleading
		// negative age, matching the mapping cache's clock-skew handling.
		age := max(time.Since(st.Library.TakenAt), 0)
		attrs = append(attrs, "library_age", age.Round(time.Second).String())
	}
	s.log.Info("state loaded", attrs...)
	return st, nil
}

// classifyReadFailure applies Load's read-failure policy to a failed open or
// read and returns the error Load reports: nil for the cold start a missing
// file (or a missing state DIRECTORY, which os.OpenRoot reports the same way)
// is, otherwise the wrapped failure. It owns the state transitions each class
// implies - clearing the flags for a cold start, preserving a deterministically
// unusable payload through maybeQuarantine (which owns loadFailed for that
// class), and arming the Save block for an unclassified fault - so the caller
// keeps only the read/decode flow. It preserves through the root the read was
// resolved against, so the rename cannot land in a directory swapped in after
// the read.
func (s *Store) classifyReadFailure(root *os.Root, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		s.unsupportedVersion = 0
		s.loadFailed = false
		s.log.Info("no state file, starting cold", "path", s.path)
		return nil
	}
	if errors.Is(err, atomicfile.ErrFileTooLarge) || errors.Is(err, atomicfile.ErrNotRegular) {
		// Save enforces maxStateBytes and only ever writes a regular file, so
		// an oversized file - or a directory, FIFO, device or socket at the
		// state path, which ReadBoundedInRoot reports as ErrNotRegular - can
		// only be foreign or corrupt; preserve it like any other corruption.
		// Both are DETERMINISTIC: no retry turns an over-cap payload or a
		// non-regular inode into valid state, so neither belongs in the
		// recoverable read-fault class below (which blocks every later Save
		// until a Load classifies the path, and would freeze persistence
		// forever here).
		s.maybeQuarantine(root)
		return fmt.Errorf("state: read %s: %w", s.path, err)
	}
	if s.foreignInode(root) {
		// A non-regular inode the confined open could not report as
		// ErrNotRegular: os.Root refuses to traverse a symlink pointing out
		// of the state directory and reports it as "path escapes from
		// parent", an unexported error carrying no sentinel to match. It is
		// as DETERMINISTIC as the two cases above (no retry makes it
		// readable, and Save's temp+rename replaces the inode outright), so
		// classify it as corruption instead of arming the recoverable-fault
		// block forever.
		s.maybeQuarantine(root)
		return fmt.Errorf("state: read %s: %w", s.path, err)
	}
	// An UNCLASSIFIED read failure (EACCES, EIO, a cancelled read - not
	// absence, not an over-cap file, not a decode error): the bytes at the
	// live path may be fully recoverable, so they must be preserved like every
	// classified failure preserves its evidence (quarantine / the newer-schema
	// Save block). Block Save until a later Load can classify the file -
	// without this, the cycle that started cold after the failed read would
	// overwrite the unread bytes at its end.
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
// recoverable classification. It probes through the SAME confined root the
// read used, so the probe never follows a symlink out of the state directory
// and never re-resolves the directory by ambient path; a probe that cannot run
// (a nil root, because the directory could not be opened at all, or an lstat
// failure) reports false so the caller keeps its conservative
// recoverable-fault classification.
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
	// A symlink that stays INSIDE the state directory is followed by the
	// confined read exactly like a regular file (ReadBoundedInRoot opens
	// through the root without O_NOFOLLOW and stats the open handle), so it
	// is NOT an inode Save cannot replace: a read that failed over one failed
	// for a transient reason - a cancelled read during a redeploy, EACCES,
	// EIO - and must keep the recoverable classification. Resolving through
	// the root separates the two cases: root.Stat follows an in-root link and
	// refuses one that escapes, so only an escaping link (or a link onto a
	// non-regular inode) reports foreign.
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
	// Save always emits valid UTF-8 JSON. encoding/json otherwise replaces
	// malformed UTF-8 inside strings with U+FFFD, silently altering cache
	// keys and values instead of reporting corruption.
	if !utf8.Valid(data) {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: invalid UTF-8", s.path)
	}
	// Require a JSON object envelope before unmarshalling: json.Unmarshal
	// accepts a literal null into a struct, so a corrupt file holding "null"
	// would otherwise load as a silently-empty state (a fake cold start that
	// discards every cache) instead of surfacing the
	// corruption. Save can never produce anything but an object.
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: not a JSON object", s.path)
	}
	// Bound the structural walk below before it runs. schemaVersion streams
	// the whole envelope through json.Decoder.Token (via bounded.Decoder.Skip),
	// and Token tracks nesting in its own Decoder.tokenStack - one int per
	// open container, with NO depth limit (the scanner's maxNestingDepth of
	// 10000 is only applied on the scanner path, not by Token). A nested
	// payload up to maxStateBytes would therefore allocate ~8 bytes per level
	// (~256 MB at the cap) and OOM-kill the 256 MiB container mid-Load, before
	// the quarantine below can preserve or replace the file - so every restart
	// re-reads it and dies again. json.Valid runs the scanner, which caps
	// nesting at the same 10000 the json.Unmarshal below already enforces, so
	// no payload this rejects could ever have loaded; it only moves the
	// rejection ahead of the unbounded walk.
	if !json.Valid(data) {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: not valid JSON", s.path)
	}
	// The wire discriminator is decoded independently BEFORE the State
	// unmarshal, on every load: State.Version is never trusted (Unmarshal may
	// populate it from an earlier duplicate key, and accepts null into an int
	// silently - see schemaVersion). A wire-level failure - a malformed
	// member, a null or non-integer version occurrence, trailing data - is
	// corruption Save can never have produced; quarantine it.
	wireVersion, found, err := schemaVersion(data)
	if err != nil {
		s.maybeQuarantine(root)
		return State{}, fmt.Errorf("state: decode %s: %w", s.path, err)
	}
	if found && wireVersion > SchemaVersion {
		// A file stamped by a newer binary (an image rollback): its members
		// may have moved, so field-by-field zero-loading is exactly the
		// silent discard SchemaVersion exists to prevent - and a type-level
		// State decode error on such a file is the "moved member" case
		// itself. This is valid state, not corruption: keep it at the live
		// path and block this older Store from overwriting it (Save refuses
		// while unsupportedVersion is set), so rolling forward again consumes
		// it in place.
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
// daemon's own Load detects and reports the corruption. It owns the
// loadFailed transition for every classified-corruption path: preservation
// SUCCEEDING clears the Save block (the corrupt bytes are safe at the
// .corrupt path, so the next Save may replace the live file), while
// preservation FAILING arms it, so Save refuses rather than atomically
// overwriting the still-live corrupt file - the only forensic copy - with a
// cold envelope.
func (s *Store) maybeQuarantine(root *os.Root) {
	// Load positively classified the live file as corrupt, so a newer-schema
	// block remembered from an earlier Load no longer describes the file at
	// the live path (unsupportedVersion is documented as what the LAST Load
	// found there); clear it so the next Save is judged against reality. The
	// generic read-error path keeps the flag: an unreadable file may still
	// be the newer-schema state.
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
// corruption overwrites the previous .corrupt copy (latest wins). A rename
// failure - an existing non-empty .corrupt directory, a cross-device or
// read-only parent - is logged at Warn and returns false, which arms the
// caller's Save block: without a preserved copy, letting the cycle's Save
// replace the live file would erase the corruption evidence entirely.
func (s *Store) quarantine(root *os.Root) bool {
	dir, base := filepath.Dir(s.path), filepath.Base(s.path)
	// Preserve through the very root Load's read was resolved against - never a
	// freshly opened one: the rename acts on the open directory handle that
	// held the bytes just read, so a component redirected AFTER that open
	// cannot move the corrupt bytes to a path the read never validated. A nil
	// root means the directory could not be opened at all, so there is nothing
	// to preserve into and the caller's Save block is armed instead.
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
// non-durable (unsynced) write is logged, not failed. Save owns the
// sanitize-on-persist invariant: the library snapshot is passed through
// SanitizedForStorage here, at the persistence boundary, so a credentialed
// ArrURL can never land in state.json regardless of which caller saves
// (SafeLogURL is idempotent, so an already-sanitized snapshot is unchanged).
// Save also stamps SchemaVersion into the envelope's version field. Both
// happen on a shallow copy, so the caller's State is never mutated.
// A context already cancelled on entry fails fast — before the sanitize and
// encode work — so scout.save's detached shutdown retry runs immediately
// instead of after a doomed full serialization of the same state. A Store
// whose last Load found a newer-than-supported schema version refuses to
// save: the newer-schema file must survive at the live path for a
// roll-forward to consume (see Load). A Store whose last Load failed WITHOUT
// classifying the file (loadFailed: an EACCES/EIO-style read error, or a read
// cut short by context cancellation), or whose last Load classified
// corruption it could not preserve (the quarantine rename failed), refuses
// too, preserving the possibly-recoverable or still-unpreserved bytes until a
// Load classifies and preserves them.
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
// is still the only forensic copy). It is never a
// write fault - nothing was lost by the refusal itself - so a caller that
// would otherwise log a failed save at ERROR should classify it instead. Note
// the third case is not benign like the first two: the on-disk state stays
// corrupt and every later Save is refused until an operator clears the
// quarantine destination. Callers match with errors.Is.
//
// The distinction matters for alerting: a redeploy SIGTERM landing in Load's
// read window sets loadFailed, so the cycle's Save refuses; reporting that
// refusal as a write fault fires the cycle-error alert on a routine redeploy.
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
	sanitized.Library = st.Library.SanitizedForStorage()
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
//
// It enforces the reader's bound on write too: persisting a file Load is
// contractually unable to consume would silently discard the whole cache
// next cycle (fail-open). The bound is atomicfile's WithMaxBytes cap, wired
// in Save: the pending file rejects the encoder's over-cap write whole -
// before any byte lands - and the caller's Cleanup discards the temp on any
// encode failure, so the last readable state file stays intact until Commit
// replaces it. (encoding/json's Encoder still buffers the complete encoding
// internally before its single Write, so peak encode memory is unchanged
// from the json.Marshal it replaced; the buffer is pooled and released
// after Encode rather than held across the atomic replacement.)
//
// The cap admits ONE byte beyond maxStateBytes for the trailing newline
// json.Encoder.Encode appends (json.Marshal produces none): a state whose
// json.Marshal encoding is exactly maxStateBytes must stay accepted, and
// the newline is truncated away below so the persisted file never exceeds
// what Load can read. The over-cap error therefore quotes the staged size
// including that newline, while the wrap names the limit Load enforces.
// The truncation also makes the persisted size match the json.Marshal
// encoding Load's bound is defined against.
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

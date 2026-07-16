// Package state persists seadex-scout's cross-cycle cache as a single JSON file
// written atomically: the last library snapshot (for diffing), the cached Fribb
// map plus its HTTP validators, the AniList fallback memo, the finding dedupe
// records, and a flag marking that the dedupe table has been seeded. A missing
// file loads as an empty state (a cold start), never an error.
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

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
)

const (
	// maxStateBytes bounds the state file on read AND write (Save refuses to
	// persist what Load would reject). The mapping cache (tens of thousands
	// of records) dominates; 128 MB is generous headroom.
	maxStateBytes = 128 << 20
	// dirMode / fileMode are applied to the created state directory and file.
	dirMode  = 0o755
	fileMode = 0o644
)

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
	ShrunkWalks int  `json:"shrunk_walks,omitempty"`
	Baselined   bool `json:"baselined,omitempty"`
}

// Store loads and saves the state file at a fixed path.
type Store struct {
	log  *slog.Logger
	path string
}

// NewStore returns a Store for the given state-file path. logger may be nil.
func NewStore(path string, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{log: logger, path: path}
}

// Load reads and decodes the state file. A missing file returns a zero State
// and no error (cold start); a present but corrupt file returns the decode
// error so the caller can decide (the scout logs it and starts cold).
func (s *Store) Load(ctx context.Context) (State, error) {
	data, err := atomicfile.ReadBounded(ctx, s.path, maxStateBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.log.Info("no state file, starting cold", "path", s.path)
			return State{}, nil
		}
		return State{}, fmt.Errorf("state: read %s: %w", s.path, err)
	}
	// Require a JSON object envelope before unmarshalling: json.Unmarshal
	// accepts a literal null into a struct, so a corrupt file holding "null"
	// would otherwise load as a silently-empty state (a fake cold start that
	// baselines findings and discards every cache) instead of surfacing the
	// corruption. Save can never produce anything but an object.
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		s.quarantine()
		return State{}, fmt.Errorf("state: decode %s: not a JSON object", s.path)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		s.quarantine()
		return State{}, fmt.Errorf("state: decode %s: %w", s.path, err)
	}
	s.log.Debug("state loaded",
		"path", s.path,
		"library_items", len(st.Library.Items),
		"mapping_records", len(st.Mapping.Records),
		"memo_entries", len(st.Memo.Entries),
		"findings", len(st.Findings))
	return st, nil
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

// Save atomically writes the state file, creating the parent directory if
// needed. It returns an error only when the data did not reach disk; a
// non-durable (unsynced) write is logged, not failed.
func (s *Store) Save(ctx context.Context, st *State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("state: encode: %w", err)
	}
	// Enforce the reader's bound on write too: persisting a file Load is
	// contractually unable to consume would silently discard the whole cache
	// next cycle (fail-open). Failing before the atomic replacement starts
	// keeps the last readable state file intact.
	if len(data) > maxStateBytes {
		return fmt.Errorf("state: encode: %d bytes exceeds the %d-byte load limit; keeping previous state file", len(data), maxStateBytes)
	}
	res, err := atomicfile.WriteFile(ctx, s.path, data,
		atomicfile.WithMkdirMode(dirMode),
		atomicfile.WithMode(fileMode))
	if err != nil {
		return fmt.Errorf("state: write %s: %w", s.path, err)
	}
	if !res.Durable {
		s.log.Warn("state written but not durable", "path", s.path)
	}
	return nil
}

// Package state persists seadex-scout's cross-cycle cache as a single JSON file
// written atomically: the last library snapshot (for diffing), the cached Fribb
// map plus its HTTP validators, the AniList fallback memo, and the finding
// dedupe records. A missing file loads as an empty state (a cold start), never
// an error.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/report"
)

const (
	// maxStateBytes bounds the state file on read. The mapping cache (tens of
	// thousands of records) dominates; 128 MB is generous headroom.
	maxStateBytes = 128 << 20
	// dirMode / fileMode are applied to the created state directory and file.
	dirMode  = 0o755
	fileMode = 0o644
)

// State is the persisted cross-cycle cache. Findings is keyed by dedupe key.
type State struct {
	Findings map[string]report.Alerted `json:"findings,omitempty"`
	Memo     match.Memo                `json:"anilist_memo"`
	Mapping  mapping.Cache             `json:"mapping"`
	Library  library.Snapshot          `json:"library"`
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
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
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

// Save atomically writes the state file, creating the parent directory if
// needed. It returns an error only when the data did not reach disk; a
// non-durable (unsynced) write is logged, not failed.
func (s *Store) Save(ctx context.Context, st *State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("state: encode: %w", err)
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

package mapping

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestLoader_Load_logsSkippedOverrideCount pins the operator-visible skipped
// count (non-positive-ID rows discarded during the parse stream): a zero-ID
// and a NEGATIVE-ID override beside one valid entry must log skipped=2 - a
// negative anilist_id is a key the tolerant Fribb decoders can never produce
// and would otherwise be indexed unreachable yet leak into the reverse
// arr-ID catalogue.
func TestLoader_Load_logsSkippedOverrideCount(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":0,"type":"tv"},{"anilist_id":2,"type":"movie"},{"anilist_id":-7,"type":"ova"}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides with missing or invalid anilist_id skipped") != 1 {
		t.Fatalf("Load logs = %v, want one skipped-overrides warning", rec.Messages())
	}
	if !rec.HasAttr("", "skipped", "2") {
		t.Errorf("Load skipped count logs = %v, want skipped=2", rec.Messages())
	}
}

// TestLoader_Load_cleanOverridesEmitNoDiagnostics pins the absence side of
// applyOverrides' diagnostic contract: a clean single-record overrides file
// (no skipped rows, no unknown keys) must emit NEITHER diagnostic warning and
// exactly one applied-overrides info with count=1. Every existing logging test
// asserts presence only, so a regression that emits a zero-count WARN on every
// cycle (log noise that pattern-matched Loki queries would surface as a
// standing alert condition) would go undetected.
func TestLoader_Load_cleanOverridesEmitNoDiagnostics(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":2,"type":"movie","tmdb_movies":[4]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, "http://unused.invalid", WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	for _, msg := range []string{
		"mapping: overrides with missing or invalid anilist_id skipped",
		"mapping: overrides contain unknown keys, ignored",
		unroutableOverrideMessage,
	} {
		if n := logs.CountExact(msg); n != 0 {
			t.Errorf("clean overrides logged %q %d times, want 0", msg, n)
		}
	}
	if logs.CountExact("mapping: applied overrides") != 1 {
		t.Errorf("clean overrides logs = %v, want one applied-overrides info", logs.Messages())
	}
	if !logs.HasAttr("", "count", "1") {
		t.Errorf("applied-overrides logs = %v, want count=1", logs.Messages())
	}
}

// TestLoader_Load_emptyOverridesEmitNoAppliedLog pins the zero-applied
// absence contract: a valid empty overrides array applies nothing, so the
// applied-overrides info line (and every diagnostic warning) must NOT be
// emitted - a count=0 "applied overrides" line every cycle would falsely
// tell the operator an overlay is active.
func TestLoader_Load_emptyOverridesEmitNoAppliedLog(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, "http://unused.invalid", WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if n := logs.CountExact("mapping: applied overrides"); n != 0 {
		t.Errorf("empty overrides logged applied-overrides %d times, want 0 (nothing was applied)", n)
	}
	for _, msg := range []string{
		"mapping: overrides with missing or invalid anilist_id skipped",
		"mapping: overrides contain unknown keys, ignored",
	} {
		if n := logs.CountExact(msg); n != 0 {
			t.Errorf("empty overrides logged %q %d times, want 0", msg, n)
		}
	}
}

// TestLoader_Load_overridesFileRefusalLogsError pins l-f69: a file-level
// overrides refusal is an ERROR, not a WARN. The file is opt-in, so its
// EXISTENCE means the operator intends those pinned mappings to apply, and both
// refusal modes persist until they act - the overlay stays inert on every cycle
// while comparisons silently run on the upstream mapping alone, which a WARN
// never surfaces through the shipped Loki rules. A MISSING file stays silent
// (the ordinary no-overrides case) and no arm may touch
// Cache.RejectedRefreshes: that streak counts UPSTREAM refresh refusals, and
// folding an operator-config failure into it would make one counter mean two
// unrelated things.
func TestLoader_Load_overridesFileRefusalLogsError(t *testing.T) {
	for name, tc := range map[string]struct {
		// setup returns the overrides path to configure.
		setup     func(t *testing.T) string
		wantError string
		wantLogs  int
	}{
		"unreadable": {
			setup: func(t *testing.T) string {
				// A directory at the overrides path fails the bounded read with
				// a non-ErrNotExist error regardless of the test user's
				// privileges (a mode-0 file is still readable as root).
				path := filepath.Join(t.TempDir(), "overrides.json")
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatalf("mkdir overrides dir: %v", err)
				}
				return path
			},
			wantError: "mapping: overrides.json unreadable",
			wantLogs:  1,
		},
		"unreadable through a non-directory config dir": {
			setup: func(t *testing.T) string {
				// The overrides file's PARENT is a regular file, so os.OpenRoot
				// fails before the bounded read runs. Its *fs.PathError carries
				// ENOTDIR and does NOT satisfy errors.Is(err, fs.ErrNotExist),
				// which is the only thing keeping this on the unreadable ERROR
				// arm instead of the silent no-overrides-configured one - and
				// a silent arm here leaves the overlay inert on every cycle
				// with nothing in the log stream to say so.
				dir := filepath.Join(t.TempDir(), "config")
				if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("write non-directory config path: %v", err)
				}
				return filepath.Join(dir, "overrides.json")
			},
			wantError: "mapping: overrides.json unreadable",
			wantLogs:  1,
		},
		"malformed": {
			setup: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "overrides.json")
				if err := os.WriteFile(path, []byte(`{"anilist_id":2}`), 0o600); err != nil {
					t.Fatalf("write overrides: %v", err)
				}
				return path
			},
			wantError: "mapping: overrides.json malformed",
			wantLogs:  1,
		},
		"missing": {
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "overrides.json")
			},
			wantLogs: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := tc.setup(t)
			logger, logs := capture.New()
			l := NewLoader(nil, "http://unused.invalid", WithOverridesPath(path), WithRefresh(time.Hour), WithLogger(logger))
			prev := freshCache()
			next, idx, err := l.Load(t.Context(), prev)
			if err != nil {
				t.Fatalf("Load error: %v (a refused overrides file must never block a cycle)", err)
			}
			if n := logs.CountLevel(slog.LevelError, "overrides.json"); n != tc.wantLogs {
				t.Errorf("overrides ERROR count = %d, want %d; logs = %v", n, tc.wantLogs, logs.Messages())
			}
			if n := logs.CountLevel(slog.LevelWarn, "overrides"); n != 0 {
				t.Errorf("overrides WARN count = %d, want 0 (a file-level refusal is an ERROR); logs = %v", n, logs.Messages())
			}
			if tc.wantError != "" && !logs.Contains(tc.wantError) {
				t.Errorf("logs = %v, want one naming %q and the remedy", logs.Messages(), tc.wantError)
			}
			// The overlay applied nothing: the index still holds only the
			// cached upstream record.
			if _, ok := idx.Lookup(2); ok {
				t.Error("a refused overrides file applied a record, want the overlay ignored")
			}
			if idx.Len() != 1 {
				t.Errorf("index size = %d, want 1 (the cached upstream record only)", idx.Len())
			}
			if next.RejectedRefreshes != prev.RejectedRefreshes {
				t.Errorf("RejectedRefreshes = %d, want %d unchanged (the upstream streak is not the overrides signal)", next.RejectedRefreshes, prev.RejectedRefreshes)
			}
		})
	}
}

// unroutableOverrideMessage is applyOverrides' un-mapped-entry warning.
const unroutableOverrideMessage = "mapping: overrides carry no arr identifier and un-map their entry; " +
	"check for a mistyped tvdb_id/tmdb_movies/imdb_ids key, and restate the ids when overriding only a type or season"

// TestLoader_Load_logsUnroutableOverrideCount pins the count on applyOverrides'
// un-mapped-entry warning. The overlay is wholesale, so an override carrying no
// identifier its routed arr consumes REPLACES a mapped Fribb record with one
// that resolves to nothing - left applied by design, which makes this warning
// the only thing standing between a mistyped id key and an entry that silently
// stops matching. Both shapes count: an entry with no identifiers at all, and a
// MOVIE entry carrying only a TVDB id, which the movie arm never consumes.
func TestLoader_Load_logsUnroutableOverrideCount(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":2,"type":"tv"},` +
		`{"anilist_id":3,"type":"movie","tvdb_id":9},` +
		`{"anilist_id":4,"type":"tv","tvdb_id":7}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, "http://unused.invalid", WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if n := logs.CountExact(unroutableOverrideMessage); n != 1 {
		t.Fatalf("unroutable-overrides warnings = %d, want 1; logs = %v", n, logs.Messages())
	}
	got, ok := logs.AttrValueExact(unroutableOverrideMessage, "count")
	if !ok {
		t.Fatalf("unroutable-overrides warning carries no count attribute; logs = %v", logs.Messages())
	}
	if got != "2" {
		t.Errorf("unroutable-overrides count = %s, want 2 (the typeless entry and the movie carrying only a TVDB id)", got)
	}
}

package mapping

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/animap/mapread"
	"github.com/cplieger/slogx/capture"
)

// TestLoader_Load_logsSkippedOverrideCount pins the operator-visible skipped
// count (non-positive-ID rows discarded during the parse stream): a zero-ID
// and a NEGATIVE-ID override beside one valid entry must log skipped=2 - a
// negative anilist_id is a key the animap decoder never indexes
// and would otherwise be indexed unreachable yet leak into the reverse
// arr-ID catalogue.
func TestLoader_Load_logsSkippedOverrideCount(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":0,"type":"tv"},{"anilist_id":2,"type":"movie"},{"anilist_id":-7,"type":"ova"}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, mapread.Source{Repo: DefaultRepo}, WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
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
// effectiveOverrides' diagnostic contract: a clean single-record overrides file
// (no skipped rows, no unknown keys) must emit NEITHER diagnostic warning and
// exactly one applied-overrides info with count=1. Every existing logging test
// asserts presence only, so a regression that emits a zero-count WARN on every
// cycle (log noise that pattern-matched Loki queries would surface as a
// standing alert condition) would go undetected.
func TestLoader_Load_cleanOverridesEmitNoDiagnostics(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":2,"tmdb_movies":[4]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, mapread.Source{Repo: DefaultRepo}, WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
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
	l := NewLoader(nil, mapread.Source{Repo: DefaultRepo}, WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
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

// TestLoader_Load_overridesFileRefusalLogsError pins that a file-level overrides
// refusal is an ERROR, not a WARN. The file is opt-in, so its EXISTENCE means the
// operator intends those pinned mappings to apply, and both refusal modes persist
// until they act - the overlay stays inert on every cycle while comparisons
// silently run on the upstream mapping alone, which a WARN never surfaces through
// the shipped Loki rules. A MISSING file stays silent, and no arm may touch
// Cache.RejectedRefreshes: that streak counts UPSTREAM refresh refusals, so
// folding an operator-config failure into it makes one counter mean two things.
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
			l := NewLoader(nil, mapread.Source{Repo: DefaultRepo}, WithOverridesPath(path), WithRefresh(time.Hour), WithLogger(logger))
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

// unroutableOverrideMessage is effectiveOverrides' un-mapped-entry warning.
const unroutableOverrideMessage = "mapping: overrides carry no arr identifier and un-map their entry; " +
	"check for a mistyped tvdb_id or tmdb_movies key, and restate the ids when overriding only a season"

// TestLoader_Load_logsUnroutableOverrideCount pins the count on effectiveOverrides'
// un-mapped-entry warning. The overlay is wholesale, so an override carrying no
// route REPLACES a mapped upstream record with one that resolves to nothing -
// left applied by design, which makes this warning the only thing standing
// between a mistyped id key and an entry that silently stops matching. Both
// shapes count: an entry with no identifiers at all, and one naming only the
// AniDB anime, which routes nothing.
func TestLoader_Load_logsUnroutableOverrideCount(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":2},` +
		`{"anilist_id":3,"anidb_id":9},` +
		`{"anilist_id":4,"tvdb_id":7,"season_kind":"absent"}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, mapread.Source{Repo: DefaultRepo}, WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
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
		t.Errorf("unroutable-overrides count = %s, want 2 (the id-less entry and the one naming only its AniDB anime)", got)
	}
}

// loadWithOverrides loads freshCache's record (AniList 1, TVDB 100) under an
// overrides file holding data, returning the index and the captured logs.
func loadWithOverrides(t *testing.T, data string) (*Index, *capture.Recorder) {
	t.Helper()
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(data), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, mapread.Source{Repo: DefaultRepo}, WithOverridesPath(overrides), WithRefresh(time.Hour), WithLogger(logger))
	_, idx, err := l.Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	return idx, logs
}

const (
	imdbRefusedMessage = "mapping: overrides.json names imdb_ids, which seadex-scout no longer reads; " +
		"pinned mappings not applied; name the film's tmdb_movies instead"
	unstatedScopeMessage = "mapping: overrides.json names type with no usable season_kind, and type no longer sets a record's season; " +
		"pinned mappings not applied; add season_kind to the record"
)

// TestLoader_Load_refusesAnOverridesFileNamingIMDb pins the retired key: a
// file naming imdb_ids anywhere applies no record, a sibling without the key
// included, and says once which key to replace.
func TestLoader_Load_refusesAnOverridesFileNamingIMDb(t *testing.T) {
	idx, logs := loadWithOverrides(t, `[{"anilist_id":1,"tvdb_id":555},{"anilist_id":2,"imdb_ids":["tt1"]}]`)
	if n := logs.CountExact(imdbRefusedMessage); n != 1 {
		t.Errorf("imdb_ids refusals = %d, want 1; logs = %v", n, logs.Messages())
	}
	if rec, ok := idx.Lookup(1); !ok || rec.TvdbID != 100 {
		t.Errorf("Lookup(1) = %+v ok=%v, want the cached record: no override applies", rec, ok)
	}
	if _, ok := idx.Lookup(2); ok {
		t.Error("Lookup(2) found the refused file's record")
	}
}

// TestLoader_Load_refusesATypeThatWasTheScope pins the one override whose
// meaning ignoring type would change: type and a tvdb_id with no season_kind
// and no positive season_tvdb used to be offered on season 0 by its type, and
// now has no scope it states, so the whole file is refused naming the record.
func TestLoader_Load_refusesATypeThatWasTheScope(t *testing.T) {
	for name, data := range map[string]string{
		"a film with both links": `[{"anilist_id":1,"tvdb_id":555,"season_kind":"absent"},{"anilist_id":9135,"type":"MOVIE","tvdb_id":85249,"tmdb_movies":[80518]}]`,
		"an OVA":                 `[{"anilist_id":1,"tvdb_id":555,"season_kind":"absent"},{"anilist_id":9135,"type":"OVA","tvdb_id":85249}]`,
		"an unrecognized season": `[{"anilist_id":1,"tvdb_id":555,"season_kind":"absent"},{"anilist_id":9135,"type":"OVA","tvdb_id":85249,"season_kind":"bogus"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			idx, logs := loadWithOverrides(t, data)
			if n := logs.CountExact(unstatedScopeMessage); n != 1 {
				t.Fatalf("type refusals = %d, want 1; logs = %v", n, logs.Messages())
			}
			if !logs.HasAttr(unstatedScopeMessage, "al_id", "9135") {
				t.Errorf("refusal logs = %v, want al_id 9135", logs.Messages())
			}
			if rec, ok := idx.Lookup(1); !ok || rec.TvdbID != 100 {
				t.Errorf("Lookup(1) = %+v ok=%v, want the cached record: the sibling override is not applied", rec, ok)
			}
		})
	}
}

// TestLoader_Load_appliesATypeBesideAStatedScope pins every other override
// naming type: it applies as if the key were absent, counted as unknown.
func TestLoader_Load_appliesATypeBesideAStatedScope(t *testing.T) {
	for name, tc := range map[string]struct {
		data string
		want Record
	}{
		"season 0 stated": {
			data: `[{"anilist_id":1,"type":"MOVIE","tvdb_id":85249,"tmdb_movies":[80518],"season_kind":"present","season_tvdb":0}]`,
			want: Record{AniListID: 1, TvdbID: 85249, TmdbMovies: []int{80518}, SeasonKind: SeasonPresent},
		},
		"a positive season, no kind": {
			data: `[{"anilist_id":1,"type":"OVA","tvdb_id":85249,"season_tvdb":2}]`,
			want: Record{AniListID: 1, TvdbID: 85249, SeasonTvdb: 2},
		},
		"the whole series": {
			data: `[{"anilist_id":1,"type":"TV","tvdb_id":85249,"season_kind":"absent"}]`,
			want: Record{AniListID: 1, TvdbID: 85249, SeasonKind: SeasonAbsent},
		},
		"no tvdb_id": {
			data: `[{"anilist_id":1,"type":"MOVIE","tmdb_movies":[80518]}]`,
			want: Record{AniListID: 1, TmdbMovies: []int{80518}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			idx, logs := loadWithOverrides(t, tc.data)
			rec, ok := idx.Lookup(1)
			if !ok || rec.TvdbID != tc.want.TvdbID || rec.SeasonKind != tc.want.SeasonKind || rec.SeasonTvdb != tc.want.SeasonTvdb ||
				!slices.Equal(rec.TmdbMovies, tc.want.TmdbMovies) {
				t.Errorf("Lookup(1) = %+v ok=%v, want %+v", rec, ok, tc.want)
			}
			if !logs.HasAttr("mapping: overrides contain unknown keys, ignored", "unknown_key_count", "1") {
				t.Errorf("logs = %v, want the unknown-keys warning counting type once", logs.Messages())
			}
		})
	}
	idx, _ := loadWithOverrides(t, `[{"anilist_id":1,"type":"MOVIE","tvdb_id":85249,"tmdb_movies":[80518],"season_kind":"present","season_tvdb":0}]`)
	if rec, _ := idx.Lookup(1); !rec.RadarrRoute() || !rec.SonarrRoute() || !rec.SonarrSpecial() {
		t.Errorf("a MOVIE override in season 0 = %+v, want both routes and a special Sonarr copy", rec)
	}
}

// TestLoader_Load_appliesAnUntypedOverrideWithNoSeason pins the shape the
// type refusal leaves alone: without type nothing about the record changed.
func TestLoader_Load_appliesAnUntypedOverrideWithNoSeason(t *testing.T) {
	idx, logs := loadWithOverrides(t, `[{"anilist_id":9135,"tvdb_id":85249,"tmdb_movies":[80518]}]`)
	if rec, ok := idx.Lookup(9135); !ok || rec.TvdbID != 85249 || rec.SeasonPresence() != seasonUnknown {
		t.Errorf("Lookup(9135) = %+v ok=%v, want the override applied with no stated season", rec, ok)
	}
	if n := logs.CountExact(unstatedScopeMessage); n != 0 {
		t.Errorf("type refusals = %d, want 0", n)
	}
}

package mapping

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
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

// TestLoader_Load_skipsOversizedOverrideIDArrays pins the per-record
// amplification cap: the 4 MiB wire bound caps the file, not what a compact
// record can fan out into retained slices and reverse-catalogue index work,
// so a record whose tmdb_movies (or imdb_ids) array exceeds
// maxOverrideIDsPerRecord is skipped loudly - never applied, never silently
// truncated - while its valid siblings still apply.
func TestLoader_Load_skipsOversizedOverrideIDArrays(t *testing.T) {
	ids := make([]string, maxOverrideIDsPerRecord+1)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":5,"type":"movie","tmdb_movies":[` + strings.Join(ids, ",") + `]},{"anilist_id":2,"type":"movie"}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	_, idx, err := l.Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides with oversized id arrays skipped") != 1 {
		t.Fatalf("Load logs = %v, want one oversized-overrides warning", rec.Messages())
	}
	if _, ok := idx.Lookup(5); ok {
		t.Error("oversized override applied, want skipped")
	}
	if _, ok := idx.Lookup(2); !ok {
		t.Error("valid sibling override not applied")
	}
}

// TestLoader_Load_warnsOnUnknownOverrideKeys pins the unknown-key diagnostic:
// an override written with the upstream Fribb field name (imdb_id) instead of
// the override name (imdb_ids) still applies, but a WARN naming the unknown
// key is logged so the silent-drop trap is visible.
func TestLoader_Load_warnsOnUnknownOverrideKeys(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":2,"type":"movie","imdb_id":"tt0000002"}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides contain unknown keys, ignored") != 1 {
		t.Fatalf("Load logs = %v, want one unknown-keys warning", rec.Messages())
	}
	if !unknownKeysAre(rec, "[imdb_id]") {
		t.Errorf("Load unknown-keys logs = %v, want keys=[imdb_id]", rec.Messages())
	}
}

// unknownKeysAre reports whether any captured record carries a "keys"
// attribute whose rendered value equals want.
func unknownKeysAre(rec *capture.Recorder, want string) bool {
	return rec.HasAttr("", "keys", want)
}

// TestLoader_Load_unknownOverrideKeysLogBounded pins the log-volume bound on
// the unknown-key diagnostic: an overrides file carrying more unique unknown
// keys than the cap logs only the fixed prefix plus the bounded retained
// count with truncation and count_capped markers, so neither the WARN nor
// the parser's retained diagnostic state can balloon on a pathological file.
func TestLoader_Load_unknownOverrideKeysLogBounded(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	total := maxLoggedUnknownKeys + 5
	var b strings.Builder
	b.WriteString(`[{"anilist_id":2,"type":"movie"`)
	for i := range total {
		fmt.Fprintf(&b, `,"unknown_%02d":1`, i)
	}
	b.WriteString(`}]`)
	if err := os.WriteFile(overrides, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides contain unknown keys, ignored") != 1 {
		t.Fatalf("Load logs = %v, want one unknown-keys warning", rec.Messages())
	}
	wantKeys := make([]string, 0, maxLoggedUnknownKeys)
	for i := range maxLoggedUnknownKeys {
		wantKeys = append(wantKeys, fmt.Sprintf("unknown_%02d", i))
	}
	if !unknownKeysAre(rec, fmt.Sprint(wantKeys)) {
		t.Errorf("bounded keys logs = %v, want the first %d keys only", rec.Messages(), maxLoggedUnknownKeys)
	}
	if !rec.HasAttr("", "unknown_key_count", strconv.Itoa(maxRetainedUnknownKeys)) {
		t.Errorf("unknown_key_count logs = %v, want the retained bound %d", rec.Messages(), maxRetainedUnknownKeys)
	}
	if !rec.HasAttr("", "count_capped", "true") {
		t.Errorf("count_capped logs = %v, want true (count is a lower bound)", rec.Messages())
	}
	if !rec.HasAttr("", "keys_truncated", "true") {
		t.Errorf("keys_truncated logs = %v, want true", rec.Messages())
	}
}

// TestLoader_Load_unknownOverrideKeyNameTruncated pins the per-key truncation
// bound on the unknown-key diagnostic: a single unknown key longer than
// maxLoggedKeyBytes is logged truncated with an ellipsis marker, so one
// operator-controlled key name cannot balloon the WARN record. The marker is
// charged INSIDE the budget, so the whole rendered name — marker included — is
// at most maxLoggedKeyBytes bytes (it used to run to maxLoggedKeyBytes plus the
// marker's width).
func TestLoader_Load_unknownOverrideKeyNameTruncated(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	key := strings.Repeat("k", maxLoggedKeyBytes+1)
	data := fmt.Sprintf(`[{"anilist_id":2,"type":"movie",%q:1}]`, key)
	if err := os.WriteFile(overrides, []byte(data), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides contain unknown keys, ignored") != 1 {
		t.Fatalf("Load logs = %v, want one unknown-keys warning", rec.Messages())
	}
	shownKey := key[:maxLoggedKeyBytes-len(keyTruncMarker)]
	if got := len(shownKey + keyTruncMarker); got != maxLoggedKeyBytes {
		t.Fatalf("rendered key name = %d bytes, want the hard bound %d", got, maxLoggedKeyBytes)
	}
	wantKeys := "[" + shownKey + keyTruncMarker + "]"
	if !unknownKeysAre(rec, wantKeys) {
		t.Errorf("unknown keys logs = %v, want keys=%s", rec.Messages(), wantKeys)
	}
	if !rec.HasAttr("", "unknown_key_count", "1") {
		t.Errorf("unknown_key_count logs = %v, want 1", rec.Messages())
	}
	if !rec.HasAttr("", "keys_truncated", "true") {
		t.Errorf("keys_truncated logs = %v, want true", rec.Messages())
	}
}

// TestLoader_Load_warnsOnDuplicateOverrideIDs pins the duplicate-override
// diagnostic end to end: the same non-zero anilist_id supplied three times
// logs one WARN naming the distinct duplicated ID once with duplicate_count=1
// (distinct conflicting mappings, not repeated rows), while the documented
// last-record-wins overlay still applies.
func TestLoader_Load_warnsOnDuplicateOverrideIDs(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":2,"type":"tv","tvdb_id":100},{"anilist_id":2,"type":"tv","tvdb_id":200},{"anilist_id":2,"type":"tv","tvdb_id":300}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	loader := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	_, idx, err := loader.Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	got, ok := idx.Lookup(2)
	if !ok || got.TvdbID != 300 {
		t.Errorf("Lookup(2) = %+v, %v, want last duplicate with TvdbID 300", got, ok)
	}
	if logs.CountExact("mapping: duplicate override anilist_ids, last record wins") != 1 {
		t.Fatalf("Load logs = %v, want one duplicate-overrides warning", logs.Messages())
	}
	if !logs.HasAttr("", "ids", "[2]") {
		t.Errorf("duplicate-overrides logs = %v, want ids=[2]", logs.Messages())
	}
	if !logs.HasAttr("", "duplicate_count", "1") {
		t.Errorf("duplicate-overrides logs = %v, want duplicate_count=1", logs.Messages())
	}
}

// TestLoader_Load_duplicateOverrideIDsLogBounded pins the log-volume bound on
// the duplicate-override diagnostic (the sibling of the unknown-keys bound):
// more distinct duplicated AniList IDs than maxLoggedDuplicateIDs logs only
// the fixed id prefix while duplicate_count still carries the full distinct
// count, so a pathological overrides file cannot balloon the WARN into a
// record downstream log limits would truncate or reject.
func TestLoader_Load_duplicateOverrideIDsLogBounded(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	total := maxLoggedDuplicateIDs + 5
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= total; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"type":"tv","tvdb_id":1},{"anilist_id":%d,"type":"tv","tvdb_id":2}`, i, i)
	}
	b.WriteByte(']')
	if err := os.WriteFile(overrides, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if logs.CountExact("mapping: duplicate override anilist_ids, last record wins") != 1 {
		t.Fatalf("Load logs = %v, want one duplicate-overrides warning", logs.Messages())
	}
	wantIDs := make([]int, 0, maxLoggedDuplicateIDs)
	for i := 1; i <= maxLoggedDuplicateIDs; i++ {
		wantIDs = append(wantIDs, i)
	}
	if !logs.HasAttr("", "ids", fmt.Sprint(wantIDs)) {
		t.Errorf("duplicate-overrides logs = %v, want the first %d ids only", logs.Messages(), maxLoggedDuplicateIDs)
	}
	if !logs.HasAttr("", "duplicate_count", strconv.Itoa(total)) {
		t.Errorf("duplicate_count logs = %v, want %d", logs.Messages(), total)
	}
}

// TestLoader_Load_unknownOverrideKeyBoundsAtLimit pins the accepting side of
// both unknown-key log bounds at their exact limits: exactly
// maxLoggedUnknownKeys keys of exactly maxLoggedKeyBytes bytes are logged
// whole - no elided tail, no ellipsis, keys_truncated=false - so the bounds
// fire only past the documented limits (a boundary off-by-one would truncate
// a legal diagnostic).
func TestLoader_Load_unknownOverrideKeyBoundsAtLimit(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	pad := strings.Repeat("x", maxLoggedKeyBytes-3)
	var b strings.Builder
	b.WriteString(`[{"anilist_id":2,"type":"movie"`)
	for i := range maxLoggedUnknownKeys {
		fmt.Fprintf(&b, `,"k%02d%s":1`, i, pad)
	}
	b.WriteString(`}]`)
	if err := os.WriteFile(overrides, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides contain unknown keys, ignored") != 1 {
		t.Fatalf("Load logs = %v, want one unknown-keys warning", rec.Messages())
	}
	wantKeys := make([]string, 0, maxLoggedUnknownKeys)
	for i := range maxLoggedUnknownKeys {
		wantKeys = append(wantKeys, fmt.Sprintf("k%02d%s", i, pad))
	}
	if !unknownKeysAre(rec, fmt.Sprint(wantKeys)) {
		t.Errorf("at-limit keys logs = %v, want all %d keys whole (no ellipsis)", rec.Messages(), maxLoggedUnknownKeys)
	}
	if !rec.HasAttr("", "unknown_key_count", strconv.Itoa(maxLoggedUnknownKeys)) {
		t.Errorf("unknown_key_count logs = %v, want %d", rec.Messages(), maxLoggedUnknownKeys)
	}
	if !rec.HasAttr("", "count_capped", "false") {
		t.Errorf("count_capped logs = %v, want false (retention bound not reached)", rec.Messages())
	}
	if !rec.HasAttr("", "keys_truncated", "false") {
		t.Errorf("keys_truncated logs = %v, want false (both bounds exactly at their limits)", rec.Messages())
	}
}

// TestLoader_Load_cleanOverridesEmitNoDiagnostics pins the absence side of
// applyOverrides' diagnostic contract: a clean single-record overrides file
// (no duplicates, no skipped rows, no oversized arrays, no unknown keys)
// must emit NONE of the four diagnostic warnings and exactly one
// applied-overrides info with count=1. Every existing logging test asserts
// presence only, so a regression that emits a zero-count WARN on every cycle
// (log noise that pattern-matched Loki queries would surface as a standing
// alert condition) would go undetected.
func TestLoader_Load_cleanOverridesEmitNoDiagnostics(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":2,"type":"movie","tmdb_movies":[4]}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	for _, msg := range []string{
		"mapping: duplicate override anilist_ids, last record wins",
		"mapping: overrides with missing or invalid anilist_id skipped",
		"mapping: overrides with oversized id arrays skipped",
		"mapping: overrides contain unknown keys, ignored",
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
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if n := logs.CountExact("mapping: applied overrides"); n != 0 {
		t.Errorf("empty overrides logged applied-overrides %d times, want 0 (nothing was applied)", n)
	}
	for _, msg := range []string{
		"mapping: duplicate override anilist_ids, last record wins",
		"mapping: overrides with missing or invalid anilist_id skipped",
		"mapping: overrides with oversized id arrays skipped",
		"mapping: overrides contain unknown keys, ignored",
	} {
		if n := logs.CountExact(msg); n != 0 {
			t.Errorf("empty overrides logged %q %d times, want 0", msg, n)
		}
	}
}

// TestLoader_Load_sanitizesUnknownOverrideKeyControlChars pins the security
// half of the unknown-key diagnostic: logUnknownKeys runs every key through
// runesafe.SanitizeSingleLine before the byte cap, so an operator-controlled
// key carrying a terminal-control rune (ESC here) is logged with the unsafe
// rune replaced by a space and can never smuggle terminal-control or
// direction-override text into the log stream.
func TestLoader_Load_sanitizesUnknownOverrideKeyControlChars(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":2,"type":"movie","e\u001bvil":1}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides contain unknown keys, ignored") != 1 {
		t.Fatalf("Load logs = %v, want one unknown-keys warning", rec.Messages())
	}
	if !unknownKeysAre(rec, "[e vil]") {
		t.Errorf("unknown keys logs = %v, want the ESC control replaced with a space: keys=[e vil]", rec.Messages())
	}
}

// TestLoader_Load_oversizedOverrideIDsLogBounded pins the log-volume bound on
// the oversized-override diagnostic, the sibling of the duplicate-ID bound:
// more oversized records than maxLoggedDuplicateIDs logs only the fixed id
// prefix while skipped still carries the exact total, so a pathological
// overrides file cannot balloon the WARN into a record downstream log limits
// would truncate or reject - and the operator keeps both the sample ids and
// the true count needed to find the offending rows.
func TestLoader_Load_oversizedOverrideIDsLogBounded(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	total := maxLoggedDuplicateIDs + 5
	var b strings.Builder
	b.WriteByte('[')
	for id := 1; id <= total; id++ {
		if id > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"anilist_id":%d,"type":"movie","tmdb_movies":%s}`, id, overCapIDs())
	}
	b.WriteByte(']')
	if err := os.WriteFile(overrides, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, logs := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(t.Context(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if logs.CountExact("mapping: overrides with oversized id arrays skipped") != 1 {
		t.Fatalf("Load logs = %v, want one oversized-overrides warning", logs.Messages())
	}
	wantIDs := make([]int, 0, maxLoggedDuplicateIDs)
	for id := 1; id <= maxLoggedDuplicateIDs; id++ {
		wantIDs = append(wantIDs, id)
	}
	if !logs.HasAttr("", "ids", fmt.Sprint(wantIDs)) {
		t.Errorf("oversized-overrides logs = %v, want the first %d ids only", logs.Messages(), maxLoggedDuplicateIDs)
	}
	if !logs.HasAttr("", "skipped", strconv.Itoa(total)) {
		t.Errorf("oversized skipped count logs = %v, want the exact total %d", logs.Messages(), total)
	}
	if !logs.HasAttr("", "max_ids", strconv.Itoa(maxOverrideIDsPerRecord)) {
		t.Errorf("oversized max_ids logs = %v, want %d", logs.Messages(), maxOverrideIDsPerRecord)
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
			l := NewLoader(nil, "http://unused.invalid", path, time.Hour, logger)
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

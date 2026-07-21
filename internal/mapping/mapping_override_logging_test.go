package mapping

import (
	"context"
	"fmt"
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
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
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
	_, idx, err := l.Load(context.Background(), freshCache())
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
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
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
// keys than the cap logs only the fixed prefix plus the full count and a
// truncation marker, so the WARN cannot balloon into a multi-megabyte record
// downstream log limits would truncate or reject.
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
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
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
	if !rec.HasAttr("", "unknown_key_count", strconv.Itoa(total)) {
		t.Errorf("unknown_key_count logs = %v, want %d", rec.Messages(), total)
	}
	if !rec.HasAttr("", "keys_truncated", "true") {
		t.Errorf("keys_truncated logs = %v, want true", rec.Messages())
	}
}

// TestLoader_Load_unknownOverrideKeyNameTruncated pins the per-key truncation
// bound on the unknown-key diagnostic: a single unknown key longer than
// maxLoggedKeyBytes is logged truncated with an ellipsis marker, so one
// operator-controlled key name cannot balloon the WARN record.
func TestLoader_Load_unknownOverrideKeyNameTruncated(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	key := strings.Repeat("k", maxLoggedKeyBytes+1)
	data := fmt.Sprintf(`[{"anilist_id":2,"type":"movie",%q:1}]`, key)
	if err := os.WriteFile(overrides, []byte(data), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides contain unknown keys, ignored") != 1 {
		t.Fatalf("Load logs = %v, want one unknown-keys warning", rec.Messages())
	}
	wantKeys := "[" + key[:maxLoggedKeyBytes] + "...]"
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
	_, idx, err := loader.Load(context.Background(), freshCache())
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
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
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
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
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
	if !rec.HasAttr("", "keys_truncated", "false") {
		t.Errorf("keys_truncated logs = %v, want false (both bounds exactly at their limits)", rec.Messages())
	}
}

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
// count (zero-ID rows discarded during the parse stream): two zero-ID
// overrides beside one valid entry must log skipped=2, not just leave the
// index unpolluted.
func TestLoader_Load_logsSkippedOverrideCount(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":0,"type":"tv"},{"anilist_id":2,"type":"movie"},{"anilist_id":0,"type":"ova"}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	logger, rec := capture.New()
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if rec.CountExact("mapping: overrides missing anilist_id skipped") != 1 {
		t.Fatalf("Load logs = %v, want one skipped-overrides warning", rec.Messages())
	}
	if !attrRendered(rec, "skipped", "2") {
		t.Errorf("Load skipped count logs = %v, want skipped=2", rec.Messages())
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
	return attrRendered(rec, "keys", want)
}

// attrRendered reports whether any captured record carries an attribute with
// the given key whose rendered value equals want.
func attrRendered(rec *capture.Recorder, key, want string) bool {
	return attrValueMatches(rec, key, func(v string) bool { return v == want })
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
	if !attrRendered(rec, "unknown_key_count", strconv.Itoa(total)) {
		t.Errorf("unknown_key_count logs = %v, want %d", rec.Messages(), total)
	}
	if !attrRendered(rec, "keys_truncated", "true") {
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
	if !attrRendered(rec, "unknown_key_count", "1") {
		t.Errorf("unknown_key_count logs = %v, want 1", rec.Messages())
	}
	if !attrRendered(rec, "keys_truncated", "true") {
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
	if !attrRendered(logs, "ids", "[2]") {
		t.Errorf("duplicate-overrides logs = %v, want ids=[2]", logs.Messages())
	}
	if !attrRendered(logs, "duplicate_count", "1") {
		t.Errorf("duplicate-overrides logs = %v, want duplicate_count=1", logs.Messages())
	}
}

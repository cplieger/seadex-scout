package mapping

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoader_Load_logsSkippedOverrideCount pins the operator-visible skipped
// count arithmetic (len(overrides) - applied): two zero-ID overrides beside
// one valid entry must log skipped=2, not just leave the index unpolluted.
func TestLoader_Load_logsSkippedOverrideCount(t *testing.T) {
	overrides := filepath.Join(t.TempDir(), "overrides.json")
	data := []byte(`[{"anilist_id":0,"type":"tv"},{"anilist_id":2,"type":"movie"},{"anilist_id":0,"type":"ova"}]`)
	if err := os.WriteFile(overrides, data, 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	l := NewLoader(nil, "http://unused.invalid", overrides, time.Hour, logger)
	if _, _, err := l.Load(context.Background(), freshCache()); err != nil {
		t.Fatalf("Load error: %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, `"msg":"mapping: overrides missing anilist_id skipped"`) {
		t.Fatalf("Load logs = %s, want skipped-overrides warning", got)
	}
	if !strings.Contains(got, `"skipped":2`) {
		t.Errorf("Load skipped count log = %s, want skipped=2", got)
	}
}

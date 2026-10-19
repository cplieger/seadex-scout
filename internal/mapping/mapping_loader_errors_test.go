package mapping

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/animap/mapread"
)

// TestNewLoader_nilLoggerDefaults pins the nil-logger constructor branch: a
// nil logger falls back to slog.Default() so the loader's log calls cannot
// panic on the fresh-reuse path (which logs at Debug).
func TestNewLoader_nilLoggerDefaults(t *testing.T) {
	l := NewLoader(nil, mapread.Source{Repo: DefaultRepo}, WithRefresh(time.Hour))
	next, err := l.refreshCache(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("refreshCache with nil logger error: %v", err)
	}
	if len(next.Records) != 1 {
		t.Errorf("nil-logger fresh reuse kept %d records, want 1", len(next.Records))
	}
}

// TestLoader_refreshCache_boundsUpstreamErrorText pins the emit-boundary
// sanitization of a refusal's cause: a hostile redirect target or body,
// echoed by mapread into its error, surfaces bounded and single-line instead
// of putting megabytes of server-chosen text into the log stream every
// degraded cycle.
func TestLoader_refreshCache_boundsUpstreamErrorText(t *testing.T) {
	hostile := strings.Repeat("A\u0007\u202e", 5000)
	for name, serve := range map[string]func(*release){
		"a latest-release target": func(r *release) {
			r.latest = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", r.srv.URL+"/x/"+strings.Repeat("%07%E2%80%AE", 2000))
				w.WriteHeader(http.StatusFound)
			}
		},
		"a document": func(r *release) { r.publish(testTag, []byte(`{"version":2,"`+hostile+`":1}`)) },
	} {
		t.Run(name, func(t *testing.T) {
			rel := newRelease(t, testTag, nil)
			rel.edit(serve)
			_, err := rel.loader().refreshCache(t.Context(), &Cache{})
			if err == nil {
				t.Fatal("refreshCache = nil error, want a refusal")
			}
			msg := err.Error()
			if len(msg) > 2*maxLoggedErrorBytes+100 {
				t.Errorf("degraded error text = %d bytes, want bounded near maxLoggedErrorBytes (%d)", len(msg), maxLoggedErrorBytes)
			}
			if strings.ContainsAny(msg, "\n\r\u0007") || strings.Contains(msg, "\u202e") {
				t.Errorf("degraded error text carries a control/bidi rune: %q", msg)
			}
		})
	}
}

// TestLoader_Load_canceledContextSkipsOverrides pins effectiveOverrides'
// context-cancellation branch: a canceled context skips the overrides read
// silently (no overlay, no warn) while the fresh cache still serves.
func TestLoader_Load_canceledContextSkipsOverrides(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":1,"type":"movie"}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := unused(WithOverridesPath(overrides), WithRefresh(time.Hour))
	_, idx, err := l.Load(ctx, freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	rec, ok := idx.Lookup(1)
	if !ok {
		t.Fatal("canceled-context Load lost the cached record for id 1")
	}
	if rec.Type != "TV" {
		t.Errorf("canceled-context Load applied the override: Type = %q, want TV", rec.Type)
	}
}

// TestLoader_Load_directoryOverridesIgnored pins the unreadable-overrides warn
// branch through a root-safe injection (a directory at the overrides path, not
// a permission bit): the read error is logged and ignored, and the upstream
// record survives unmodified.
func TestLoader_Load_directoryOverridesIgnored(t *testing.T) {
	l := unused(WithOverridesPath(t.TempDir()), WithRefresh(time.Hour))
	_, idx, err := l.Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load with directory overrides error: %v", err)
	}
	if rec, ok := idx.Lookup(1); !ok || rec.Type != "TV" {
		t.Errorf("directory overrides changed the record: %+v ok=%v", rec, ok)
	}
}

// TestLoader_Load_zeroIDOverrideIgnored pins effectiveOverrides' keying guard: an
// override record with anilist_id 0 is not indexed (it cannot key a SeaDex
// lookup), so no phantom entry appears.
func TestLoader_Load_zeroIDOverrideIgnored(t *testing.T) {
	dir := t.TempDir()
	overrides := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(overrides, []byte(`[{"anilist_id":0,"type":"movie"}]`), 0o644); err != nil {
		t.Fatalf("write overrides: %v", err)
	}
	l := unused(WithOverridesPath(overrides), WithRefresh(time.Hour))
	_, idx, err := l.Load(t.Context(), freshCache())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if idx.Len() != 1 {
		t.Errorf("zero-id override was indexed: Len = %d, want 1", idx.Len())
	}
	if _, ok := idx.Lookup(0); ok {
		t.Error("Lookup(0) found a phantom zero-id override entry")
	}
}

package match

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/slogx/capture"
)

// TestFindByTitleBoundsAmbiguousTitleLog is the cross-package acceptance test
// for the matcher's adoption of the bounded joiner: logattr's own suite proves
// Joiner is bounded, but nothing pinned that findByTitle actually routes its
// UNTRUSTED ambiguous-title attribute through it. Replacing j.String() with an
// eagerly joined or raw title would leave the rest of the suite green while
// re-opening the CWE-400 log-amplification path (one hostile multi-megabyte
// AniList title emitted verbatim into a Loki-shipped attribute).
//
// The assertion deliberately accepts either a string or a []string attribute so
// it does not pre-decide the deferred log-schema question; only the VOLUME bound
// and the truncation marker are pinned.
func TestFindByTitleBoundsAmbiguousTitleLog(t *testing.T) {
	title := strings.Repeat("A", logattr.MaxBytes+1)
	li := NewLibIndex(&library.Snapshot{Items: []library.Item{
		{Arr: library.ArrSonarr, ArrID: 1, Title: title},
		{Arr: library.ArrSonarr, ArrID: 2, Title: title},
	}})
	logger, recorder := capture.New()

	if got := li.findByTitle([]string{title}, 0, library.ArrSonarr, logger); got != nil {
		t.Fatalf("findByTitle() = %+v, want nil for an ambiguous title", got)
	}
	records := recorder.Records()
	if len(records) != 1 {
		t.Fatalf("log records = %d, want 1", len(records))
	}
	var encoded string
	records[0].Attrs(func(a slog.Attr) bool {
		if a.Key != "titles" {
			return true
		}
		switch value := a.Value.Any().(type) {
		case string:
			encoded = value
		case []string:
			encoded = strings.Join(value, ", ")
		default:
			t.Fatalf("titles attribute type = %T, want string or []string", value)
		}
		return false
	})
	if len(encoded) > logattr.MaxBytes+len(logattr.TruncMarker) {
		t.Errorf("titles attribute = %d bytes, want at most %d", len(encoded), logattr.MaxBytes+len(logattr.TruncMarker))
	}
	if !strings.HasSuffix(encoded, logattr.TruncMarker) {
		t.Errorf("titles attribute has no %q suffix despite truncation", logattr.TruncMarker)
	}
}

package logattr

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/keyenc"
)

// TestMaxBytesMirrorsKeyencBudget pins the equality logattr's doc comment
// asserts: the slog-attribute budget is kept equal to the bound the dedupe-key
// path applies to the same SeaDex data. audit/render.go used to alias
// keyenc.MaxComponentBytes directly (a compile-time tie); after the extraction
// the relationship is only a comment, so this test is what keeps a change to
// keyenc from silently diverging the two budgets. keyenc imports stdlib only,
// so the test-only import keeps logattr a dependency-free production leaf.
func TestMaxBytesMirrorsKeyencBudget(t *testing.T) {
	if MaxBytes != keyenc.MaxComponentBytes {
		t.Errorf("MaxBytes = %d, want keyenc.MaxComponentBytes (%d)", MaxBytes, keyenc.MaxComponentBytes)
	}
}

// TestCapPassesHonestValuesUnchanged pins the byte-identical guarantee an
// honest attribute relies on: only the rune policy applies, no marker, no cut.
func TestCapPassesHonestValuesUnchanged(t *testing.T) {
	const honest = "SubsPlease · Frieren S01 (1080p)"
	if got := Cap(honest); got != honest {
		t.Errorf("Cap(%q) = %q, want it unchanged", honest, got)
	}
	if got := Cap("group\u0007name\u202e"); got != "group name " {
		t.Errorf("Cap unsafe runes = %q, want them replaced with spaces", got)
	}
}

// TestCapBoundsOversizedValues pins the volume bound plus the truncation
// signal, including the post-sanitize re-cap: every invalid UTF-8 byte grows
// into the three-byte U+FFFD, so a pre-sanitize cap alone would emit ~3x the
// budget and re-mint partial runes.
func TestCapBoundsOversizedValues(t *testing.T) {
	for name, raw := range map[string]string{
		"ascii":   strings.Repeat("A", 4*MaxBytes),
		"invalid": strings.Repeat("\xff", 4*MaxBytes),
	} {
		t.Run(name, func(t *testing.T) {
			got := Cap(raw)
			if len(got) > MaxBytes+len(TruncMarker) {
				t.Errorf("Cap(%d bytes) = %d bytes, want <= %d", len(raw), len(got), MaxBytes+len(TruncMarker))
			}
			if !strings.HasSuffix(got, TruncMarker) {
				t.Errorf("Cap(%d bytes) carries no %q marker", len(raw), TruncMarker)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Cap(%d bytes) is not valid UTF-8", len(raw))
			}
		})
	}
}

// TestJoinerBoundsAggregateAndMarksTruncation pins the multi-source contract:
// separators are charged against the same budget, a piece that no longer fits
// stops the joiner, and the aggregate carries one truncation marker.
func TestJoinerBoundsAggregateAndMarksTruncation(t *testing.T) {
	j := NewJoiner()
	piece := strings.Repeat("B", MaxBytes/2)
	if !j.Write(piece) {
		t.Fatal("first piece did not fit the budget")
	}
	for range 8 {
		if !j.WriteSep(",") || !j.Write(piece) {
			break
		}
	}
	got := j.String()
	if len(got) > MaxBytes+len(TruncMarker) {
		t.Errorf("joined attribute = %d bytes, want <= %d", len(got), MaxBytes+len(TruncMarker))
	}
	if !strings.HasSuffix(got, TruncMarker) {
		t.Errorf("joined attribute carries no %q marker despite dropped pieces", TruncMarker)
	}

	// An exhausted joiner keeps refusing, so a caller's loop terminates.
	if j.Write("more") {
		t.Error("exhausted joiner accepted another piece")
	}
}

// TestJoinerHonestAggregateMatchesJoinThenCap pins the equivalence the
// streaming design rests on: for values within the budget, writing each piece
// and the raw ASCII separators yields the same bytes as joining first.
func TestJoinerHonestAggregateMatchesJoinThenCap(t *testing.T) {
	parts := []string{"pmr", "SubsPlease", "LostYears"}
	j := NewJoiner()
	for i, p := range parts {
		if i > 0 {
			j.WriteSep(",")
		}
		j.Write(p)
	}
	if got, want := j.String(), Cap(strings.Join(parts, ",")); got != want {
		t.Errorf("streamed aggregate = %q, want the joined-then-capped form %q", got, want)
	}
}

// TestCapOrderDivergesOnlyForShrinkingHostileValues pins both sides of the ONE
// observable difference between this package's cap-before-sanitize order and a
// sanitize-then-cap composition (d-u14c2-2, which flagged the shift as
// untested rather than wrong).
//
// Sanitizing can SHRINK - strings.Map replaces each unsafe rune with a
// single-byte space, so a 3-byte bidi control collapses to one byte - so a raw
// value over the budget can sanitize to well under it. Under sanitize-then-cap
// that value emits whole and UNMARKED; here it emits cut and MARKED, because the
// cap runs first. Both properties are deliberate: the pre-sanitize cap is what
// makes the budget bound the WORK (one hostile multi-MB SeaDex value must never
// walk the sanitizer in a 256 MiB container, CWE-400), and the marker is honest
// because bytes really were dropped. An HONEST value must be byte-identical
// under either order - that is the half a regression would break silently, since
// every emitted attribute (title, groups, tracker, classification_reason, the
// URLs, info_hash) and emitResolved all ride this primitive.
func TestCapOrderDivergesOnlyForShrinkingHostileValues(t *testing.T) {
	sanitizeThenCap := func(raw string) string {
		clean := runesafe.Sanitize(raw)
		if len(clean) <= MaxBytes {
			return clean
		}
		return runesafe.CapBytes(clean, MaxBytes) + TruncMarker
	}

	t.Run("honest values are identical under either order", func(t *testing.T) {
		for _, raw := range []string{
			"",
			"[SubsPlease] Show - S01E01 (1080p) [ABCD1234].mkv",
			strings.Repeat("a", MaxBytes-1),
			strings.Repeat("a", MaxBytes),
			strings.Repeat("a", MaxBytes+1),
			strings.Repeat("日", MaxBytes), // multi-byte but SAFE: no shrink
		} {
			if got, want := Cap(raw), sanitizeThenCap(raw); got != want {
				t.Errorf("Cap(len %d) = %d bytes, want the sanitize-then-cap form's %d bytes (honest values must not diverge)",
					len(raw), len(got), len(want))
			}
		}
	})

	t.Run("an oversized shrinking value is cut and marked", func(t *testing.T) {
		// Bidi controls: 3 raw bytes each, one byte sanitized. Over the raw
		// budget, well under it once sanitized.
		raw := strings.Repeat("\u202e", MaxBytes/3+100)
		if len(raw) <= MaxBytes {
			t.Fatalf("fixture is %d bytes, want it over the %d-byte budget", len(raw), MaxBytes)
		}
		if n := len(runesafe.Sanitize(raw)); n > MaxBytes {
			t.Fatalf("sanitized fixture is %d bytes, want it UNDER the budget so the two orders diverge", n)
		}
		if other := sanitizeThenCap(raw); strings.HasSuffix(other, TruncMarker) {
			t.Fatal("sanitize-then-cap marked the fixture; the divergence this test pins would not exist")
		}
		got := Cap(raw)
		if !strings.HasSuffix(got, TruncMarker) {
			t.Errorf("Cap(oversized shrinking value) = %d bytes unmarked, want the truncation marker (raw bytes were dropped)", len(got))
		}
		if len(got) > MaxBytes+len(TruncMarker) {
			t.Errorf("Cap() = %d bytes, want at most %d", len(got), MaxBytes+len(TruncMarker))
		}
		if !utf8.ValidString(got) {
			t.Error("Cap() returned invalid UTF-8")
		}
	})
}

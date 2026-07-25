package logattr

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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

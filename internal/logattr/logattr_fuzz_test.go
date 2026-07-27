package logattr

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/runesafe"
)

// FuzzCapBoundsAndPreservesHonestValues fuzzes the slog-attribute volume
// primitive every untrusted SeaDex string is rendered through, with the three
// invariants the callers rely on: the output never exceeds the budget plus the
// marker (CWE-400 - the bound is what keeps one hostile multi-MB value from
// ballooning a Loki record or the 256 MiB container), it is always valid UTF-8
// (the pre-sanitize cap and the post-sanitize re-cap both cut on rune
// boundaries, so no arbitrary byte offset may re-mint a partial rune), and an
// UNMARKED result is exactly the sanitized input (nothing is silently dropped
// without the truncation signal). The seed corpus carries the boundary shapes
// the table tests cannot enumerate: a cut landing mid-rune, an invalid byte at
// the budget edge, and a value whose sanitized form shrinks below the budget.
func FuzzCapBoundsAndPreservesHonestValues(f *testing.F) {
	f.Add("")
	f.Add("[SubsPlease] Show - S01E01 (1080p)")
	f.Add("group\u0007name\u202e")
	f.Add(strings.Repeat("A", MaxBytes+1))
	f.Add(strings.Repeat("\xff", MaxBytes+1))
	f.Add(strings.Repeat("日", MaxBytes/3+1))            // multi-byte: the cut lands mid-rune
	f.Add(strings.Repeat("a", MaxBytes-1) + "日")        // rune straddles the budget edge
	f.Add(strings.Repeat("a", MaxBytes-1) + "\xff\xff") // invalid bytes at the budget edge
	f.Add(strings.Repeat("\u202e", MaxBytes/3+100))     // sanitizing SHRINKS below the budget
	f.Fuzz(func(t *testing.T, raw string) {
		got := Cap(raw)
		if len(got) > MaxBytes+len(TruncMarker) {
			t.Fatalf("Cap(%d bytes) = %d bytes, want at most %d", len(raw), len(got), MaxBytes+len(TruncMarker))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Cap(%d bytes) is not valid UTF-8", len(raw))
		}
		if !strings.HasSuffix(got, TruncMarker) {
			if want := runesafe.Sanitize(raw); got != want {
				t.Fatalf("Cap(%d bytes) dropped bytes without the %q marker: got %d bytes, want the sanitized %d",
					len(raw), TruncMarker, len(got), len(want))
			}
		}
	})
}

// FuzzJoinerBoundsAggregate fuzzes the multi-source path with the invariant a
// single value cannot check: however many untrusted pieces a hostile SeaDex
// entry contributes (up to 512 torrents per entry), the JOINED attribute stays
// inside the same budget, stays valid UTF-8, and reports truncation whenever a
// piece was refused - so a caller's write loop always terminates.
func FuzzJoinerBoundsAggregate(f *testing.F) {
	f.Add("pmr", "SubsPlease", "LostYears")
	f.Add("", "", "")
	f.Add(strings.Repeat("A", MaxBytes), "b", "c")
	f.Add("a", strings.Repeat("\xff", MaxBytes), "c")
	f.Add(strings.Repeat("日", MaxBytes/3), strings.Repeat("日", MaxBytes/3), "\u202e")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		j := NewJoiner()
		accepted := 0
		for _, piece := range []string{a, b, c} {
			if accepted > 0 {
				j.WriteSep(",")
			}
			if !j.Write(piece) {
				// An exhausted joiner must keep refusing, or a streaming caller
				// cannot know when to stop.
				if j.Write(piece) {
					t.Fatal("joiner accepted a piece after refusing one")
				}
				break
			}
			accepted++
		}
		got := j.String()
		if len(got) > MaxBytes+len(TruncMarker) {
			t.Fatalf("joined attribute = %d bytes, want at most %d", len(got), MaxBytes+len(TruncMarker))
		}
		if !utf8.ValidString(got) {
			t.Fatal("joined attribute is not valid UTF-8")
		}
	})
}

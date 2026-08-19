package logattr

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/runesafe/v2"
)

// FuzzCapBoundsAndPreservesHonestValues fuzzes the slog-attribute volume
// primitive every untrusted SeaDex string is rendered through, with the three
// invariants the callers rely on: the output never exceeds the budget plus the
// marker (CWE-400 - the bound is what keeps one hostile multi-MB value from
// ballooning a Loki record or the 256 MiB container), it is always valid UTF-8
// (the pre-sanitize cap and the post-sanitize re-cap both cut on rune
// boundaries, so no arbitrary byte offset may re-mint a partial rune), and an
// IN-BUDGET input is rendered byte-identically to its sanitized form while an
// over-budget one is marked (nothing is silently dropped, and the verdict is
// read off the input so an honest "..." tail cannot excuse a drop). The seed
// corpus carries the boundary shapes the table tests cannot enumerate: a cut
// landing mid-rune, an invalid byte at the budget edge, and a value whose
// sanitized form shrinks below the budget.
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
	f.Add("honest title...")                            // honest value whose own tail is the marker
	f.Add(strings.Repeat("c", MaxBytes+50) + "...")     // truncated AND marker-tailed content
	f.Fuzz(func(t *testing.T, raw string) {
		got := Cap(raw)
		if len(got) > MaxBytes+len(TruncMarker) {
			t.Fatalf("Cap(%d bytes) = %d bytes, want at most %d", len(raw), len(got), MaxBytes+len(TruncMarker))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Cap(%d bytes) is not valid UTF-8", len(raw))
		}
		// The marker is content, not metadata: an honest value whose own tail is
		// "..." is indistinguishable from a cut one in the OUTPUT, so whether
		// truncation is expected is derived from the INPUT instead. Cap truncates
		// on exactly two conditions (both from Joiner.Write under the full
		// budget): the raw value exceeds the budget, or its sanitized form does
		// (sanitizing grows each invalid byte to a three-byte U+FFFD). Anything
		// else must pass through byte-identical to the sanitized input, which is
		// the check a silent drop has to fail.
		want := runesafe.Sanitize(raw)
		truncated := len(raw) > MaxBytes || len(want) > MaxBytes
		if !truncated {
			if got != want {
				t.Errorf("Cap(%d bytes) changed an in-budget value: got %q, want %q", len(raw), got, want)
			}
		} else {
			if !strings.HasSuffix(got, TruncMarker) {
				t.Fatalf("Cap(%d bytes) truncated without the %q marker: got %q", len(raw), TruncMarker, got)
			}
			// Cap only ever cuts a suffix (Sanitize is a per-rune map and both
			// caps land on rune boundaries), so the result minus one marker is
			// always a prefix of the sanitized input - an interior byte dropped
			// silently fails this too.
			if !strings.HasPrefix(want, strings.TrimSuffix(got, TruncMarker)) {
				t.Fatalf("Cap(%d bytes) = %q, want a prefix of the sanitized input (%d bytes)",
					len(raw), got, len(want))
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
		// Whether the joiner refused a write that carried bytes: an empty piece
		// drops nothing, so only a non-empty piece or a separator obliges the
		// truncation signal.
		refusedBytes := false
		for _, piece := range []string{a, b, c} {
			if accepted > 0 && !j.WriteSep(",") {
				refusedBytes = true
			}
			if !j.Write(piece) {
				if piece != "" {
					refusedBytes = true
				}
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
		if refusedBytes && !strings.HasSuffix(got, TruncMarker) {
			t.Fatalf("joiner refused a non-empty write without the %q marker: got %q", TruncMarker, got)
		}
	})
}

package anilist

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestDedupeTitles_idempotentAndLossless pins the title-cleaning contract the
// fuzz targets also assert (no blank, no duplicate titles) plus two properties
// the tables cannot reach across arbitrary inputs: no usable (non-blank) input
// title is ever lost, and the function is idempotent (re-deduping its own
// output is a no-op), so a downstream normalized-title match sees a stable,
// complete list.
func TestDedupeTitles_idempotentAndLossless(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		titles := rapid.SliceOfN(
			rapid.OneOf(rapid.SampledFrom([]string{"", " ", "A", "B", "Frieren"}), rapid.String()),
			0, 12,
		).Draw(t, "titles")

		out := dedupeTitles(titles...)

		seen := make(map[string]struct{}, len(out))
		for _, title := range out {
			if strings.TrimSpace(title) == "" {
				t.Fatalf("dedupeTitles(%q) returned a blank title", titles)
			}
			if _, dup := seen[title]; dup {
				t.Fatalf("dedupeTitles(%q) returned duplicate title %q", titles, title)
			}
			seen[title] = struct{}{}
		}
		for _, title := range titles {
			if strings.TrimSpace(title) == "" {
				continue
			}
			if _, ok := seen[title]; !ok {
				t.Fatalf("dedupeTitles(%q) lost usable input title %q", titles, title)
			}
		}
		if again := dedupeTitles(out...); !slices.Equal(again, out) {
			t.Fatalf("dedupeTitles not idempotent: dedupeTitles(%q) = %q, want %q", out, again, out)
		}
	})
}

// TestParseMediaPage_roundTripsGeneratedBatchesProperty is the every-PR
// round-trip complement to the fixed batch tables and the weekly fuzz run:
// arbitrary well-formed Page(media) envelopes (built by encoding/json, not by
// hand) must decode into exactly the generated id set with format, year, and
// titles preserved. Generators stay on the unambiguous side of the contract
// (unique ids, positive seasonYear, one non-empty title) so the expectation is
// the generated value verbatim, never a reimplementation of the decoder's
// fallback logic; the year-fallback and dedupe edges stay pinned by the tables.
func TestParseMediaPage_roundTripsGeneratedBatchesProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		type wireTitle struct {
			Romaji string `json:"romaji"`
		}
		type wireMedia struct {
			Title      wireTitle `json:"title"`
			Format     string    `json:"format"`
			ID         int       `json:"id"`
			SeasonYear int       `json:"seasonYear"`
		}
		type wirePage struct {
			Media []wireMedia `json:"media"`
		}
		type wireData struct {
			Page wirePage `json:"Page"`
		}
		type wireEnvelope struct {
			Data wireData `json:"data"`
		}

		// Capped at batchSize: the decoder's boundedMediaList rejects a longer
		// array by contract (the query requests perPage=batchSize), and that
		// edge is pinned by TestParseMediaPageBoundsMediaCardinality.
		ids := rapid.SliceOfNDistinct(rapid.IntRange(1, 1_000_000), 0, batchSize, rapid.ID).Draw(t, "ids")
		media := make([]wireMedia, len(ids))
		want := make(map[int]Media, len(ids))
		for i, id := range ids {
			m := wireMedia{
				ID:         id,
				Format:     rapid.SampledFrom([]string{"TV", "MOVIE", "OVA", "SPECIAL"}).Draw(t, fmt.Sprintf("format%d", i)),
				SeasonYear: rapid.IntRange(1950, 2100).Draw(t, fmt.Sprintf("year%d", i)),
				Title:      wireTitle{Romaji: rapid.StringMatching(`[A-Za-z][A-Za-z0-9 ]{0,20}`).Draw(t, fmt.Sprintf("title%d", i))},
			}
			media[i] = m
			want[id] = Media{Format: m.Format, Year: m.SeasonYear, Titles: []string{m.Title.Romaji}}
		}

		raw, err := json.Marshal(wireEnvelope{Data: wireData{Page: wirePage{Media: media}}})
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		got, err := parseMediaPage(raw)
		if err != nil {
			t.Fatalf("parseMediaPage(%s): %v", raw, err)
		}
		if len(got) != len(want) {
			t.Fatalf("parseMediaPage returned %d ids, want %d", len(got), len(want))
		}
		for id, wm := range want {
			gm, ok := got[id]
			if !ok {
				t.Fatalf("id %d missing from parsed batch", id)
			}
			if gm.Format != wm.Format || gm.Year != wm.Year || !slices.Equal(gm.Titles, wm.Titles) {
				t.Fatalf("parsed media for id %d = %+v, want %+v", id, gm, wm)
			}
		}
	})
}

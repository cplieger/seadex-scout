package indexer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestDerivedTitle_preservesSingleEpisodesAndCollapsesPacksProperty is the
// every-PR randomized complement to the feed-title tables: for any generated
// title/season/episode pair, a single-episode torrent keeps its SxxExx title
// while a torrent spanning two distinct episodes is a real pack and collapses
// to the season.
func TestDerivedTitle_preservesSingleEpisodesAndCollapsesPacksProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		title := rapid.StringMatching(`[A-Za-z0-9]{1,24}`).
			Filter(func(s string) bool {
				// The title must not itself look like an episode marker or a
				// creditless-extra tag: derivedTitle/coveredEpisodes key on the
				// LAST matching token in a file name, and real fansub names put
				// the marker after the title, so a token-shaped title is outside
				// the domain this heuristic (documented best-effort) supports.
				return !episodeToken.MatchString(s) && !payload.IsCreditlessExtra(s)
			}).
			Draw(t, "title")
		season := rapid.IntRange(1, 99).Draw(t, "season")
		first := rapid.IntRange(1, 8_000).Draw(t, "first_episode")
		second := first + rapid.IntRange(1, 1_000).Draw(t, "episode_gap")
		firstName := fmt.Sprintf("%s - S%02dE%02d [Grp].mkv", title, season, first)
		secondName := fmt.Sprintf("%s - S%02dE%02d [Grp].mkv", title, season, second)

		single := &seadex.Torrent{Files: []seadex.File{{Name: firstName}}}
		if got, want := derivedTitle(single, EntryInfo{}), firstName[:len(firstName)-len(".mkv")]; got != want {
			t.Fatalf("derivedTitle(single episode) = %q, want %q", got, want)
		}

		pack := &seadex.Torrent{Files: []seadex.File{{Name: firstName}, {Name: secondName}}}
		if got := coveredEpisodes(pack.Files); got != 2 {
			t.Fatalf("coveredEpisodes(pack) = %d, want 2", got)
		}
		if got, want := derivedTitle(pack, EntryInfo{}), fmt.Sprintf("%s - S%02d [Grp]", title, season); got != want {
			t.Fatalf("derivedTitle(pack) = %q, want %q", got, want)
		}
	})
}

// TestLastSubmatchIndex_isFindAllsLastMatchProperty pins the equivalence the
// memory-bounded replay in lastSubmatchIndex claims: for the two patterns the
// title synthesis actually scans with, it must return exactly what
// FindAllStringSubmatchIndex's LAST element would be (and nil when there is no
// match). Every season/episode decision in this file - the pack collapse, the
// single-episode marker, the cour-local season relabel, the per-file season
// tally - reads its span offsets from that return, so an off-by-one in the
// offset rebase or a lost last-match progression silently serves a mangled or
// wrong-episode title instead of failing. A name assembled from repeated
// marker-shaped pieces is what makes the property discriminating: with a
// single match the offset rebase is a no-op.
func TestLastSubmatchIndex_isFindAllsLastMatchProperty(t *testing.T) {
	piece := rapid.SampledFrom([]string{
		"Show", " - ", "_", ".", "-", " ", "1080p", "v2", "NCED",
		"S01E01", "S1E7", "S02E05-E07", "S01E15v2", " - 07", "_-_02_", " - 1085 ",
	})
	rapid.Check(t, func(t *rapid.T) {
		re := episodeToken
		if rapid.Bool().Draw(t, "absolute_form") {
			re = absoluteEpisode
		}
		name := strings.Join(rapid.SliceOfN(piece, 0, 8).Draw(t, "pieces"), "")
		all := re.FindAllStringSubmatchIndex(name, -1)
		var want []int
		if len(all) > 0 {
			want = all[len(all)-1]
		}
		got := lastSubmatchIndex(re, name)
		if len(got) != len(want) {
			t.Fatalf("lastSubmatchIndex(%v, %q) = %v, want %v (the last of %d matches)", re, name, got, want, len(all))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("lastSubmatchIndex(%v, %q) = %v, want %v (index %d differs)", re, name, got, want, i)
			}
		}
	})
}

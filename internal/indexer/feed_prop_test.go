package indexer

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/seadex"
	"pgregory.net/rapid"
)

// TestFeedTitle_preservesSingleEpisodesAndCollapsesPacksProperty is the
// every-PR randomized complement to the feed-title tables: for any generated
// title/season/episode pair, a single-episode torrent keeps its SxxExx title
// while a torrent spanning two distinct episodes is a real pack and collapses
// to the season.
func TestFeedTitle_preservesSingleEpisodesAndCollapsesPacksProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		title := rapid.StringMatching(`[A-Za-z0-9]{1,24}`).
			Filter(func(s string) bool {
				// The title must not itself look like an episode marker or a
				// creditless-extra tag: feedTitle/coveredEpisodes key on the
				// LAST matching token in a file name, and real fansub names put
				// the marker after the title, so a token-shaped title is outside
				// the domain this heuristic (documented best-effort) supports.
				return !episodeToken.MatchString(s) && !creditlessExtra.MatchString(s)
			}).
			Draw(t, "title")
		season := rapid.IntRange(1, 99).Draw(t, "season")
		first := rapid.IntRange(1, 8_000).Draw(t, "first_episode")
		second := first + rapid.IntRange(1, 1_000).Draw(t, "episode_gap")
		firstName := fmt.Sprintf("%s - S%02dE%02d [Grp].mkv", title, season, first)
		secondName := fmt.Sprintf("%s - S%02dE%02d [Grp].mkv", title, season, second)

		single := &seadex.Torrent{Files: []seadex.File{{Name: firstName}}}
		if got, want := feedTitle(single), firstName[:len(firstName)-len(".mkv")]; got != want {
			t.Fatalf("feedTitle(single episode) = %q, want %q", got, want)
		}

		pack := &seadex.Torrent{Files: []seadex.File{{Name: firstName}, {Name: secondName}}}
		if got := coveredEpisodes(pack.Files); got != 2 {
			t.Fatalf("coveredEpisodes(pack) = %d, want 2", got)
		}
		if got, want := feedTitle(pack), fmt.Sprintf("%s - S%02d [Grp]", title, season); got != want {
			t.Fatalf("feedTitle(pack) = %q, want %q", got, want)
		}
	})
}

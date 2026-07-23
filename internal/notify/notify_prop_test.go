package notify

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/filter"
	"pgregory.net/rapid"
)

// TestTrackerURLsRoutingProperty pins trackerURLs' routing invariants under
// randomized link sets, with filter's own classifiers as the oracle (never a
// reimplementation of the switch): every returned slot is one of the input
// URLs or empty; the public/nyaa slot never carries an AB-gated
// (unclassifiable or AnimeBytes) link, so an ambiguous URL can never render
// as the clickable public link; and the FIRST definite AnimeBytes link
// always wins the AB slot, ahead of any fail-closed fallback.
func TestTrackerURLsRoutingProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		trackerGen := rapid.SampledFrom([]string{"Nyaa", "nyaa", "AB", "animebytes", "AnimeTosho", "RuTracker", "Unknown", ""})
		shapeGen := rapid.SampledFrom([]string{
			"https://nyaa.si/view/%d",
			"https://animetosho.org/v/%d",
			"https://animebytes.tv/torrents.php?id=%d",
			"https://animebytes.tv exploit %d",
			"https:/animebytes.tv/torrents.php?id=%d",
			"https://animebytes\uff0etv/t/%d",
		})
		n := rapid.IntRange(0, 6).Draw(rt, "n")
		links := make([]compare.ReleaseLink, n)
		for i := range links {
			links[i] = compare.ReleaseLink{
				Tracker: trackerGen.Draw(rt, "tracker"),
				URL:     fmt.Sprintf(shapeGen.Draw(rt, "shape"), i),
			}
		}

		nyaa, ab := trackerURLs(links)

		find := func(url string) *compare.ReleaseLink {
			for i := range links {
				if links[i].URL == url {
					return &links[i]
				}
			}
			return nil
		}
		if nyaa != "" {
			if l := find(nyaa); l == nil {
				rt.Fatalf("nyaa = %q is not an input URL", nyaa)
			} else if filter.ABGated(l.Tracker, l.URL) {
				rt.Fatalf("nyaa slot carries an AB-gated link %+v", *l)
			}
		}
		if ab != "" && find(ab) == nil {
			rt.Fatalf("ab = %q is not an input URL", ab)
		}
		for i := range links {
			if filter.DefinitelyAB(links[i].Tracker, links[i].URL) {
				if ab != links[i].URL {
					rt.Fatalf("ab = %q, want the first definite AnimeBytes link %q", ab, links[i].URL)
				}
				break
			}
		}
	})
}

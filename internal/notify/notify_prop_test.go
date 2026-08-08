package notify

import (
	"fmt"
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
	"pgregory.net/rapid"
)

// TestTrackerURLsRoutingProperty pins trackerURLs' routing invariants under
// randomized link sets, with tracker's own classifiers as the oracle (never a
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
			"https://tracker.example/v/%d",
			"/view/%d",
		})
		n := rapid.IntRange(0, 6).Draw(rt, "n")
		links := make([]compare.ReleaseLink, n)
		for i := range links {
			links[i] = compare.ReleaseLink{
				Tracker: trackerGen.Draw(rt, "tracker"),
				URL:     fmt.Sprintf(shapeGen.Draw(rt, "shape"), i),
			}
		}

		pub, abLink := trackerURLs(gradedLinks(links...))
		ab := abLink.url

		find := func(url string) *compare.ReleaseLink {
			for i := range links {
				if links[i].URL == url {
					return &links[i]
				}
			}
			return nil
		}
		if pub.url != "" {
			if l := find(pub.url); l == nil {
				rt.Fatalf("public = %q is not an input URL", pub.url)
			} else if l.AB != tracker.ABNone {
				rt.Fatalf("public slot carries a link with AnimeBytes evidence %+v", *l)
			}
			// The public slot must name the tracker it came from, so the alert
			// can label the link truthfully instead of calling everything Nyaa.
			if l := find(pub.url); l != nil && pub.tracker != l.Tracker {
				rt.Fatalf("public tracker = %q, want the link's own tracker %q", pub.tracker, l.Tracker)
			}
			// Exactly one of the two emitted URL attrs is ever populated.
			if pub.nyaaURL() != "" && pub.otherURL() != "" {
				rt.Fatalf("both nyaa_url (%q) and public_url (%q) populated", pub.nyaaURL(), pub.otherURL())
			}
			// A non-Nyaa public link whose URL carries a host must always
			// carry a name to render: a host naming no known tracker labels
			// the link with itself (canonicalTracker's last resort), which is
			// what keeps the nameless-public-link defect closed. A hostless
			// value (a bare tracker-relative path) has nothing to name it and
			// is unreachable in production - every Finding.Links URL comes
			// from classify.PublishURL, which publishes only absolute URLs on
			// a canonical tracker host - so the invariant is scoped to
			// host-bearing links rather than claiming more than holds.
			if host := urlform.Classify(pub.otherURL()).Host; host != "" && pub.otherTracker() == "" {
				rt.Fatalf("public_url %q carries no public_tracker to label it", pub.otherURL())
			}
		}
		if ab != "" && find(ab) == nil {
			rt.Fatalf("ab = %q is not an input URL", ab)
		}
		// The AB slot must always carry a name to render beside its URL: the
		// alert labels ab_url with ab_tracker, and an unnameable link falls
		// back to AnimeBytes (the slot's own meaning) rather than to "".
		if ab != "" && abLink.abTracker() == "" {
			rt.Fatalf("ab_url %q carries no ab_tracker to label it", ab)
		}
		for i := range links {
			if links[i].AB == tracker.ABDefinite {
				if ab != links[i].URL {
					rt.Fatalf("ab = %q, want the first definite AnimeBytes link %q", ab, links[i].URL)
				}
				break
			}
		}
	})
}

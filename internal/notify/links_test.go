package notify

import (
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/tracker"
)

// gradedLinks returns links the way compare's producer emits them: each
// carrying the AnimeBytes grade for its (tracker, URL) pair.
//
// Production grades the RAW upstream record (classify.ABEvidence), never the
// published link. These fixtures ARE raw values, so grading them here
// reproduces the producer exactly, and the routing assertions keep testing what
// this package owns: slot precedence.
func gradedLinks(links ...compare.ReleaseLink) []compare.ReleaseLink {
	for i := range links {
		links[i].AB = tracker.ClassifyAB(links[i].Tracker, links[i].URL)
	}
	return links
}

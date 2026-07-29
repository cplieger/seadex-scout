package notify

import (
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/filter"
)

// gradedLinks returns links the way compare's producer emits them: each
// carrying the AnimeBytes grade for its (tracker, URL) pair.
//
// In production that grade is read from the RAW upstream record
// (classify.ABEvidence), never from the published link - one grading site for
// the app, with the raw-URL invariant kept at its documented owner (h-f43).
// These fixtures ARE raw values, so grading them here reproduces the producer
// exactly, and the routing assertions keep testing what this package still
// owns: slot precedence. A link whose grade must diverge from its published
// URL is asserted where the divergence lives, in compare's producer test.
func gradedLinks(links ...compare.ReleaseLink) []compare.ReleaseLink {
	for i := range links {
		links[i].AB = filter.ClassifyAB(links[i].Tracker, links[i].URL)
	}
	return links
}

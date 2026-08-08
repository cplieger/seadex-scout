package indexer

import (
	"context"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// Advance folds a bounded WINDOW of recently-changed SeaDex entries into the
// persisted feed. It is the SAME pass as Rebuild at a different SCOPE (see
// run), which is the whole point of the rewrite this file used to be the
// counter-example to: Advance was a separate code path, and that is why it got
// five persisted members wrong by omission - it re-implemented a subset of
// Rebuild rather than running Rebuild's rules over a smaller input.
//
// What the window scope withholds is exactly three verdicts, each of which
// would be acting on ABSENCE from an input that is not the whole catalogue:
//
//   - it may not DELETE a curation owner (an entry absent from the window is
//     not absent from SeaDex, and an empty window is legitimate). It MAY upsert
//     the owners it evaluated, which is what makes a release curated this tick
//     searchable within one tick instead of within one reconcile interval;
//   - it may not take the BASELINE path over a missing or malformed snapshot,
//     which would record "everything currently curated" in the never-pruned
//     publication log - from a window that would forfeit ~8700 identities the
//     app has not actually served, permanently. run defers to the next
//     reconcile instead;
//   - it may not conclude that a carried item it did not evaluate has been
//     de-curated, and may not refuse one on its stored GUID: only the reconcile
//     holds the evidence to re-render and self-heal that GUID, and under the
//     never-pruned log the window's verdict would be the irreversible one (see
//     carryUnevaluatedItem).
//
// Three computations also stay reconcile-only because their criterion is
// catalogue-wide: the warned-identity closure, the Prowlarr title harvest, and
// retainTitles. And one asymmetry remains and is bounded rather than closed - a
// PURE removal (an entry loses a torrent and gains nothing) may not move the
// entry's `updated` field at all, so it never enters a window; that de-curation
// stays the reconcile's to notice, within one reconcile interval.
//
// What it does NOT withhold, and used to: a carried item is re-rendered when the
// window evaluated it, the window's own curation is admitted to the search
// index, the warned retraction runs over the window's own positive evidence, and
// every counter the pass computes reaches the log line.
func (w *FeedWriter) Advance(ctx context.Context, window []seadex.Entry, info EntryInfoFunc) error {
	return w.run(ctx, window, info, scopeWindow)
}

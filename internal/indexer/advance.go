package indexer

import (
	"context"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// Advance folds a bounded WINDOW of recently-changed SeaDex entries into the
// persisted feed. It is the SAME pass as Rebuild at a different SCOPE (see run).
func (w *FeedWriter) Advance(ctx context.Context, window []seadex.Entry, info EntryInfoFunc) error {
	return w.run(ctx, window, info, scopeWindow)
}

// Package classify houses the shared SeaDex-to-release classification glue: the
// single construction of a release.Release from a seadex.Torrent (in the
// context of its entry) that both the compare (findings) and audit (report)
// flows depend on. Keeping it in one place means the two flows classify an
// identical SeaDex release identically and cannot silently diverge if the
// release.Input contract gains a field. It is a seadex-aware adapter so the
// release package can stay a pure, seadex-free leaf.
package classify

import (
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
)

// --- AB visibility gates (adapters over filter) ---

// PublishURL returns the clickable tracker link for a SeaDex torrent, or "" when
// the publisher refused the raw upstream value (see trackerlink.Publish).
func PublishURL(t *seadex.Torrent) string {
	return trackerlink.Publish(t.Tracker, t.URL)
}

// PublishRefusal is PublishURL plus the publisher's refusal reason, for the
// consumers that DIAGNOSE a drop rather than just render a link: the audit
// report's row marker and the SeaDex client's aggregate catalogue WARN both
// have to name a remedy, and an unknown tracker's remedy (an internal/tracker
// table entry, shipped in a release) is not the SeaDex record's.
func PublishRefusal(t *seadex.Torrent) (string, trackerlink.Refusal) {
	return trackerlink.PublishReason(t.Tracker, t.URL)
}

// Obtainable reports whether a classified SeaDex release is obtainability
// evidence under the operator's AnimeBytes toggle. It owns the argument
// invariant shared by compare and audit (mirroring ABEvidence's adapter
// pattern): the RAW upstream URL (t.URL) feeds the tracker cross-check while
// the published link (PublishURL) is the grabbable one, in that order.
func Obtainable(rel *release.Release, t *seadex.Torrent, animeBytes bool) bool {
	return filter.Obtainable(rel, t.URL, PublishURL(t), animeBytes)
}

// ABEvidence grades the AnimeBytes evidence in a SeaDex torrent. Like
// filter.ABVisible it reads the RAW upstream URL (t.URL), never the published
// link, because publishing trusts the tracker label and would rewrite or erase
// the very host evidence the grading needs; the adapter owns that invariant for
// compare and audit alike.
func ABEvidence(t *seadex.Torrent) tracker.ABEvidence {
	return tracker.ClassifyAB(t.Tracker, t.URL)
}

// --- Torrent classification ---

// Torrent classifies one SeaDex torrent, in the context of its entry (for the
// shared notes), into a normalized release.Release. This is the one place the
// release.Input for a SeaDex torrent is built, so compare and audit classify
// the same release identically.
func Torrent(entry *seadex.Entry, t *seadex.Torrent) release.Release {
	return release.Classify(&release.Input{
		Names:     payload.Names(t.Files),
		Notes:     entry.Notes,
		Group:     t.ReleaseGroup,
		Tracker:   t.Tracker,
		DualAudio: t.DualAudio,
	})
}

// FileResolution classifies a torrent's resolution from its file names
// alone, over the shared payload.Names eligibility rule. The entry notes are
// deliberately excluded: they are entry-wide and routinely describe sibling
// releases, so they must not stamp a per-torrent title (the indexer's RSS
// title synthesis is the consumer).
func FileResolution(files []seadex.File) string {
	return release.Classify(&release.Input{Names: payload.Names(files)}).Resolution
}

// --- Shared entry-state verdict rules ---

// EntryFallback classifies an entry that lists no recommended releases.
// Theoretical beats incomplete - the one precedence compare's emptyResult
// and audit's rowQualifier must share.
type EntryFallback int

const (
	// FallbackNone means the entry warrants no fallback classification.
	FallbackNone EntryFallback = iota
	// FallbackTheoretical means the entry names only a theoretical best.
	FallbackTheoretical
	// FallbackIncomplete means the entry is incomplete with nothing recommended.
	FallbackIncomplete
)

// Fallback derives the shared fallback classification for an entry whose
// recommended-release set is empty: a theoretical-best-only entry outranks an
// incomplete one. Both compare (StatusTheoretical/StatusIncomplete) and audit
// (QualifierTheoretical/QualifierIncomplete) map their vocabulary from this
// one precedence, so the two flows cannot silently drift.
func Fallback(entry *seadex.Entry) EntryFallback {
	switch {
	case entry.HasTheoreticalBest():
		return FallbackTheoretical
	case entry.Incomplete:
		return FallbackIncomplete
	}
	return FallbackNone
}

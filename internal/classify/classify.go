// Package classify houses the shared SeaDex-to-release classification glue: the
// single construction of a release.Release from a seadex.Torrent (in the
// context of its entry) that both the compare (findings) and audit (report)
// flows depend on. Keeping it in one place means the two flows classify an
// identical SeaDex release identically and cannot silently diverge if the
// release.Input contract gains a field. It is a seadex-aware adapter so the
// release package can stay a pure, seadex-free leaf.
//
// Which FILES of a torrent are evidence is not decided here: that rule is
// internal/payload, a leaf this package reads (and the indexer's feed synthesis
// reads directly), so the file rule's two consumer sets no longer reach it
// through the compare/audit adapter (l-f195, h-f21).
package classify

import (
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
)

// --- AB visibility gates (adapters over filter) ---

// ABVisible reports whether a SeaDex torrent may surface under the operator's
// AnimeBytes toggle. It owns the raw-URL invariant shared by compare and audit:
// the guard inspects the RAW upstream URL (t.URL), never the published link,
// because publishing trusts the tracker label and would rewrite or erase the
// very host evidence the cross-check needs. Obtainability re-checks the label
// downstream as defense in depth.
func ABVisible(t *seadex.Torrent, includeAnimeBytes bool) bool {
	return filter.ABVisible(t.Tracker, t.URL, includeAnimeBytes)
}

// PublishURL returns the clickable tracker link for a SeaDex torrent, or "" when
// the publisher refused the raw upstream value (see trackerlink.Publish). It is
// the adapter that keeps the (tracker, rawURL) argument order in ONE place for
// every consumer of the SeaDex model, mirroring the ABVisible/Obtainable pattern
// - and it is why internal/seadex no longer carries this policy as a method on
// the wire struct: the publish half of the link concern now sits beside
// its hide half in filter, one layer below the flows.
func PublishURL(t *seadex.Torrent) string {
	return trackerlink.Publish(t.Tracker, t.URL)
}

// PublishRefusal is PublishURL plus the publisher's refusal reason, for the
// consumers that DIAGNOSE a drop rather than just render a link: the audit
// report's row marker and the SeaDex client's aggregate catalogue WARN both
// have to name a remedy, and an unknown tracker's remedy (an internal/tracker
// table entry, shipped in a release) is not the SeaDex record's (l-f127).
// Same argument-order invariant as PublishURL, one implementation of the
// policy (trackerlink.PublishReason).
func PublishRefusal(t *seadex.Torrent) (string, trackerlink.Refusal) {
	return trackerlink.PublishReason(t.Tracker, t.URL)
}

// Obtainable reports whether a classified SeaDex release is obtainability
// evidence under the operator's AnimeBytes toggle. It owns the argument
// invariant shared by compare and audit (mirroring ABVisible's adapter
// pattern): the RAW upstream URL (t.URL) feeds the tracker cross-check while
// the published link (PublishURL) is the grabbable one, in that order.
func Obtainable(rel *release.Release, t *seadex.Torrent, animeBytes bool) bool {
	return filter.Obtainable(rel, t.URL, PublishURL(t), animeBytes)
}

// ABEvidence grades the AnimeBytes evidence in a SeaDex torrent. Like ABVisible
// it owns the raw-URL invariant shared by compare and audit: the grading reads
// the RAW upstream URL (t.URL), never the published link, because publishing
// trusts the tracker label and would rewrite or erase the very host evidence the
// grading needs.
//
// Consumers pick their own fail direction over the grade. The audit report gates
// row VISIBILITY on ABDefinite (fail open: a definite AB row hides with the
// toggle off, while an ambiguous public-labeled row stays listed, annotated
// unobtainable), where ABVisible stays the fail-closed verdict-eligibility gate
// shared with compare.
func ABEvidence(t *seadex.Torrent) filter.ABEvidence {
	return filter.ClassifyAB(t.Tracker, t.URL)
}

// --- Torrent classification ---

// Torrent classifies one SeaDex torrent, in the context of its entry (for the
// shared notes), into a normalized release.Release. This is the one place the
// release.Input for a SeaDex torrent is built, so compare and audit classify
// the same release identically. DualAudio is the structured per-torrent SeaDex
// field passed through as-is — the same structured source as isBest — never
// sniffed from the entry notes, which are entry-wide (they describe every
// release in the entry and can even negate: "lacks dual audio") and so are
// unreliable per-release evidence.
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
// title synthesis is the consumer). Kept beside Torrent so every
// release.Input built from SeaDex data has one home.
func FileResolution(files []seadex.File) string {
	names := payload.Names(files)
	if len(names) == 0 {
		return ""
	}
	return release.Classify(&release.Input{Names: names}).Resolution
}

// --- Shared entry-state verdict rules ---

// DivergedIncomplete reports whether a diverged comparison of
// entry downgrades to the incomplete vocabulary (compare's
// StatusIncomplete, audit's QualifierIncomplete) - the one
// downgrade rule both flows must share, kept here beside
// Fallback so they cannot silently drift.
func DivergedIncomplete(entry *seadex.Entry) bool {
	return entry.Incomplete
}

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

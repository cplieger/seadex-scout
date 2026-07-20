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
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// ABVisible reports whether a SeaDex torrent may surface under the operator's
// AnimeBytes toggle. It owns the raw-URL invariant shared by compare and audit:
// the guard inspects the RAW upstream URL (t.URL), never t.UsableURL(), because
// that normalization trusts the tracker label and would rewrite or erase the
// very host evidence the cross-check needs. Obtainability re-checks the label
// downstream as defense in depth.
func ABVisible(t *seadex.Torrent, includeAnimeBytes bool) bool {
	return filter.ABVisible(t.Tracker, t.URL, includeAnimeBytes)
}

// Obtainable reports whether a classified SeaDex release is obtainability
// evidence under the operator's AnimeBytes toggle. It owns the argument
// invariant shared by compare and audit (mirroring ABVisible's adapter
// pattern): the RAW upstream URL (t.URL) feeds the tracker cross-check while
// the normalized t.UsableURL() is the grabbable link, in that order.
func Obtainable(rel *release.Release, t *seadex.Torrent, animeBytes bool) bool {
	return filter.Obtainable(rel, t.URL, t.UsableURL(), animeBytes)
}

// DefinitelyAB reports whether a SeaDex torrent is DEFINITIVELY AnimeBytes:
// by its tracker label, or by successfully extracted host evidence in its
// raw upstream URL. Like ABVisible it owns the raw-URL invariant (t.URL
// feeds the host cross-check, never t.UsableURL()), but unlike ABVisible it
// fails OPEN on malformed or ambiguous evidence. The audit report uses it
// for row VISIBILITY — a definite AB row hides with the toggle off, while an
// ambiguous public-labeled row stays listed, annotated unobtainable — where
// ABVisible stays the fail-closed verdict-eligibility gate shared with
// compare.
func DefinitelyAB(t *seadex.Torrent) bool {
	return filter.DefinitelyAB(t.Tracker, t.URL)
}

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
		Names:     torrentFileNames(t.Files),
		Notes:     entry.Notes,
		Group:     t.ReleaseGroup,
		Tracker:   t.Tracker,
		DualAudio: t.DualAudio,
	})
}

// torrentFileNames returns the file names the classifier parses: the primary
// payload — files at least half the byte length of the torrent's largest file,
// i.e. the normal episode/movie files — so one small extra (an NCED, a
// subtitle sidecar, a screenshot) whose name carries a marker such as BDRemux
// cannot override the main release's classification. When no file carries a
// positive length (fixtures or upstream records without sizes) every
// non-empty name is kept, preserving the previous all-names behavior.
func torrentFileNames(files []seadex.File) []string {
	var maxLength int64
	for i := range files {
		if files[i].Name != "" && files[i].Length > maxLength {
			maxLength = files[i].Length
		}
	}
	// Overflow-safe ceil-half: (maxLength+1)/2 wraps negative when an
	// untrusted length is math.MaxInt64, which would let every file survive.
	minPrimary := maxLength/2 + maxLength%2
	names := make([]string, 0, len(files))
	for i := range files {
		if files[i].Name == "" {
			continue
		}
		if maxLength > 0 && files[i].Length < minPrimary {
			continue
		}
		names = append(names, files[i].Name)
	}
	return names
}

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

// Package classify houses the shared SeaDex-to-release classification glue: the
// single construction of a release.Release from a seadex.Torrent (in the
// context of its entry) that both the compare (findings) and audit (report)
// flows depend on. Keeping it in one place means the two flows classify an
// identical SeaDex release identically and cannot silently diverge if the
// release.Input contract gains a field. It is a seadex-aware adapter so the
// release package can stay a pure, seadex-free leaf.
package classify

import (
	"path"
	"regexp"
	"strings"

	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// --- AB visibility gates (adapters over filter) ---

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

// --- Torrent classification + payload eligibility ---

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
		Names:     PayloadNames(t.Files),
		Notes:     entry.Notes,
		Group:     t.ReleaseGroup,
		Tracker:   t.Tracker,
		DualAudio: t.DualAudio,
	})
}

// FileResolution classifies a torrent's resolution from its file names
// alone, over the shared PayloadNames eligibility rule. The entry notes are
// deliberately excluded: they are entry-wide and routinely describe sibling
// releases, so they must not stamp a per-torrent title (the indexer's RSS
// title synthesis is the consumer). Kept beside Torrent so every
// release.Input built from SeaDex data has one home.
func FileResolution(files []seadex.File) string {
	names := PayloadNames(files)
	if len(names) == 0 {
		return ""
	}
	return release.Classify(&release.Input{Names: names}).Resolution
}

// PayloadNames returns the file names eligible as classification evidence for
// a torrent's release: the ONE layered eligibility rule shared by the
// compare/audit classification (Torrent above) and the indexer's synthesized
// feed title (its fileResolution), so a daemon finding and the RSS title can
// never disagree about which files vote (h-f3).
//
//  1. Type gate: when the record carries names, a file whose name fails
//     ContentMediaFile (a non-video extension such as a subtitle sidecar or
//     screenshot, or a creditless NCOP/NCED extra) is dropped — an extra
//     cannot vote whatever its size, so even an NCED as large as the payload
//     never drives the classification.
//  2. Size refinement: among the surviving content files, only those at
//     least half the byte length of the largest survivor count, so a small
//     bonus video that passes the type gate by name silence (a featurette,
//     a sampler) cannot dilute the primary payload's verdict — and in a
//     mixed-resolution batch the small specials deliberately do not vote:
//     the release is headlined by its main content.
//
// Two fallbacks keep the rule total. Files without positive lengths skip the
// size refinement (the type gate alone decides — fixtures and sparse
// upstream records). And a torrent whose names ALL fail the type gate (an
// unlisted container such as an .iso remux, or a sidecar-only list) falls
// back to the size rule over every non-empty name — the historical
// size-only behavior — so real content can never lose all its evidence to
// the extension list.
func PayloadNames(files []seadex.File) []string {
	pool := eligiblePool(files)
	var maxLength int64
	for i := range pool {
		if pool[i].Length > maxLength {
			maxLength = pool[i].Length
		}
	}
	// Overflow-safe ceil-half: (maxLength+1)/2 wraps negative when an
	// untrusted length is math.MaxInt64, which would let every file survive.
	minPrimary := maxLength/2 + maxLength%2
	names := make([]string, 0, len(pool))
	for i := range pool {
		if maxLength > 0 && pool[i].Length < minPrimary {
			continue
		}
		names = append(names, pool[i].Name)
	}
	return names
}

// eligiblePool selects the files PayloadNames' size refinement runs over:
// the type gate's content survivors, or — when none survive — every named
// file (the unlisted-container / sidecar-only / creditless-only fallback).
func eligiblePool(files []seadex.File) []seadex.File {
	pool := make([]seadex.File, 0, len(files))
	for i := range files {
		if files[i].Name != "" && ContentMediaFile(files[i].Name) {
			pool = append(pool, files[i])
		}
	}
	if len(pool) > 0 {
		return pool
	}
	for i := range files {
		if files[i].Name != "" {
			pool = append(pool, files[i])
		}
	}
	return pool
}

// ContentMediaFile reports whether name is eligible BY TYPE to identify
// release content: a known video container extension (IsMediaFile) and not a
// creditless extra (IsCreditlessExtra). It is PayloadNames' type gate and
// the predicate the indexer's title/pack synthesis scanners share, so "what
// counts as a content file" has one home.
func ContentMediaFile(name string) bool {
	return IsMediaFile(name) && !IsCreditlessExtra(name)
}

// IsMediaFile reports whether name carries a known video container
// extension — an episode/movie file rather than a sidecar (subtitles,
// fonts, screenshots) that happens to carry an episode token.
func IsMediaFile(name string) bool {
	return mediaExts[strings.ToLower(path.Ext(name))]
}

// IsCreditlessExtra reports whether name marks a creditless bonus OP/ED file
// (NCOP/NCED/creditless, optionally numbered and versioned) — an extra that
// may carry absolute-looking numbers and quality markers but must never
// identify the release.
func IsCreditlessExtra(name string) bool {
	return creditlessExtra.MatchString(name)
}

// creditlessExtra matches bonus OP/ED files that may carry absolute-looking
// numbers ("NCED01v2") which must not read as episodes or classification
// evidence. Explicit case classes instead of a global (?i): the release
// marker engine uses strings.ToLower-faithful classes because Go regexp's
// SimpleFold diverges from ToLower on U+0130 (İ, which must match as I/i)
// and U+017F (ſ, which must NOT match as S/s), and this marker feeds the
// same classification pipeline. Explicit ASCII-alnum boundaries instead of
// \b: underscore is a regexp word character, but the rest of the
// classification stack treats it as a scene delimiter, so an
// underscore-delimited extra ("NCED_01", "creditless_OP") must still read
// as creditless.
var creditlessExtra = regexp.MustCompile(
	`(?:^|[^[:alnum:]])(?:[Nn][Cc][Oo][Pp]|[Nn][Cc][Ee][Dd]|` +
		`[Cc][Rr][Ee][Dd][Ii\x{0130}][Tt][Ll][Ee][Ss][Ss])\d*(?:[Vv]\d+)?(?:$|[^[:alnum:]])`,
)

// mediaExts are the video container extensions used to tell an episode/movie
// file from a sidecar file (subtitles, samples) when scanning a torrent's
// files.
var mediaExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m2ts": true,
	".ts": true, ".ogm": true, ".mov": true, ".wmv": true, ".webm": true,
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

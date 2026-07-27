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
	"slices"
	"strings"

	"github.com/cplieger/seadex-scout/internal/filter"
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
// the wire struct (l-f86): the publish half of the link concern now sits beside
// its hide half in filter, one layer below the flows.
func PublishURL(t *seadex.Torrent) string {
	return trackerlink.Publish(t.Tracker, t.URL)
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
	payload := PayloadFiles(files)
	names := make([]string, 0, len(payload))
	for i := range payload {
		names = append(names, payload[i].Name)
	}
	return names
}

// PayloadFiles returns the files PayloadNames draws its names from: the same
// layered type-gate-then-size-refinement eligibility rule, kept in file form
// so a caller that needs a file's length or its position in the record judges
// the same payload the classification does instead of the raw file list.
// PayloadNames is its only caller today; the indexer's title and pack
// synthesis scanners (representativeFile, coveredEpisodes, seasonCounts)
// deliberately do NOT use this rule - they run an episode census and take
// PopulationFiles, because the max-anchored floor below deletes every regular
// episode of a pack carrying one double-length file.
//
// It answers "which files are evidence for THIS RELEASE'S quality attributes"
// - resolution, codec, group - so it deliberately keeps only the primary
// payload. A caller counting how many distinct EPISODES a torrent spans wants
// PopulationFiles instead: there a shorter file is a legitimate episode, not a
// diluting extra.
func PayloadFiles(files []seadex.File) []seadex.File {
	pool := eligiblePool(files)
	// The anchor is the pool MAXIMUM: anything far below the primary payload is
	// an extra and must not dilute the release's quality verdict.
	var maxLength int64
	for i := range pool {
		if pool[i].Length > maxLength {
			maxLength = pool[i].Length
		}
	}
	return keepAtLeast(pool, halfFloor(maxLength))
}

// PopulationFiles returns the files an EPISODE CENSUS runs over: the same type
// gate and the same two totality fallbacks as PayloadFiles, but a size floor
// anchored on the pool's MEDIAN length rather than its maximum.
//
// The distinction is the whole point of having two rules. PayloadFiles asks
// which files vote on the release's quality attributes, so anything far below
// the primary payload is an extra and must not dilute the verdict. A census
// asks how many distinct episodes the torrent spans, and there a shorter file
// is usually a legitimately shorter episode - a 12-minute entry in a 24-minute
// season, a recap. Reusing the max-anchored floor for counting therefore
// deletes real episodes whenever ONE file is more than twice its siblings: a
// season pack with a double-length premiere, or a batch that also bundles the
// franchise movie, loses every regular episode, reads as a single episode, and
// is served under that episode's own SxxExx marker instead of collapsing to
// the season label - the inverse of the pack-collapse contract the feed
// documents, and a release Sonarr then grabs as one episode without its
// FullSeason ranking.
//
// The median is robust to an outlier in EITHER direction, which is exactly the
// requirement: one over-long file cannot lift the floor above the episode
// population, and one tiny sample cannot lower it. The floor stays at half the
// median so an episode-shaped sample far below the real episodes is still
// excluded (it cannot inflate a lone episode into a "pack"). A torrent whose
// files are MOSTLY sub-episode samples defeats the median and counts them;
// that shape does not occur in a curated release record, and admitting it is
// the safer failure than deleting real episodes.
func PopulationFiles(files []seadex.File) []seadex.File {
	pool := eligiblePool(files)
	// The anchor is the pool MEDIAN, robust to an outlier in either direction:
	// one over-long file cannot lift the floor above the episode population and
	// one tiny sample cannot lower it.
	return keepAtLeast(pool, halfFloor(medianLength(pool)))
}

// halfFloor returns the overflow-safe ceil-half of a size anchor: the minimum
// length a file must carry to survive a size refinement. The halving happens
// before the rounding correction because (anchor+1)/2 wraps negative when an
// untrusted record carries math.MaxInt64, which would let every file survive.
// It is shared so PayloadFiles and PopulationFiles cannot drift apart on the
// arithmetic - only on their ANCHOR, which is their whole difference.
func halfFloor(anchor int64) int64 { return anchor/2 + anchor%2 }

// keepAtLeast returns the pool members whose length reaches floor. A floor that
// is not positive keeps the whole pool: that is the shared totality fallback
// for a record carrying no positive lengths (sparse upstream data, fixtures),
// where the type gate alone decides.
func keepAtLeast(pool []seadex.File, floor int64) []seadex.File {
	out := make([]seadex.File, 0, len(pool))
	for i := range pool {
		if floor > 0 && pool[i].Length < floor {
			continue
		}
		out = append(out, pool[i])
	}
	return out
}

// medianLength returns the median length of pool, or 0 for an empty pool: the
// exact middle length on an odd-size pool, and the MIDPOINT of the two central
// lengths on an even-size one.
//
// The even case may not take the upper middle. On a two-file pool the upper
// middle IS the maximum, so a single over-long file lifts the floor above its
// sibling episode - exactly the max-anchored behavior PopulationFiles exists to
// avoid, and the reason a two-episode pack whose finale (or bundled franchise
// movie) runs more than twice its sibling counted as ONE episode.
//
// Overflow safety is kept without adding two untrusted lengths: the midpoint is
// computed as lo+(hi-lo)/2 over the two central values clamped at 0, so a
// math.MaxInt64 length cannot wrap the sum and a negative length (the wire type
// is a plain int64 with no upstream validation) cannot wrap the difference.
func medianLength(pool []seadex.File) int64 {
	if len(pool) == 0 {
		return 0
	}
	lengths := make([]int64, 0, len(pool))
	for i := range pool {
		lengths = append(lengths, pool[i].Length)
	}
	slices.Sort(lengths)
	mid := len(lengths) / 2
	if len(lengths)%2 == 1 {
		return lengths[mid]
	}
	lo, hi := max(0, lengths[mid-1]), max(0, lengths[mid])
	return lo + (hi-lo)/2
}

// eligiblePool selects the files the size refinements of PayloadFiles and
// PopulationFiles run over: the type gate's content survivors, or — when none
// survive — every named file (the unlisted-container / sidecar-only /
// creditless-only fallback).
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

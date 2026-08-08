// Package payload owns ONE rule: which of a SeaDex torrent's files are
// evidence about the release, and for which question. It is a pure leaf over
// the SeaDex file model - the type gate (a video container that is neither a
// creditless extra nor a sample clip), the primary-payload size rule the
// quality classification votes on (Names), and the episode-census size rule a
// file COUNT runs over (Population).
//
// It is a package of its own because those two size rules and the type gate
// they share are one concern with two consumer sets that change for
// independent reasons (l-f195, h-f21): internal/classify builds a
// release.Input from the primary payload for the compare/audit flows, while
// internal/indexer's RSS title and pack synthesis runs the census. Held inside
// classify, the census rule lived in a package whose declared job is
// compare/audit sharing, the indexer had to depend on that whole surface (and
// transitively on the entry model, filter and trackerlink) to reach a
// file-name predicate, and - the reason this is a repair and not a
// preventive reshape - the invariant the two size rules must not violate was
// documented and TESTED on the indexer's side of the boundary while the code
// deciding it sat in classify. Routing the census through the primary-payload
// floor there deleted every regular episode of a pack carrying one over-long
// file, and only a diff review caught it.
//
// h-f3's invariant is preserved and strengthened: "which files vote" has one
// home, and now that home also owns the census, so the two floors can be
// compared - and their difference asserted - in one package's tests.
//
// The runner-up shapes were moving the census INTO internal/indexer beside its
// token scanners (which splits the shared type gate and the halving arithmetic
// across two packages) and moving the indexer's token vocabulary INTO classify
// (which grows the package this extraction is shrinking, and pulls RSS title
// synthesis into the compare/audit adapter).
package payload

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/cplieger/seadex-scout/internal/nametoken"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// Names returns the file names eligible as classification evidence for a
// torrent's release: the ONE layered eligibility rule shared by the
// compare/audit classification (classify.Torrent) and the indexer's
// synthesized feed title (its fileResolution), so a daemon finding and the RSS
// title can never disagree about which files vote.
//
//  1. Type gate: when the record carries names, a file whose name fails
//     ContentMediaFile (a non-video extension such as a subtitle sidecar or
//     screenshot, a creditless NCOP/NCED extra, or a sample clip) is dropped —
//     an extra cannot vote whatever its size, so even an NCED as large as the
//     payload never drives the classification.
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
func Names(files []seadex.File) []string {
	primary := primaryFiles(files)
	names := make([]string, 0, len(primary))
	for i := range primary {
		names = append(names, primary[i].Name)
	}
	return names
}

// primaryFiles returns the files Names draws its names from: the same layered
// type-gate-then-size-refinement eligibility rule, kept in file form so a
// caller that needs a file's length or its position in the record judges the
// same payload the classification does instead of the raw file list.
//
// It answers "which files are evidence for THIS RELEASE'S quality attributes"
// - resolution, codec, group - so it deliberately keeps only the primary
// payload. A caller counting how many distinct EPISODES a torrent spans wants
// Population instead: there a shorter file is a legitimate episode, not a
// diluting extra, and the max-anchored floor below deletes every regular
// episode of a pack carrying one double-length file.
func primaryFiles(files []seadex.File) []seadex.File {
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

// Population returns the files an EPISODE CENSUS runs over: the same type
// gate and the same two totality fallbacks as the primary-payload rule, but a
// size floor anchored on the pool's MEDIAN length rather than its maximum.
//
// The distinction is the whole point of having two rules. primaryFiles asks
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
// population, and one tiny sample cannot lower it. On an even-size pool the
// anchor is the LOWER of the two central lengths, because any statistic above
// it degenerates to the maximum on a two-file pool - and a two-file pool is a
// real shape here (a two-part OVA, a season pair whose finale or bundled
// franchise movie runs several times its sibling), where the max-anchored
// reading is the very deletion this rule exists to avoid (l-f234 /
// d-gpt-u3c1-1).
//
// Sample exclusion therefore does NOT rest on the size floor: a marked sample
// clip is dropped by the type gate (IsSampleExtra), by NAME, whatever its
// size. That is what makes the permissive floor affordable - size alone cannot
// separate a 200 MiB sample beside a 1 GiB episode from a 1 GiB episode beside
// a 4 GiB finale, since the two ratios overlap, so the discriminator has to be
// evidence other than size. The residual size floor still keeps an UNMARKED
// bonus video far below the episode population from inflating a lone episode
// into a "pack". A torrent whose files are MOSTLY sub-episode samples defeats
// the median and counts them; that shape does not occur in a curated release
// record, and admitting it is the safer failure than deleting real episodes.
//
// The result is a FILE SUBSET, not a census verdict: a consumer that also
// needs the type gate applied to what it counts (the indexer's episode
// scanners do, because the no-type-survivor fallback above deliberately
// re-admits unlisted containers and sidecars) re-applies ContentMediaFile
// itself. Counting the fallback's survivors would count an .iso container or a
// subtitle sidecar as an episode.
func Population(files []seadex.File) []seadex.File {
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
// It is shared so primaryFiles and Population cannot drift apart on the
// arithmetic - only on their ANCHOR, which is their whole difference.
func halfFloor(anchor int64) int64 { return anchor/2 + anchor%2 }

// keepAtLeast returns the pool members whose length reaches floor. A floor that
// is not positive keeps the whole pool: that is the shared totality fallback
// for a record whose size evidence cannot discriminate (sparse upstream data,
// fixtures), where the type gate alone decides. What reaches that state differs
// by anchor: primaryFiles' maximum is non-positive only when NO length is
// positive, while Population's median is non-positive once the pool's
// central lengths are - more than half the pool on an odd-size pool, and half
// of it on an even one, where the lower of the two central lengths is the
// anchor - so a census over lengths [0, 0, 0, 1000] deliberately keeps all four
// files rather than reading the one real file as the whole population.
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
// exact middle length on an odd-size pool, and the LOWER of the two central
// lengths on an even-size one.
//
// The even case may take neither the upper middle nor the midpoint of the two.
// On a two-file pool the upper middle IS the maximum, so a single over-long
// file lifts the floor above its sibling episode - exactly the max-anchored
// behavior Population exists to avoid, and the reason a two-episode pack whose
// finale (or bundled franchise movie) ran more than twice its sibling counted
// as ONE episode. The midpoint only widened that band: it kept the smaller file
// while the larger ran up to ~3x it, so a 1 GiB episode beside a 4 GiB finale
// was still deleted. Both bounds tried to separate a sample from a short
// episode by SIZE, and those two populations overlap - a 200 MiB sample beside
// a 1 GiB episode is a smaller ratio than a real 1 GiB episode beside a 4 GiB
// one, so no threshold can hold both claims (l-f234 / d-gpt-u3c1-1).
//
// The lower middle resolves it in the direction this package already states is
// the safer failure (admitting an extra beats deleting a real episode), and the
// sample guard moves to the evidence that actually distinguishes a sample: its
// NAME (IsSampleExtra, applied in the type gate above, before any size rule
// runs). The runner-ups were a fixed ratio cut somewhere between the two
// observed populations - arbitrary, and still lost a real pack skewed past it -
// and importing the resolution classifier so a 480p file beside a 1080p one
// could be read as an extra, which buys a narrower signal for a dependency this
// leaf does not otherwise need.
//
// Overflow safety needs no addition of two untrusted lengths: the anchor is one
// of the pool's own values, clamped at 0 so a negative wire length (the wire
// type is a plain int64 with no upstream validation) cannot drive the floor
// negative.
func medianLength(pool []seadex.File) int64 {
	if len(pool) == 0 {
		return 0
	}
	lengths := make([]int64, 0, len(pool))
	for i := range pool {
		lengths = append(lengths, pool[i].Length)
	}
	slices.Sort(lengths)
	// (len-1)/2 is the exact middle on an odd-size pool and the lower of the
	// two central lengths on an even one.
	return max(0, lengths[(len(lengths)-1)/2])
}

// eligiblePool selects the files the size refinements of primaryFiles and
// Population run over: the type gate's content survivors, or — when none
// survive — every named file (the unlisted-container / sidecar-only /
// creditless-only / sample-only fallback).
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
// release content: a known video container extension (IsMediaFile) that is
// neither a creditless extra (IsCreditlessExtra) nor a sample clip
// (IsSampleExtra). It is Names' type gate and the predicate the indexer's
// title/pack synthesis scanners share, so "what counts as a content file" has
// one home.
func ContentMediaFile(name string) bool {
	return IsMediaFile(name) && !IsCreditlessExtra(name) && !IsSampleExtra(name)
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
// evidence. The marker letters are the shared strings.ToLower-faithful case
// classes (nametoken.Literal, via Alternation) instead of a global (?i),
// because Go regexp's SimpleFold diverges from ToLower on U+0130 (İ, which must
// match as I/i) and U+017F (ſ, which must NOT match as S/s), and this marker
// feeds the same classification pipeline internal/release does - the two must
// read one vocabulary. Boundaries are the shared release-name token edge
// (nametoken.NonWordEdge), not \b: underscore is a regexp word character while
// the rest of the classification stack treats it as a scene delimiter (so an
// underscore-delimited extra - "NCED_01", "creditless_OP" - must still read as
// creditless), and the shared edge is what keeps this marker's letters and its
// boundaries reading ONE alphabet - a name carrying U+0130 or U+212A abutting a
// marker continues the token, exactly as internal/release reads the same
// construct.
var creditlessExtra = regexp.MustCompile(
	`(?:^|` + nametoken.NonWordEdge + `)(?:` +
		nametoken.Alternation([]string{"ncop", "nced", "creditless"}) +
		`)\d*(?:` + nametoken.Literal("v") + `\d+)?(?:$|` + nametoken.NonWordEdge + `)`,
)

// IsSampleExtra reports whether name marks a SAMPLE clip (the near-universal
// scene marker for a short excerpt of the payload, optionally numbered) — an
// extra that carries the payload's own episode token and quality markers and so
// is indistinguishable from a real episode by size alone.
//
// It is the census's sample guard, moved off the size floor: a sample sits at a
// ratio to the real payload that overlaps the ratio between two REAL episodes
// of unequal length, so a size threshold cannot exclude one without deleting the
// other (see medianLength). Excluding it by name lets the census floor be
// permissive enough to keep a strongly skewed real pack.
//
// A file name containing nothing but this marker is still total: a torrent whose
// names ALL fail the type gate falls back to every named file (eligiblePool), so
// a sample-only or oddly named record never loses all its evidence.
//
// Same case-class and boundary discipline as creditlessExtra, and deliberately
// only this one token: "sample" is what scene naming and the tracker-side
// samples actually use, and every additional guess (preview, teaser, menu)
// would risk a real title word for a shape not observed in the SeaDex
// catalogue.
func IsSampleExtra(name string) bool {
	return sampleExtra.MatchString(name)
}

// sampleExtra matches sample clips ("sample", "Sample01", a "Sample/"
// directory). Shared case classes instead of a global (?i) and the shared
// release-name token edge (nametoken.NonWordEdge) instead of \b, for the same
// two reasons creditlessExtra spells out: Go regexp's SimpleFold diverges from
// strings.ToLower on the Unicode folds this classification pipeline cares about,
// and underscore is a regexp word character while the rest of the stack treats
// it as a scene delimiter (so "Show_sample_01.mkv" must still read as a
// sample). Reading the shared edge is what keeps this marker's letters and its
// boundaries reading ONE alphabet - a name carrying U+0130 or U+212A abutting
// the marker continues the token, exactly as internal/release reads the same
// construct.
var sampleExtra = regexp.MustCompile(
	`(?:^|` + nametoken.NonWordEdge + `)` + nametoken.Literal("sample") + `\d*(?:$|` + nametoken.NonWordEdge + `)`,
)

// mediaExts are the video container extensions used to tell an episode/movie
// file from a sidecar file (subtitles, samples) when scanning a torrent's
// files.
var mediaExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m2ts": true,
	".ts": true, ".ogm": true, ".mov": true, ".wmv": true, ".webm": true,
}

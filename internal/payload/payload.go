// Package payload owns ONE rule: which of a SeaDex torrent's files are
// evidence about the release, and for which question. It is a pure leaf over
// the SeaDex file model - the type gate (a video container that is neither a
// creditless extra nor a sample clip), the primary-payload size rule the
// quality classification votes on (Names), and the episode-census size rule a
// file COUNT runs over (Population).
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
// The distinction is the whole point of having two rules.
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
func halfFloor(anchor int64) int64 { return anchor/2 + anchor%2 }

// keepAtLeast returns the pool members whose length reaches floor. A floor that
// is not positive keeps the whole pool: that is the shared totality fallback
// for a record whose size evidence cannot discriminate (sparse upstream data,
// fixtures), where the type gate alone decides.
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
// (IsSampleExtra).
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
// evidence.
var creditlessExtra = regexp.MustCompile(
	`(?:^|` + nametoken.NonWordEdge + `)(?:` +
		nametoken.Alternation([]string{"ncop", "nced", "creditless"}) +
		`)\d*(?:` + nametoken.Literal("v") + `\d+)?(?:$|` + nametoken.NonWordEdge + `)`,
)

// IsSampleExtra reports whether name marks a SAMPLE clip (the near-universal
// scene marker for a short excerpt of the payload, optionally numbered) — an
// extra that carries the payload's own episode token and quality markers and so
// is indistinguishable from a real episode by size alone.
func IsSampleExtra(name string) bool {
	return sampleExtra.MatchString(name)
}

// sampleExtra matches sample clips ("sample", "Sample01", a "Sample/"
// directory).
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

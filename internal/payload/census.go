package payload

import (
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/nametoken"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// EpisodeToken matches a season+episode token (S01E01, S1E1, S01E01-E13, S01E15v2),
// captured in group 1 with its season half in group 2. The three patterns are
// shared read-only; nothing reassigns them.
var EpisodeToken = regexp.MustCompile(
	`((` + nametoken.Literal("S") + `\d{1,2})` + nametoken.Literal("E") + `\d{1,4}` +
		`(?:-` + nametoken.Literal("E") + `?\d{1,4})?(?:` + nametoken.Literal("v") + `\d+)?)` +
		`(?:` + nametoken.NonWordEdge + `|$)`,
)

// AbsoluteEpisode matches an absolute episode number in the fansub "- 07" form
// (optional version suffix), with the episode number captured in group 1. The
// delimiters accept underscores as well as spaces: underscore-named releases
// ("_Show_-_01_") use "_" everywhere a space would sit, and matching only the
// space-dash form made such packs read as a single episode.
var AbsoluteEpisode = regexp.MustCompile(`[\s_]-[\s_](\d{1,4}(?:v\d+)?)(?:[\s_]|$)`)

// EpisodeVersion strips a trailing vN revision from an episode token so a v2
// replacement of the same episode never counts as a second episode. The v is
// the shared case class (nametoken.Literal), which for a letter with no
// non-ASCII fold is exactly what (?i)v was - it reads from the one home rather
// than restating the rule.
var EpisodeVersion = regexp.MustCompile(nametoken.Literal("v") + `\d+$`)

// LastSubmatchIndex returns the submatch index pairs of the LAST non-overlapping match
// of re in s, or nil when there is none.
func LastSubmatchIndex(re *regexp.Regexp, s string) []int {
	all := re.FindAllStringSubmatchIndex(s, -1)
	if len(all) == 0 {
		return nil
	}
	return all[len(all)-1]
}

// StripExt drops a trailing known video extension from a file name, leaving any
// other trailing dotted token (a release name is not a path) intact.
func StripExt(name string) string {
	if !isMediaFile(name) {
		return name
	}
	return name[:len(name)-len(path.Ext(name))]
}

// HasEpisodeEvidence reports whether a path fragment carries an episode
// identity - an SxxExx token or an absolute "- NN" number. It is the ONE
// predicate every episode-evidence reader shares, so they cannot disagree about
// which fragment names the episode.
func HasEpisodeEvidence(s string) bool {
	return EpisodeToken.MatchString(s) || AbsoluteEpisode.MatchString(s)
}

// Census narrows a file list to the population the episode census
// counts over: the episode pool, then content media files only.
func Census(files []seadex.File) []seadex.File {
	files = Population(files)
	kept := make([]seadex.File, 0, len(files))
	for i := range files {
		if ContentMediaFile(files[i].Name) {
			kept = append(kept, files[i])
		}
	}
	return kept
}

// DistinctEpisodes counts the distinct episodes a census population spans,
// keying on the SxxExx token first and the "- NN" absolute-episode form (space-
// or underscore-delimited) as a fallback. Creditless extras (NCED/NCOP) and
// other sidecars carry neither token and are not counted, so an episode bundled
// with its creditless files still reads as a single episode.
func DistinctEpisodes(files []seadex.File) int {
	seen := make(map[string]struct{})
	for i := range files {
		base := EpisodeKeyBase(files[i].Name)
		qualifier := sharedTokenQualifier(files[i].Name)
		if l := LastSubmatchIndex(EpisodeToken, base); l != nil {
			// Key on the LAST token: scene naming puts the episode marker
			// after the title, so a title containing an SxxExx-shaped
			// substring must not shadow the real episode marker.
			tok := strings.ToUpper(base[l[2]:l[3]])
			seen["e"+EpisodeVersion.ReplaceAllString(tok, "")+qualifier] = struct{}{}
			continue
		}
		if l := LastSubmatchIndex(AbsoluteEpisode, base); l != nil {
			tok := base[l[2]:l[3]]
			seen["a"+EpisodeVersion.ReplaceAllString(tok, "")+qualifier] = struct{}{}
		}
	}
	return len(seen)
}

// EpisodeKeyBase picks the portion of a file's name its episode identity is
// read from: the file's OWN base name when that carries episode evidence
// (an SxxExx token or an absolute "- NN" number), else the full path - so a
// pack whose only episode tokens live in a directory component still keys per
// directory. Reading the full path unconditionally let a shared directory
// token (a batch folder named "... S01E01-E12 ...") shadow every file's own
// absolute number, collapsing a whole season pack onto ONE episode key: the
// pack then read as a single episode and was served titled as episode 1.
func EpisodeKeyBase(name string) string {
	base := StripExt(path.Base(name))
	if HasEpisodeEvidence(base) {
		return base
	}
	return StripExt(name)
}

// sharedTokenQualifier returns the per-file suffix an episode key needs when
// the token EpisodeKeyBase found does NOT come from the file's own base name.
func sharedTokenQualifier(name string) string {
	own := StripExt(path.Base(name))
	if HasEpisodeEvidence(own) {
		return ""
	}
	return "|" + EpisodeVersion.ReplaceAllString(own, "")
}

// TotalSize sums the byte lengths of a torrent's files, every file counted
// whatever its type. The lengths come from the untrusted SeaDex record, so a
// negative length or a sum past math.MaxInt64 returns 0, which every caller
// reads as an unknown size.
func TotalSize(files []seadex.File) int64 {
	var n int64
	for i := range files {
		length := files[i].Length
		if length < 0 || length > math.MaxInt64-n {
			return 0
		}
		n += length
	}
	return n
}

// EpisodeSpan is the run of episodes one file's token names: an SxxExx token's
// season and episode range, or Season AbsoluteSeason for an absolute "- NN"
// number. First and Last are inclusive and equal for one episode.
type EpisodeSpan struct {
	Season, First, Last int
}

// AbsoluteSeason is the Season of a span read from an absolute "- NN" number.
const AbsoluteSeason = -1

// maxSpanEpisodes bounds one token's range: the regex admits four-digit
// numbers, and a consumer iterates every episode of a span.
const maxSpanEpisodes = 2000

// seasonEpisodeToken splits an upper-cased, version-stripped EpisodeToken match.
var seasonEpisodeToken = regexp.MustCompile(`^S(\d{1,2})E(\d{1,4})(?:-E?(\d{1,4}))?$`)

// Spans returns the episode span of every file in a census population, and
// false when any file names no episode of its own: no token, a token only a
// shared directory carries, or a range that runs backwards or past
// maxSpanEpisodes.
func Spans(files []seadex.File) ([]EpisodeSpan, bool) {
	spans := make([]EpisodeSpan, 0, len(files))
	for i := range files {
		if sharedTokenQualifier(files[i].Name) != "" {
			return nil, false
		}
		span, ok := fileSpan(EpisodeKeyBase(files[i].Name))
		if !ok {
			return nil, false
		}
		spans = append(spans, span)
	}
	return spans, true
}

// fileSpan reads base's last episode token, the one DistinctEpisodes keys on.
func fileSpan(base string) (EpisodeSpan, bool) {
	if l := LastSubmatchIndex(EpisodeToken, base); l != nil {
		tok := EpisodeVersion.ReplaceAllString(strings.ToUpper(base[l[2]:l[3]]), "")
		m := seasonEpisodeToken.FindStringSubmatch(tok)
		if m == nil {
			return EpisodeSpan{}, false
		}
		season, _ := strconv.Atoi(m[1])
		first, _ := strconv.Atoi(m[2])
		last := first
		if m[3] != "" {
			last, _ = strconv.Atoi(m[3])
		}
		return checkedSpan(season, first, last)
	}
	if l := LastSubmatchIndex(AbsoluteEpisode, base); l != nil {
		n, err := strconv.Atoi(EpisodeVersion.ReplaceAllString(base[l[2]:l[3]], ""))
		if err != nil {
			return EpisodeSpan{}, false
		}
		return checkedSpan(AbsoluteSeason, n, n)
	}
	return EpisodeSpan{}, false
}

func checkedSpan(season, first, last int) (EpisodeSpan, bool) {
	if first <= 0 || last < first || last-first >= maxSpanEpisodes {
		return EpisodeSpan{}, false
	}
	return EpisodeSpan{Season: season, First: first, Last: last}, true
}

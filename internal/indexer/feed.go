package indexer

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// defaultSeaDexBaseURL is the SeaDex site base the writer builds per-item
// info links under (entryURL) and the reader's InfoURL allowlist is derived
// from (reload.go's seadexInfoHost) - the ONE source for both ends of the
// persisted feed.json contract. It references the canonical constant in
// internal/seadex (the package that owns the releases.moe contract) so it
// cannot drift from it.
const defaultSeaDexBaseURL = seadex.DefaultBaseURL

// --- Per-show metadata and categories ---

// EntryInfo is the per-show (per-AniList-id) metadata the compare cycle hands
// the feed writer for title synthesis: the show's own title as its arr knows it
// (or the AniList canonical title as fallback; empty when neither is known),
// its release year, the season the entry maps to, and whether it is a movie.
// The cycle builds it from persisted state only (the mapping index, the last
// library snapshot, the AniList memo), so the feed rebuild stays
// arr-independent. The zero value is valid: synthesis then falls back to
// file-name derivation and the anime category.
type EntryInfo struct {
	Title string
	Year  int
	// Season is the season number this entry's releases belong to, and
	// SeasonKnown reports whether one was resolved at all. The pair arrives
	// ALREADY RESOLVED from the producer (scout.feedEntryInfo): interpreting
	// Fribb's raw season/typing fields is the mapping layer's semantics, and
	// this package deliberately imports neither align nor mapping, so it must
	// not re-derive that rule from raw fields. An entry with no
	// resolvable season - an absolute-numbered run, a title-only match, or an
	// unmapped entry - has SeasonKnown false and Season 0, and a resolved
	// season is authoritative over any season the file names carry (fansub
	// episode naming is cour-local).
	Season      int
	SeasonKnown bool
	IsMovie     bool
}

// EntryInfoFunc resolves the per-show (per-AniList-id) metadata the feed
// writer synthesizes RSS titles and categories from. It is total: an id the
// producer knows nothing about yields the zero EntryInfo (file-name
// fallback, anime category). The compare cycle supplies it; see
// entryInfoFunc for the nil-safe wrapper.
type EntryInfoFunc func(alID int) EntryInfo

// entryInfoFunc normalizes a possibly-nil per-show metadata callback to a
// total function returning the zero EntryInfo (file-name fallback, anime
// category), so the journal and harvest paths never nil-check it.
func entryInfoFunc(info EntryInfoFunc) EntryInfoFunc {
	if info != nil {
		return info
	}
	return func(int) EntryInfo { return EntryInfo{} }
}

// categoriesFor maps a show's Fribb typing to its Torznab categories: a movie
// routes to Movies (Radarr) and everything else - TV, OVA, ONA, SPECIAL, or an
// unmapped entry - to Anime (Sonarr). Defaulting the unknown case to anime is
// deliberate: a single-file OVA/special looks just like a movie by file name,
// so the failure that matters (a special mis-routed to Radarr, where it can
// never match) is avoided at the cost of a rare unmapped film not surfacing on
// Radarr's RSS view.
func categoriesFor(isMovie bool) []int {
	if isMovie {
		return []int{catMovies}
	}
	return []int{catAnime}
}

// --- Assembled title synthesis (known show title) ---

// synthesizeTitle builds the served release title for one curated torrent.
//
// With a known show title (meta.Title, the arr's own title or the AniList
// canonical title) the title is assembled, not derived: the show title, a
// season/episode marker computed from the Fribb season and the full file-list
// span (see episodeMarker), and the real release flags this app actually holds
// (see releaseFlags) - so the arr parses back a title built from its own
// vocabulary. A movie is "{Title} ({Year})" instead of a marker. Without a
// show title the file-name derivation (derivedTitle) is the permanent last
// resort.
func synthesizeTitle(t *seadex.Torrent, meta EntryInfo) string {
	title := strings.TrimSpace(meta.Title)
	if title == "" {
		return derivedTitle(t, meta)
	}
	parts := []string{title}
	switch {
	case meta.IsMovie:
		if meta.Year > 0 {
			parts[0] = fmt.Sprintf("%s (%d)", title, meta.Year)
		}
	default:
		if marker := episodeMarker(t, meta); marker != "" {
			parts = append(parts, marker)
		}
	}
	return strings.Join(append(parts, releaseFlags(t)...), " ")
}

// episodeMarker derives the season/episode token for a synthesized series
// title from the entry's resolved season AND the full file-list span:
//
//   - A pack labels by season: the entry's resolved season when it has one
//     (meta.SeasonKnown - the arr's own season numbering, or the season-0
//     specials bucket), else the dominant/lowest REAL season across the file
//     list (so a pack bundling S00 specials with S01 episodes labels S01,
//     never the specials bucket its first file happens to sit in), else no
//     marker (an absolute-numbered pack with no season evidence stays a bare
//     title).
//   - A single release keeps its own file marker (SxxExx, or the fansub
//     "- NN" absolute form) with its SEASON half relabeled to the resolved
//     season when the entry has one - fansub episode naming is cour-local, so
//     the file's own season half routinely disagrees with the season the arr
//     tracks the entry under - and a marker-less single file (a movie-shaped
//     OVA) gets none. The one exception is the specials bucket, where an
//     absolute marker is rewritten into a season-0 token
//     (specialsEpisodeMarker).
func episodeMarker(t *seadex.Torrent, meta EntryInfo) string {
	if !isPack(t) {
		marker := singleEpisodeMarker(t.Files)
		if special := specialsEpisodeMarker(marker, meta); special != "" {
			return special
		}
		// The resolved season outvotes the file's cour-local season half; a
		// markerless or absolute "- NN" marker under a POSITIVE resolved
		// season carries no season token, so it passes through unchanged
		// (relabelEpisodeSeason's no-token arm).
		return relabelEpisodeSeason(marker, meta)
	}
	label, _ := packSeasonLabel(t, meta)
	return label
}

// specialsEpisodeMarker rewrites a single release's ABSOLUTE marker ("- 07")
// into the specials-bucket token S00E07 when the entry resolves to season 0,
// or returns "" when the rule does not apply (no resolved season, a positive
// season, or a marker that is not the absolute form - all of which the caller
// handles unchanged).
//
// Season 0 is the one resolved season absolute numbering can never address:
// for a POSITIVE season an absolute "- NN" already names the right episode of
// the run (inventing "S02E14" would be wrong), which is why the general rule
// leaves it alone. For a Fribb-typed special the assembled title is
// {parent series title} + {marker}, so a bare "- 07" is byte-identical to what
// a REGULAR absolute episode 7 of the parent series synthesizes - the file's
// own "OVA"/"SP" text is not part of the assembled path - and an arr matching
// it grabs the wrong content into the monitored run. Emitting the specials
// token keeps the item visible and routes it at the season-0 bucket instead.
//
// The runner-up was suppressing the marker entirely (an unparseable title, so
// the item is invisible rather than mismatched); it was rejected because
// dropping a curated release off RSS is a worse outcome than landing it in the
// bucket the entry is typed into. A version suffix ("- 07v2") is dropped, as
// the episode census already does, so the emitted token stays parseable.
func specialsEpisodeMarker(marker string, meta EntryInfo) string {
	if !meta.SeasonKnown || meta.Season != 0 {
		return ""
	}
	number, ok := strings.CutPrefix(marker, "- ")
	if !ok {
		return ""
	}
	n, err := strconv.Atoi(episodeVersion.ReplaceAllString(number, ""))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%sE%02d", seasonLabel(0), n)
}

// seasonLabel renders a season number as the SNN token the arrs parse
// (the one wire format every season marker in this file must agree on).
func seasonLabel(s int) string { return fmt.Sprintf("S%02d", s) }

// packSeasonLabel resolves the season token a PACK is labeled with, in the one
// precedence both title paths share: the entry's resolved season outvotes the
// pack's file-season evidence (fansub numbering is cour-local), which in turn
// outvotes nothing at all. ok is false when neither source pins a season, so
// each caller supplies its own fallback (no marker on the assembled path, the
// file's own season half on the derived one).
func packSeasonLabel(t *seadex.Torrent, meta EntryInfo) (string, bool) {
	if meta.SeasonKnown {
		return seasonLabel(meta.Season), true
	}
	if s, ok := packSeason(t.Files); ok {
		return seasonLabel(s), true
	}
	return "", false
}

// relabelEpisodeSeason rewrites the season half of the LAST SxxExx token in a
// single-release title (or in the episode marker assembled beside a known show
// title) to the entry's resolved season - the one implementation of that
// cour-local correction both title paths share. A no-op without a resolved
// season or when the value carries no SxxExx token (an absolute "- NN" or
// marker-less name - nothing to relabel).
func relabelEpisodeSeason(value string, meta EntryInfo) string {
	if !meta.SeasonKnown {
		return value
	}
	l := lastSubmatchIndex(episodeToken, value)
	if l == nil {
		return value
	}
	return value[:l[4]] + seasonLabel(meta.Season) + value[l[5]:]
}

// singleEpisodeMarker returns a single-episode torrent's own episode token:
// the last SxxExx token of the representative file (uppercased), or the
// absolute-episode number in the fansub "- NN" form, or "" when the file
// carries neither (a movie/single-OVA file).
func singleEpisodeMarker(files []seadex.File) string {
	name := representativeFile(files)
	if name == "" {
		return ""
	}
	// Read the episode identity from the same base-then-full-path rule the
	// census uses (episodeKeyBase), so a token that lives only in a
	// directory component still names the episode instead of being lost:
	// coveredEpisodes already keys on it, so dropping it here made the
	// marker disagree with the pack/single decision that produced it.
	base := episodeKeyBase(name)
	if l := lastSubmatchIndex(episodeToken, base); l != nil {
		return strings.ToUpper(base[l[2]:l[3]])
	}
	if l := lastSubmatchIndex(absoluteEpisode, base); l != nil {
		return "- " + base[l[2]:l[3]]
	}
	return ""
}

// releaseFlags returns the real, verifiable release flags in an arr-parseable
// suffix shape: the resolution when classifiable from the torrent's own file
// names, "Dual Audio" when SeaDex's structured per-torrent flag is set, and
// the release group bracketed. Flags this app does not hold are omitted, never
// guessed - prior art proves parseable boilerplate works (seadexerr ships a
// hardcoded "{title} S01 Bluray 1080p remux"), and real values beat invented
// ones.
func releaseFlags(t *seadex.Torrent) []string {
	var flags []string
	// The resolution flag comes from the shared classify.FileResolution (the ONE
	// place a release.Input is built from SeaDex data, over the shared
	// payload.Names eligibility rule), so the RSS title's resolution and the
	// daemon finding's classification can never disagree about which files
	// vote.
	if res := classify.FileResolution(t.Files); res != "" {
		flags = append(flags, res)
	}
	if t.DualAudio {
		flags = append(flags, "Dual Audio")
	}
	if g := strings.TrimSpace(t.ReleaseGroup); g != "" {
		flags = append(flags, "["+g+"]")
	}
	return flags
}

// --- Episode/pack heuristics: token regexes, derivedTitle, packSeason ---

// episodeToken matches a season+episode token (S01E01, S1E1, S01E01-E13,
// S01E15v2), captured in group 1 with its season half in group 2. Collapsing
// its episode half to just the season turns a season pack's per-episode file
// name into a whole-season release title, so the arr grabs the pack rather
// than treating it as a single episode. The token must end at a
// non-alphanumeric boundary (underscore included - underscore-delimited
// names use "_" everywhere a space would sit) or the end of the string:
// without it, the E-less range arm swallowed a dash-joined resolution
// ("S01E07-1080p" tokenized as the bogus range "S01E07-1080", corrupting
// both the single-episode marker and the pack collapse, which left a stray
// "p" in the title). Consumers read the SUBMATCH (group 1), never the full
// match, which may include the terminator character.
var episodeToken = regexp.MustCompile(`(?i)((S\d{1,2})E\d{1,4}(?:-E?\d{1,4})?(?:v\d+)?)(?:[^0-9a-z]|$)`)

// absoluteEpisode matches an absolute episode number in the fansub "- 07" form
// (optional version suffix), with the episode number captured in group 1. The
// delimiters accept underscores as well as spaces: underscore-named releases
// ("_Show_-_01_") use "_" everywhere a space would sit, and matching only the
// space-dash form made such packs read as a single episode. Used to keep a
// multi-file pack from reading as episode 7 when there is no SxxExx token to
// collapse, and to extract a single absolute episode's number for synthesis.
var absoluteEpisode = regexp.MustCompile(`[\s_]-[\s_](\d{1,4}(?:v\d+)?)(?:[\s_]|$)`)

// episodeVersion strips a trailing vN revision from an episode token so a v2
// replacement of the same episode never counts as a second episode.
var episodeVersion = regexp.MustCompile(`(?i)v\d+$`)

// multiSpace collapses runs of whitespace left after removing a token.
var multiSpace = regexp.MustCompile(`\s{2,}`)

// lastSubmatchIndex returns the submatch index pairs of the LAST
// non-overlapping match of re in s, or nil when there is none. It replays
// FindAllStringSubmatchIndex(s, -1)'s progression but retains only the
// current match, so - for a pattern that cannot match the empty string,
// which every pattern in this file is - it is equivalent to taking that
// call's last element while allocating O(1) instead of O(matches). An
// empty-matching pattern is OUT OF CONTRACT: the guard below only keeps the
// scan terminating, and deliberately does not reproduce FindAll's
// rune-width advance or its rejection of an empty match sitting at the
// previous match's end. SeaDex bounds a page at
// 48 MiB but caps no individual string, and match slices cost ~108 bytes
// per match: 4 MiB of repeated "S1E1 " retains ~86 MiB, so one hostile
// file name can OOM the 256 MiB container during a feed rebuild (CWE-400/
// CWE-789), well inside the working-set budget internal/seadex sizes its
// byte caps against. Neither episodeToken nor absoluteEpisode is anchored
// with ^, so scanning the s[off:] suffix is equivalent, and their trailing
// (?:...|$) alternatives still see the real end of s.
func lastSubmatchIndex(re *regexp.Regexp, s string) []int {
	var last []int
	for off := 0; off <= len(s); {
		m := re.FindStringSubmatchIndex(s[off:])
		if m == nil {
			break
		}
		if last == nil {
			last = make([]int, len(m))
		}
		for i := range m {
			if m[i] < 0 {
				last[i] = -1
				continue
			}
			last[i] = m[i] + off
		}
		if m[1] == m[0] {
			// Out-of-contract empty match (no pattern here can produce one):
			// advance so the scan cannot spin. This is a termination guard, not
			// FindAll's rune-width progression.
			off = last[1] + 1
			continue
		}
		off = last[1]
	}
	return last
}

// derivedTitle is the file-name derivation with the entry's known mapping
// applied: when the entry pins a season (a positive Fribb TVDB season, or a
// Fribb-typed special's mapped season 0), the pack's collapsed season label
// (SxxExx and absolute arms alike) and a single release's LAST SxxExx season
// half are relabeled to it - the same cour-local correction episodeMarker
// applies on the assembled path, with the same precedence (the Fribb season
// beats file evidence; a SINGLE release's absolute "- NN" or marker-less name
// is never relabeled).
func derivedTitle(t *seadex.Torrent, meta EntryInfo) string {
	name := representativeFile(t.Files)
	if name == "" {
		return strings.TrimSpace(t.ReleaseGroup)
	}
	base := titleBase(name)
	if !isPack(t) {
		// A single episode, movie, or single OVA: the file name is already the
		// release title the arr should parse (do not collapse its episode) -
		// with its cour-local season half relabeled when the entry maps one.
		return strings.TrimSpace(relabelEpisodeSeason(base, meta))
	}
	if l := lastSubmatchIndex(episodeToken, base); l != nil {
		// Collapse only the LAST episode token: scene naming puts the marker
		// after the title, so a title that itself contains an SxxExx-shaped
		// substring is preserved verbatim. The season label comes from the
		// whole pack (packSeason), not this one file, so a representative
		// file from the S00 specials bucket cannot mislabel the pack. The
		// replacement spans the TOKEN group (l[2]:l[3]), never the full
		// match, whose trailing terminator character must survive the
		// collapse.
		// No resolved and no file-list season: keep the file's own season half.
		label := base[l[4]:l[5]]
		if resolved, ok := packSeasonLabel(t, meta); ok {
			label = resolved
		}
		return strings.TrimSpace(base[:l[2]] + label + base[l[3]:])
	}
	if last := lastSubmatchIndex(absoluteEpisode, base); last != nil {
		// Collapse only the LAST absolute episode token (mirroring the SxxExx
		// arm above): a title segment that is itself " - NN"-shaped (e.g.
		// "Show - 07 (WEB) - 01") must be preserved, not stripped with the
		// real episode token. The collapsed token is replaced by the pack's
		// season label under the SAME precedence the SxxExx arm uses
		// (packSeasonLabel): collapsing the episode already claims the whole
		// season, so a pack that has a resolved (or file-list) season must say
		// which one - a title carrying neither a season nor an episode is the
		// one shape the pack collapse exists to avoid. With no season from
		// either source (a plain absolute-numbered pack) the token drops and
		// the title stays a bare name, as before.
		label := " "
		if resolved, ok := packSeasonLabel(t, meta); ok {
			label = " " + resolved + " "
		}
		collapsed := base[:last[0]] + label + base[last[1]:]
		return strings.TrimSpace(multiSpace.ReplaceAllString(collapsed, " "))
	}
	return strings.TrimSpace(base)
}

// packSeason resolves the season a multi-episode pack is labeled with from the
// FULL file-list span: the dominant REAL (non-zero) season by episode-file
// count, ties broken toward the lowest - so a pack bundling S00 specials with
// an S01 season labels S01, never the specials bucket its first file happens
// to sit in. A pack whose files are all S00 returns (0, true) (a specials
// pack); ok is false when no media file carries an SxxExx token (an
// absolute-numbered pack).
func packSeason(files []seadex.File) (season int, ok bool) {
	counts := seasonCounts(files)
	if len(counts) == 0 {
		return 0, false
	}
	best, bestCount := -1, -1
	for s, c := range counts {
		if s == 0 {
			continue
		}
		if c > bestCount || (c == bestCount && s < best) {
			best, bestCount = s, c
		}
	}
	if best >= 0 {
		return best, true
	}
	return 0, true
}

// seasonCounts tallies episode files per SxxExx season across the torrent's
// media files, keying each file on its LAST token (scene naming puts the real
// marker after the title).
func seasonCounts(files []seadex.File) map[int]int {
	counts := make(map[int]int)
	// Judge the episode population, not the raw list: a bonus video far below
	// the real episodes (and any marked sample, which the type gate drops by
	// name) must not contribute a false season count. The census rule, not
	// payload's primary-payload rule: a legitimately shorter episode still
	// votes for its season, so the floor is median-anchored.
	files = payload.Population(files)
	for i := range files {
		if !isContentMediaFile(files[i].Name) {
			continue
		}
		name := stripExt(files[i].Name)
		l := lastSubmatchIndex(episodeToken, name)
		if l == nil {
			continue
		}
		s, err := strconv.Atoi(name[l[4]+1 : l[5]])
		if err != nil {
			continue
		}
		counts[s]++
	}
	return counts
}

// episodeKeyBase picks the portion of a file's name its episode identity is
// read from: the file's OWN base name when that carries episode evidence
// (an SxxExx token or an absolute "- NN" number), else the full path - so a
// pack whose only episode tokens live in a directory component still keys per
// directory. Reading the full path unconditionally let a shared directory
// token (a batch folder named "... S01E01-E12 ...") shadow every file's own
// absolute number, collapsing a whole season pack onto ONE episode key: the
// pack then read as a single episode and was served titled as episode 1.
func episodeKeyBase(name string) string {
	base := stripExt(path.Base(name))
	if hasEpisodeEvidence(base) {
		return base
	}
	return stripExt(name)
}

// hasEpisodeEvidence reports whether a path fragment carries an episode
// identity - an SxxExx token or an absolute "- NN" number. It is the ONE
// predicate the episode-evidence readers of this file share (episodeKeyBase's
// base-then-full-path rule and titleBase's headline pick), so they cannot
// disagree about which fragment names the episode.
func hasEpisodeEvidence(s string) bool {
	return episodeToken.MatchString(s) || absoluteEpisode.MatchString(s)
}

// titleBase picks the path fragment a DERIVED title headlines with: the file's
// own base name, or the nearest ancestor directory component when the base
// carries no episode evidence and that directory carries both episode evidence
// AND text of its own.
//
// The base name is the primary source because it is normally the release name.
// But a pack (or a single release) whose episode evidence lives only in a
// directory component - the top-level directory IS the release name, the files
// under it are bare "01.mkv"/"video.mkv" - would otherwise headline as "01" or
// "video": a title no arr can parse into a series, so the curated release is
// invisible on RSS with no diagnostic, while the parseable name sits one
// component up. Reading the same base-then-full-path evidence rule the episode
// census uses (episodeKeyBase) keeps the two from disagreeing about which
// fragment names the episode.
//
// The own-text requirement is what keeps a token-ONLY directory ("S01E01/Movie
// Cut A.mkv") on the base name: promoting it would headline a bare "S01" with
// no show name at all, strictly worse than the basename it replaced.
func titleBase(name string) string {
	base := stripExt(path.Base(name))
	if hasEpisodeEvidence(base) {
		return base
	}
	for dir := path.Dir(name); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		if component := path.Base(dir); hasEpisodeEvidence(component) && hasNonTokenText(component) {
			return component
		}
	}
	return base
}

// hasNonTokenText reports whether a path fragment carries letters or digits
// beyond its episode tokens - i.e. whether it could name a show at all. A
// fragment that is nothing but its token ("S01E01") is not a release name.
func hasNonTokenText(s string) bool {
	stripped := absoluteEpisode.ReplaceAllString(episodeToken.ReplaceAllString(s, " "), " ")
	for _, r := range stripped {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// isPack reports whether a torrent bundles more than one episode (a real season
// pack) rather than a single episode. SeaDex stores a complete season that was
// never packed as one torrent per episode - each a single-file release - so the
// file count is what separates a pack from a lone episode. The file list ships
// in the SeaDex record, so this needs no torrent fetch.
func isPack(t *seadex.Torrent) bool {
	return coveredEpisodes(t.Files) > 1
}

// coveredEpisodes counts the distinct episodes a torrent's files span, keying on
// the SxxExx token first and the "- NN" absolute-episode form (space- or
// underscore-delimited) as a fallback. Creditless extras (NCED/NCOP) and other
// sidecars carry neither token and are not counted, so an episode bundled with
// its creditless files still reads as a single episode.
func coveredEpisodes(files []seadex.File) int {
	seen := make(map[string]struct{})
	// Census files only, so an unmarked bonus video far below the real episodes
	// cannot inflate a lone episode into a "pack" - and a MARKED sample never
	// reaches the count at all, because payload's type gate drops it by name.
	// The floor is anchored on the pool's median, not its maximum: a pack whose
	// premiere runs double length (or that bundles the franchise movie) would
	// otherwise lose every regular episode and read as a single episode.
	files = payload.Population(files)
	for i := range files {
		if !isContentMediaFile(files[i].Name) {
			continue
		}
		base := episodeKeyBase(files[i].Name)
		if l := lastSubmatchIndex(episodeToken, base); l != nil {
			// Key on the LAST token: scene naming puts the episode marker
			// after the title, so a title containing an SxxExx-shaped
			// substring must not shadow the real episode marker.
			tok := strings.ToUpper(base[l[2]:l[3]])
			seen["e"+episodeVersion.ReplaceAllString(tok, "")] = struct{}{}
			continue
		}
		if l := lastSubmatchIndex(absoluteEpisode, base); l != nil {
			tok := base[l[2]:l[3]]
			seen["a"+episodeVersion.ReplaceAllString(tok, "")] = struct{}{}
		}
	}
	return len(seen)
}

// --- Media-file classification helpers ---

// representativeFile picks the file name a title is derived from: the first file
// carrying a season+episode token (so extras like NCED/NCOP/creditless files,
// which lack one, are skipped in favour of a real episode), or the first file
// when none match (a movie/single release).
func representativeFile(files []seadex.File) string {
	if len(files) == 0 {
		return ""
	}
	// Derive the title from the episode population, so a first-listed sample or
	// featurette can never headline the synthesized title while a legitimately
	// shorter first episode still can (a marked sample is dropped by name in
	// payload's type gate, an unmarked bonus video by the census floor).
	// payload.Population keeps the primary-payload rule's totality fallbacks
	// (type-gate-only when no lengths, size-only when no type survivor), so a
	// sidecar-only or container-only list still yields a candidate; an
	// all-unnamed list yields none.
	files = payload.Population(files)
	if len(files) == 0 {
		return ""
	}
	// Prefer a real episode file (skipping creditless extras/sidecars): first an
	// SxxExx token, then an absolute-numbered episode, so the title derives from a
	// real episode rather than an extra. The two predicates are deliberately
	// asymmetric: episodeToken matches the RAW name (its E-digit body has no trailing
	// anchor, so it matches with the extension still present), but absoluteEpisode ends
	// in (?:[\s_]|$) and an absolute number can abut the extension ("Show - 07.mkv"), so it
	// must run on stripExt(n) to match. Do not unify them onto one input - dropping
	// stripExt here breaks absolute-episode detection.
	if name := firstEpisodeFile(files, episodeToken.MatchString); name != "" {
		return name
	}
	if name := firstEpisodeFile(files, func(n string) bool {
		return absoluteEpisode.MatchString(stripExt(n))
	}); name != "" {
		return name
	}
	// No episode-marked media file: fall back to the first media file (a movie/
	// single release), then to the first file at all (a sidecar-only list).
	for i := range files {
		if isContentMediaFile(files[i].Name) {
			return files[i].Name
		}
	}
	return files[0].Name
}

// firstEpisodeFile returns the name of the first real media file (not a creditless
// extra or sidecar) whose name satisfies match, or "" when none match.
func firstEpisodeFile(files []seadex.File, match func(string) bool) string {
	for i := range files {
		if !isContentMediaFile(files[i].Name) {
			continue
		}
		if match(files[i].Name) {
			return files[i].Name
		}
	}
	return ""
}

// isContentMediaFile reports whether name is eligible to identify the release
// content, delegating to the shared type predicate in payload (one home for
// "what counts as a content file").
func isContentMediaFile(name string) bool {
	return payload.ContentMediaFile(name)
}

// stripExt drops a trailing known video extension from a file name, leaving any
// other trailing dotted token (a release name is not a path) intact.
func stripExt(name string) string {
	if !payload.IsMediaFile(name) {
		return name
	}
	return name[:len(name)-len(path.Ext(name))]
}

// --- Feed assembly utilities ---

// totalSize sums the byte lengths of a torrent's files (the pack size). The
// lengths come from the untrusted SeaDex record, so the arithmetic is
// validated: a negative length, or a sum that would overflow int64 into a
// negative value, returns 0 - the feed's existing "size unknown"
// representation - rather than rendering a negative enclosure length to the
// arrs.
func totalSize(files []seadex.File) int64 {
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

// entryURL is the SeaDex entry page for an AniList id under the canonical
// site base (seadex.DefaultBaseURL, the same constant defaultSeaDexBaseURL -
// and through it the reader's InfoURL allowlist - is derived from), or "" when
// the id is unknown - the per-item info URL (the feed <comments>), so the
// operator can see why a release is curated. The URL rule is the shared
// releases.moe contract in internal/seadex; this is a thin delegate, like
// validInfoHash.
func entryURL(alID int) string {
	return seadex.EntryURL(alID)
}

// validInfoHash returns h lowercased when it is a 40-char SHA-1 hex info hash,
// else "". SeaDex publishes the literal string "<redacted>" for AnimeBytes info
// hashes (private tracker), so this keeps a bogus value out of the feed's
// infohash attr; AB items are grabbed via their id-based download URL regardless.
// The redaction/validity knowledge is the upstream releases.moe contract and
// lives in internal/seadex (seadex.ValidInfoHash); this is a thin delegate.
func validInfoHash(h string) string {
	return seadex.ValidInfoHash(h)
}

// sortFeed orders a journal feed newest-first by first-seen time (stable, so
// items journaled in the same rebuild keep catalogue order). The persisted
// journal is deliberately bounded by AGE alone (feedJournalMaxAge,
// journal.go), never by count: growJournal marks every new identity seen
// before this runs, so evicting an item here would permanently deny it RSS
// exposure (the seen ledger can never re-admit it). Size caps apply only at
// render/serve time (applyPaging + maxItems, query.go), evicting from the
// rendered view, and maxFeedBytes bounds the persisted snapshot as a whole.
func sortFeed(items []journalItem) []journalItem {
	slices.SortStableFunc(items, func(a, b journalItem) int {
		return b.FirstSeen.Compare(a.FirstSeen)
	})
	return items
}

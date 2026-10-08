package indexer

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// defaultSeaDexBaseURL is the SeaDex site base the writer builds per-item
// info links under (entryURL) and the reader's InfoURL allowlist is derived
// from (reload.go's seadexInfoHost) - the ONE source for both ends of the
// persisted feed.json contract. It references the canonical constant in
// internal/seadex (the package that owns the releases.moe contract) so it
// cannot drift from it.
const defaultSeaDexBaseURL = seadex.DefaultBaseURL

// EntryInfo is the per-show (per-AniList-id) metadata the compare cycle hands
// the feed writer for title synthesis: the show's own title as its arr knows it
// (or the AniList canonical title as fallback; empty when neither is known),
// its release year, the season the entry maps to, whether it is a movie, and the
// TVDB id its mapping record carries.
type EntryInfo struct {
	Title string
	// SeriesTitle is the Sonarr series the entry is filed under, set only beside
	// SpecialEpisodes: the second title the twin is labeled with, since Title
	// keeps the entry's own name.
	SeriesTitle string
	// Seasons are the TVDB seasons this entry's absolute-numbered episodes fall
	// into, from the mapping list; nil when the list names none. The
	// RANGES, never a scalar: three measured entries need 6, 8 and 2 distinct
	// seasons across their packs, so one season per entry mislabels most of them.
	Seasons []SeasonRange
	// SpecialEpisodes are the TVDB season-0 episodes of SeriesTitle this offered
	// entry is, ascending, nil when the map names none; SpecialsEpisodes is the
	// AniDB episode count they answer, 0 when unknown. They let the feed serve a
	// release a second time as "<SeriesTitle> S00Exx", which Sonarr can parse.
	SpecialEpisodes  []int
	SpecialsEpisodes int
	Year             int
	// Season is the season number this entry's releases belong to, and SeasonKnown
	// reports whether one was resolved at all.
	Season int
	// TvdbID is the series id the entry's mapping record carries, 0 when it has
	// none. It feeds the rendered tvdbid attribute, which is what lets Sonarr
	// resolve a series it could not parse out of the release title.
	TvdbID      int
	SeasonKnown bool
	IsMovie     bool
}

// SeasonRange is one TVDB season an absolute-numbered run's episodes fall into:
// episodes First..Last are season Season, with Last 0 meaning open-ended. This
// package's own copy of the mapping-list fact (it imports neither mapping nor
// align, and EntryInfo is its only channel).
type SeasonRange struct {
	Season int
	First  int
	Last   int
}

// contains reports whether the whole episode span first..last sits inside the
// range. An open-ended range (Last 0) contains a span only from its First on, so
// it never claims a span that starts before it.
func (r SeasonRange) contains(first, last int) bool {
	return r.First <= first && (r.Last == 0 || last <= r.Last)
}

// EntryInfoFunc resolves the per-show (per-AniList-id) metadata the feed
// writer synthesizes RSS titles and categories from. It is total: an id the
// producer knows nothing about yields the zero EntryInfo (file-name
// fallback, anime category). The compare cycle supplies it; see
// entryInfoFunc for the nil-safe wrapper.
type EntryInfoFunc func(alID int) EntryInfo

// ref resolves one show's metadata to a fresh addressable value, for the title
// synthesizers that take it by pointer (EntryInfo outgrew the pass-by-value size).
func (f EntryInfoFunc) ref(alID int) *EntryInfo {
	info := f(alID)
	return &info
}

// entryInfoFunc normalizes a possibly-nil per-show metadata callback to a
// total function returning the zero EntryInfo (file-name fallback, anime
// category), so the journal and harvest paths never nil-check it.
func entryInfoFunc(info EntryInfoFunc) EntryInfoFunc {
	if info != nil {
		return info
	}
	return func(int) EntryInfo { return EntryInfo{} }
}

// categoriesFor maps a show's mapping type to its Torznab categories: a movie to
// Movies (Radarr) whichever arr holds it, since Sonarr is offered a film only as
// its twin (twinsFor), and everything else to Anime. The unknown case defaults to
// Anime: a single-file OVA looks like a movie by file name, and a special
// mis-routed to Radarr can never match there.
func categoriesFor(isMovie bool) []int {
	if isMovie {
		return []int{catMovies}
	}
	return []int{catAnime}
}

// synthesizeTitle builds the served release title for one curated torrent.
func synthesizeTitle(t *seadex.Torrent, meta *EntryInfo) string {
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
	return joinWithRevision(parts, t)
}

// joinWithRevision joins a synthesized title's head (the show and its season or
// episode marker) to the release flags, with the payload's revision token
// between the two when the title would otherwise read an older revision than
// the newest payload file carries: without it Sonarr reads Version 1 off a pack
// whose reissued episode is v2 and never sees the upgrade. The token is added
// only before a non-empty flag list, because Sonarr reads a trailing bracketed
// token as the release group, and only when the tokened title reads exactly
// the payload's version.
func joinWithRevision(head []string, t *seadex.Torrent) string {
	flags := releaseFlags(t)
	plain := strings.Join(slices.Concat(head, flags), " ")
	payloadRev := classify.PayloadRevision(t.Files)
	token := payloadRev.Token()
	if len(flags) == 0 || token == "" || release.ParseTitleRevision(plain).Version >= payloadRev.Version {
		return plain
	}
	tokened := strings.Join(slices.Concat(head, []string{token}, flags), " ")
	if release.ParseTitleRevision(tokened).Version != payloadRev.Version {
		return plain
	}
	return tokened
}

// episodeMarker derives the season/episode token for a synthesized series title from
// the entry's resolved season AND the full file-list span: - A pack labels by season:
// the entry's resolved season when it has one (meta.SeasonKnown - the arr's own season
// numbering, or the season-0 specials bucket), else the dominant/lowest REAL season
// across the file list (so a pack bundling S00 specials with S01 episodes labels S01,
// never the specials bucket its first file happens to sit in), else no marker (an
// absolute-numbered pack with no season evidence stays a bare title).
func episodeMarker(t *seadex.Torrent, meta *EntryInfo) string {
	// The synthesized path has no title to judge: it is BUILDING one, so the file
	// census is the only evidence there is.
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
func specialsEpisodeMarker(marker string, meta *EntryInfo) string {
	if !meta.SeasonKnown || meta.Season != 0 {
		return ""
	}
	n, ok := absoluteEpisodeNumber(marker)
	if !ok {
		return ""
	}
	return seasonLabel(0) + episodeLabel(n)
}

// seasonLabel renders a season number as the SNN token the arrs parse
// (the one wire format every season marker in this file must agree on).
func seasonLabel(s int) string { return fmt.Sprintf("S%02d", s) }

// absoluteEpisodeNumber reads the episode number out of a census
// single-episode marker in the absolute "- NN" form, stripping a version
// suffix exactly as the census keys it (payload.EpisodeVersion). ok is false for a
// marker in any other form - an SxxExx token, or the empty marker.
func absoluteEpisodeNumber(marker string) (int, bool) {
	number, found := strings.CutPrefix(marker, "- ")
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(payload.EpisodeVersion.ReplaceAllString(number, ""))
	if err != nil {
		return 0, false
	}
	return n, true
}

// episodeLabel renders an episode number as the ENN token the arrs parse -
// the episode twin of seasonLabel, so both halves of the marker wire format
// are single-homed together.
func episodeLabel(e int) string { return fmt.Sprintf("E%02d", e) }

// packSeasonLabel resolves the season token a PACK is labeled with, in the one
// precedence both title paths share: the entry's resolved season outvotes the
// pack's own file-season evidence (fansub numbering is cour-local), which
// outvotes the mapping-list's season ranges - so a token a release name already
// carries is never rewritten by a range. ok is false when no source pins a
// season, so each caller supplies its own fallback (no marker on the assembled
// path, the file's own season half on the derived one).
func packSeasonLabel(t *seadex.Torrent, meta *EntryInfo) (string, bool) {
	if meta.SeasonKnown {
		return seasonLabel(meta.Season), true
	}
	if s, ok := packSeason(t.Files); ok {
		return seasonLabel(s), true
	}
	if s, ok := rangeSeason(meta.Seasons, t.Files); ok {
		return seasonLabel(s), true
	}
	return "", false
}

// rangeSeason resolves the season a tokenless absolute-numbered pack is labeled
// with from the entry's mapping-list ranges: the pack's absolute episode span,
// then EXACTLY one range containing the whole span. Zero or several containing
// ranges (a pack straddling a season boundary, or a span an open range and a
// bounded sibling both claim) yield no season: the label is written only where
// the mapping places the WHOLE release inside one TVDB season.
func rangeSeason(ranges []SeasonRange, files []seadex.File) (int, bool) {
	if len(ranges) == 0 {
		return 0, false
	}
	first, last, ok := absoluteEpisodeSpan(files)
	if !ok {
		return 0, false
	}
	season, hits := 0, 0
	for _, r := range ranges {
		if r.contains(first, last) {
			season, hits = r.Season, hits+1
		}
	}
	if hits != 1 {
		return 0, false
	}
	return season, true
}

// absoluteEpisodeSpan reads the lowest and highest absolute "- NN" episode
// number across a pack's census population, keyed exactly as payload.DistinctEpisodes'
// absolute arm keys them (payload.EpisodeKeyBase, version suffix stripped). ok is false
// when no file carries the absolute form.
func absoluteEpisodeSpan(files []seadex.File) (first, last int, ok bool) {
	for _, f := range payload.Census(files) {
		base := payload.EpisodeKeyBase(f.Name)
		l := payload.LastSubmatchIndex(payload.AbsoluteEpisode, base)
		if l == nil {
			continue
		}
		n, err := strconv.Atoi(payload.EpisodeVersion.ReplaceAllString(base[l[2]:l[3]], ""))
		if err != nil {
			continue
		}
		if !ok || n < first {
			first = n
		}
		if !ok || n > last {
			last = n
		}
		ok = true
	}
	return first, last, ok
}

// relabelEpisodeSeason rewrites the season half of the LAST SxxExx token in a
// single-release title (or in the episode marker assembled beside a known show
// title) to the entry's resolved season - the one implementation of that
// cour-local correction both title paths share. A no-op without a resolved
// season or when the value carries no SxxExx token (an absolute "- NN" or
// marker-less name - nothing to relabel).
func relabelEpisodeSeason(value string, meta *EntryInfo) string {
	if !meta.SeasonKnown {
		return value
	}
	l := payload.LastSubmatchIndex(payload.EpisodeToken, value)
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
	// Read the episode identity from the same base-then-full-path rule the census
	// uses (payload.EpisodeKeyBase), so a token that lives only in a directory component
	// still names the episode instead of being lost.
	base := payload.EpisodeKeyBase(name)
	if l := payload.LastSubmatchIndex(payload.EpisodeToken, base); l != nil {
		return strings.ToUpper(base[l[2]:l[3]])
	}
	if l := payload.LastSubmatchIndex(payload.AbsoluteEpisode, base); l != nil {
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
	// The resolution flag comes from the shared classify.FileResolution (the ONE place
	// a release.Input is built from SeaDex data, over the shared payload.Names
	// eligibility rule), so the RSS title's resolution and the daemon finding's
	// classification can never disagree about which files vote.
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

// multiSpace collapses runs of whitespace left after removing a token.
var multiSpace = regexp.MustCompile(`\s{2,}`)

// derivedTitle is the file-name derivation with the entry's known mapping
// applied: when the entry pins a season (a positive mapped TVDB season, or a
// mapping-typed special's mapped season 0), the pack's collapsed season label
// (SxxExx and absolute arms alike) and a single release's LAST SxxExx season
// half are relabeled to it - the same cour-local correction episodeMarker
// applies on the assembled path, with the same precedence (the mapped season
// beats file evidence; a SINGLE release's absolute "- NN" or marker-less name
// is never relabeled).
func derivedTitle(t *seadex.Torrent, meta *EntryInfo) string {
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
	if l := payload.LastSubmatchIndex(payload.EpisodeToken, base); l != nil {
		// Collapse only the LAST episode token: scene naming puts the marker after the
		// title, so a title that itself contains an SxxExx-shaped substring is
		// preserved verbatim.
		label := base[l[4]:l[5]]
		if resolved, ok := packSeasonLabel(t, meta); ok {
			label = resolved
		}
		return strings.TrimSpace(base[:l[2]] + label + base[l[3]:])
	}
	if last := payload.LastSubmatchIndex(payload.AbsoluteEpisode, base); last != nil {
		// Collapse only the LAST absolute episode token (mirroring the SxxExx
		// arm above): a title segment that is itself " - NN"-shaped (e.g.
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
	// The census population, not the raw list: payload.Census is the one home
	// of that rule (the episode pool, then content media files only), so the
	// season tally and the pack verdict read the same file set by construction.
	files = payload.Census(files)
	for i := range files {
		name := payload.StripExt(files[i].Name)
		l := payload.LastSubmatchIndex(payload.EpisodeToken, name)
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

// titleBase picks the path fragment a DERIVED title headlines with: the file's
// own base name, or the nearest ancestor directory component when the base
// carries no episode evidence and that directory carries both episode evidence
// AND text of its own.
func titleBase(name string) string {
	base := payload.StripExt(path.Base(name))
	if payload.HasEpisodeEvidence(base) {
		return base
	}
	// Walk the ancestors nearest-first over ONE cleaned split, so a component is
	// never re-derived and path.Dir is never called per step.
	for _, component := range slices.Backward(strings.Split(path.Dir(name), "/")) {
		if component == "" {
			continue
		}
		if payload.HasEpisodeEvidence(component) && hasNonTokenText(component) {
			return component
		}
	}
	return base
}

// hasNonTokenText reports whether a path fragment carries letters or digits
// beyond its episode tokens - i.e. whether it could name a show at all. A
// fragment that is nothing but its token ("S01E01") is not a release name.
func hasNonTokenText(s string) bool {
	stripped := payload.AbsoluteEpisode.ReplaceAllString(payload.EpisodeToken.ReplaceAllString(s, " "), " ")
	for _, r := range stripped {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// packEvidence is the three-valued statement a torrent's FILE LIST makes about
// how many episodes it contains. It exists because a zero episode count is NOT
// proof of a single episode: it equally covers an absent file list and file
// naming outside the two recognized token forms. Any policy that acts on "the
// files say this is not a pack" must be able to tell positive single-episode
// evidence from no evidence at all, which a bool cannot express.
type packEvidence int

const (
	// packEvidenceUnknown is zero recognized episode tokens: absent files, or
	// naming outside the recognized forms. It proves NOTHING. It is the zero
	// value, so an absent census reads as no evidence rather than as a verdict.
	packEvidenceUnknown packEvidence = iota
	// packEvidenceSingle is EXACTLY one distinct recognized token over a
	// non-empty content population - positive proof of one episode.
	packEvidenceSingle
	// packEvidencePack is more than one distinct recognized episode token.
	packEvidencePack
)

// packEvidenceOf grades what a torrent's file list proves about its episode
// count. The distinct-token count is read from payload.Census, the SAME
// population coveredEpisodes counts, so one recognized token implies a
// non-empty population.
func packEvidenceOf(t *seadex.Torrent) packEvidence {
	switch n := payload.DistinctEpisodes(payload.Census(t.Files)); {
	case n > 1:
		return packEvidencePack
	case n == 1:
		return packEvidenceSingle
	default:
		return packEvidenceUnknown
	}
}

// isPack reports whether a torrent bundles more than one episode (a real season
// pack) rather than a single episode. SeaDex stores a complete season that was
// never packed as one torrent per episode - each a single-file release - so the
// file count is what separates a pack from a lone episode. The file list ships
// in the SeaDex record, so this needs no torrent fetch.
func isPack(t *seadex.Torrent) bool {
	return packEvidenceOf(t) == packEvidencePack
}

// seasonOnlyTitle matches what Sonarr calls a "season only release"
// (Sonarr/src/NzbDrone.Core/Parser/Parser.cs:325), with the anime bracketed variant
// (Parser.cs:113) folded in as the optional "[" or "(" before the season word: a title,
// a separator, a season word (Season / Saison / Series / Stagione / S), and the season
// number.
var seasonOnlyTitle = regexp.MustCompile(`(?i)^.+?[-_. ]+[\[(]?((?:Season|Saison|Series|Stagione|S)[-_. ]?(\d{1,2}))`)

// seasonPackDisqualifier matches the tokens that cancel a season-pack reading
// even when the season-only shape matched: EXTRAS and SUBPACK are Sonarr's own
// extras group in the season-only regex (Parser.cs:325 - they set IsSeasonExtra,
// which Sonarr filters out rather than grabbing as a season), and a special
// marker cancels FullSeason outright (Parser.cs:753-755). The vocabulary is
// deliberately minimal; a match only ever moves the verdict to UNKNOWN, which
// falls back to the file census, so a token this list misses costs nothing that
// the census does not already answer.
var seasonPackDisqualifier = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:EXTRAS|SUBPACK|SPECIALS?|OVA|ONA)(?:[^\p{L}\p{N}]|$)`)

// sonarrSimpleNoise mirrors the quality tokens Sonarr DELETES from a release name
// before its season/episode patterns ever run (Parser.cs SimpleTitleRegex, applied in
// ParseTitle to build simpleTitle): the resolution, the codec, DD5.1, the WxH
// dimensions and the bit-depth markers. It belongs beside seasonOnlyTitle and
// seasonPackDisqualifier for the same reason they keep their own (?i) and delimiter
// classes - the vocabulary being mirrored is .NET's, and the job is to answer what
// SONARR will make of a title, not what this app believes a name says.
var sonarrSimpleNoise = regexp.MustCompile(
	`(?i)(?:(?:480|540|576|720|1080|2160)[ip]|[xh][\W_]?26[45]|DD\W?5\W1` +
		`|848x480|1280x720|1920x1080|3840x2160|4096x2160|10-bit)\s*`,
)

// packFromTitle reports the season-pack verdict a release TITLE carries, and
// whether the title answered at all. It is the title half of the harvest's
// title-vs-census cross-check (journal.go's titleAudit.served); the file
// census (isPack) is the other half and the fallback.
func packFromTitle(title string) (pack, known bool) {
	s := strings.TrimSpace(title)
	if s == "" {
		return false, false
	}
	if payload.HasEpisodeEvidence(s) {
		return false, true
	}
	if seasonPackDisqualifier.MatchString(s) {
		return false, false
	}
	m := seasonOnlyTitle.FindStringSubmatchIndex(s)
	if m == nil {
		return false, false
	}
	// Sonarr applies its lookahead to the CLEANED title (SimpleTitleRegex runs before
	// ReportTitleRegex), so strip the same quality tokens from the tail before reading
	// it. Only the tail is normalized, never the string the offsets index, so
	// correctSeasonOnlyTitle's own match on the raw title still lines up.
	if !seasonNumberEnds(sonarrSimpleNoise.ReplaceAllString(s[m[3]:], "")) {
		return false, false
	}
	return true, true
}

// seasonNumberEnds applies the two boundary conditions Sonarr's season-only
// regex expresses in its tail, against the text following the season number:
// the number must end at a non-alphanumeric boundary or the end of the string
// (Sonarr's "(?:[-_. ]|$)+", widened to the closing bracket the anime variant
// needs), and it must NOT be followed by another number, optionally separated
// (Sonarr's "(?![-_. ]?\d+)" negative lookahead). The lookahead is the
// load-bearing half: without it "Show - S01 05" reads as a whole season when it
// names episode 5 of it.
func seasonNumberEnds(rest string) bool {
	if rest == "" {
		return true
	}
	r, size := utf8.DecodeRuneInString(rest)
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return false
	}
	if strings.ContainsRune("-_. ", r) {
		rest = rest[size:]
	}
	next, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsDigit(next)
}

// correctSeasonOnlyTitle rewrites the season-only token inside a harvested
// tracker title into the season+episode form the file census's own marker names,
// returning the corrected title and whether the rewrite applied.
func correctSeasonOnlyTitle(title, marker string) (string, bool) {
	m := seasonOnlyTitle.FindStringSubmatchIndex(title)
	if m == nil {
		return title, false
	}
	season, err := strconv.Atoi(title[m[4]:m[5]])
	if err != nil {
		return title, false
	}
	episode, ok := episodeSuffix(marker)
	if !ok {
		return title, false
	}
	// Group 1 is the whole season token; splicing over it leaves the optional
	// bracket that precedes it (and everything after the number) in place.
	return title[:m[2]] + seasonLabel(season) + episode + title[m[3]:], true
}

// episodeSuffix renders the E-half a corrected season token carries, from the
// census's own single-episode marker: an SxxExx marker's episode text verbatim
// (so a range token "S01E01-E13" keeps its range rather than naming one
// episode of it), or the absolute "- NN" form as E%02d. A version suffix is
// stripped exactly as the census keys it (payload.EpisodeVersion), so a "v2" marker
// cannot emit an unparseable token. ok is false for a marker in neither form -
// including the empty marker, which packEvidenceSingle cannot produce but the
// caller must still handle rather than assume away.
func episodeSuffix(marker string) (string, bool) {
	if l := payload.LastSubmatchIndex(payload.EpisodeToken, marker); l != nil {
		// Group 1 is the whole token and group 2 its season half, so the text
		// between the season half's end and the token's end is the episode half.
		return payload.EpisodeVersion.ReplaceAllString(strings.ToUpper(marker[l[5]:l[3]]), ""), true
	}
	n, ok := absoluteEpisodeNumber(marker)
	if !ok {
		return "", false
	}
	return episodeLabel(n), true
}

// representativeFile picks the file name a title is derived from: the first file
// carrying a season+episode token (so extras like NCED/NCOP/creditless files,
// which lack one, are skipped in favour of a real episode), or the first file
// when none match (a movie/single release).
func representativeFile(files []seadex.File) string {
	// Derive the title from the episode population, so a first-listed sample or
	// featurette can never headline the synthesized title while a legitimately
	// shorter first episode still can (a marked sample is dropped by name in
	// payload's type gate, an unmarked bonus video by the census floor).
	files = payload.Population(files)
	if len(files) == 0 {
		return ""
	}
	// Prefer a real episode file (skipping creditless extras/sidecars): first an SxxExx
	// token, then an absolute-numbered episode, so the title derives from a real
	// episode rather than an extra.
	if name := firstEpisodeFile(files, payload.EpisodeToken.MatchString); name != "" {
		return name
	}
	if name := firstEpisodeFile(files, func(n string) bool {
		return payload.AbsoluteEpisode.MatchString(payload.StripExt(n))
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
func validInfoHash(h string) string {
	return seadex.ValidInfoHash(h)
}

// sortFeed orders a journal feed newest-first by first-seen time (stable, so
// items journaled in the same rebuild keep catalogue order). The persisted
// journal is deliberately bounded by AGE alone (feedJournalMaxAge,
// journal.go), never by count: growJournal marks every new identity seen
// before this runs, so evicting an item here would permanently deny it RSS
// exposure (the publication log can never re-admit it). Size caps apply only at
// render/serve time (applyPaging + maxItems, query.go), evicting from the
// rendered view, and maxFeedBytes bounds the persisted snapshot as a whole.
func sortFeed(items []journalItem) []journalItem {
	slices.SortStableFunc(items, func(a, b journalItem) int {
		return b.FirstSeen.Compare(a.FirstSeen)
	})
	return items
}

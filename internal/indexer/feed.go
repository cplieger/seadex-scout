package indexer

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// feedWindow caps each per-tracker synthesized RSS feed. It is the secondary
// bound under the journal's feedJournalMaxAge prune (see journal.go): the
// journal rarely approaches it, but a burst of new curation can never bloat
// the rendered XML or the persisted snapshot past this many items per tracker.
const feedWindow = 300

// seaDexEntryURL is the SeaDex entry page base; the per-item info URL (the feed
// <comments>) is this plus the AniList id, so the operator can see why a release
// is curated.
const seaDexEntryURL = "https://releases.moe/"

// EntryInfo is the per-show (per-AniList-id) metadata the compare cycle hands
// the feed writer for title synthesis: the show's own title as its arr knows it
// (or the AniList canonical title as fallback; empty when neither is known),
// its release year, the Fribb TVDB season the entry maps to (0 = unmapped or
// specials), and the Fribb media typing. The cycle builds it from persisted
// state only (the mapping index, the last library snapshot, the AniList memo),
// so the feed rebuild stays arr-independent. The zero value is valid: synthesis
// then falls back to file-name derivation and the anime category.
type EntryInfo struct {
	Title      string
	Year       int
	SeasonTvdb int
	IsMovie    bool
	IsSpecial  bool
}

// entryInfoFunc normalizes a possibly-nil per-show metadata callback to a
// total function returning the zero EntryInfo (file-name fallback, anime
// category), so the journal and harvest paths never nil-check it.
func entryInfoFunc(info func(alID int) EntryInfo) func(alID int) EntryInfo {
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

// synthesizeTitle builds the served release title for one curated torrent.
//
// With a known show title (meta.Title, the arr's own title or the AniList
// canonical title) the title is assembled, not derived: the show title, a
// season/episode marker computed from the Fribb season and the full file-list
// span (see episodeMarker), and the real release flags this app actually holds
// (see releaseFlags) - so the arr parses back a title built from its own
// vocabulary. A movie is "{Title} ({Year})" instead of a marker. Without a
// show title the file-name derivation (feedTitle) is the permanent last
// resort.
func synthesizeTitle(t *seadex.Torrent, meta EntryInfo) string {
	title := strings.TrimSpace(meta.Title)
	if title == "" {
		return feedTitle(t)
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
// title from the Fribb season AND the full file-list span:
//
//   - A pack labels by season: the Fribb TVDB season when the entry maps one
//     (the arr's own season numbering), else the dominant/lowest REAL season
//     across the file list (so a pack bundling S00 specials with S01 episodes
//     labels S01, never the specials bucket its first file happens to sit in),
//     else S00 for a Fribb-typed special, else no marker (an absolute-numbered
//     pack with no season evidence stays a bare title).
//   - A single release keeps its own file marker verbatim (SxxExx, or the
//     fansub "- NN" absolute form) - today's proven arr-parseable shape - and a
//     marker-less single file (a movie-shaped OVA) gets none.
func episodeMarker(t *seadex.Torrent, meta EntryInfo) string {
	if !isPack(t) {
		return singleEpisodeMarker(t.Files)
	}
	if meta.SeasonTvdb > 0 {
		return fmt.Sprintf("S%02d", meta.SeasonTvdb)
	}
	if s, ok := packSeason(t.Files); ok {
		return fmt.Sprintf("S%02d", s)
	}
	if meta.IsSpecial {
		return "S00"
	}
	return ""
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
	base := stripExt(path.Base(name))
	if toks := episodeToken.FindAllString(base, -1); len(toks) > 0 {
		return strings.ToUpper(toks[len(toks)-1])
	}
	if m := absoluteEpisode.FindAllStringSubmatch(base, -1); len(m) > 0 {
		return "- " + m[len(m)-1][1]
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
	if res := fileResolution(t.Files); res != "" {
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

// fileResolution classifies a torrent's resolution from its media file names
// alone, via the shared release classifier. The entry notes are deliberately
// excluded: they are entry-wide and routinely describe sibling releases, so a
// note mentioning another release's 1080p must not stamp this torrent's title.
func fileResolution(files []seadex.File) string {
	names := make([]string, 0, len(files))
	for i := range files {
		if isMediaFile(files[i].Name) {
			names = append(names, files[i].Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return release.Classify(&release.Input{Names: names}).Resolution
}

// episodeToken matches a season+episode token (S01E01, S1E1, S01E01-E13,
// S01E15v2). Collapsing its episode half to just the season turns a season
// pack's per-episode file name into a whole-season release title, so the arr
// grabs the pack rather than treating it as a single episode.
var episodeToken = regexp.MustCompile(`(?i)(S\d{1,2})E\d{1,4}(?:-E?\d{1,4})?(?:v\d+)?`)

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

// creditlessExtra matches bonus OP/ED files that may carry absolute-looking
// numbers but should not count as episode files or drive the synthesized title.
var creditlessExtra = regexp.MustCompile(`(?i)\b(?:NCOP|NCED|creditless)\d*(?:v\d+)?\b`)

// multiSpace collapses runs of whitespace left after removing a token.
var multiSpace = regexp.MustCompile(`\s{2,}`)

// feedTitle synthesizes an arr-parseable release title from a torrent's file
// names - the permanent last resort when no show title is known (see
// synthesizeTitle), since SeaDex stores file names, not clean titles.
// A real season pack (files spanning more than one episode) collapses the
// episode marker to the pack's season (see packSeason: the dominant/lowest
// real season across the WHOLE file list, so a pack bundling S00 specials
// with S01 episodes labels S01 even when its first file is a special); a
// single-episode torrent keeps its SxxExx so the arr grabs it as that
// episode. This distinction matters because SeaDex tracks a complete-but-unpacked
// season as one torrent PER episode: collapsing those would mislabel, say, 24
// episodes as 24 copies of the season. A movie / single OVA (no episode marker)
// is used verbatim, and with no files the title falls back to the release group.
// The feed deliberately does NOT filter packs vs episodes - it lists both and
// lets Sonarr's FullSeason preference + already-grabbed dedupe pick (see the
// indexer package doc); this function only has to LABEL each release correctly.
func feedTitle(t *seadex.Torrent) string {
	name := representativeFile(t.Files)
	if name == "" {
		return strings.TrimSpace(t.ReleaseGroup)
	}
	base := stripExt(path.Base(name))
	if !isPack(t) {
		// A single episode, movie, or single OVA: the file name is already the
		// release title the arr should parse (do not collapse its episode).
		return strings.TrimSpace(base)
	}
	if episodeToken.MatchString(base) {
		// Collapse only the LAST episode token: scene naming puts the marker
		// after the title, so a title that itself contains an SxxExx-shaped
		// substring is preserved verbatim. The season label comes from the
		// whole pack (packSeason), not this one file, so a representative
		// file from the S00 specials bucket cannot mislabel the pack.
		locs := episodeToken.FindAllStringSubmatchIndex(base, -1)
		l := locs[len(locs)-1]
		label := base[l[2]:l[3]]
		if s, ok := packSeason(t.Files); ok {
			label = fmt.Sprintf("S%02d", s)
		}
		return strings.TrimSpace(base[:l[0]] + label + base[l[1]:])
	}
	if absoluteEpisode.MatchString(base) {
		return strings.TrimSpace(multiSpace.ReplaceAllString(absoluteEpisode.ReplaceAllString(base, " "), " "))
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
	for i := range files {
		if !isContentMediaFile(files[i].Name) {
			continue
		}
		toks := episodeToken.FindAllStringSubmatch(stripExt(files[i].Name), -1)
		if len(toks) == 0 {
			continue
		}
		s, err := strconv.Atoi(toks[len(toks)-1][1][1:])
		if err != nil {
			continue
		}
		counts[s]++
	}
	return counts
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
	for i := range files {
		if !isContentMediaFile(files[i].Name) {
			continue
		}
		base := stripExt(files[i].Name)
		switch {
		case episodeToken.MatchString(base):
			// Key on the LAST token: scene naming puts the episode marker
			// after the title, so a title containing an SxxExx-shaped
			// substring must not shadow the real episode marker.
			all := episodeToken.FindAllString(base, -1)
			tok := strings.ToUpper(all[len(all)-1])
			seen["e"+episodeVersion.ReplaceAllString(tok, "")] = struct{}{}
		case absoluteEpisode.MatchString(base):
			all := absoluteEpisode.FindAllStringSubmatch(base, -1)
			tok := all[len(all)-1][1]
			seen["a"+episodeVersion.ReplaceAllString(tok, "")] = struct{}{}
		}
	}
	return len(seen)
}

// representativeFile picks the file name a title is derived from: the first file
// carrying a season+episode token (so extras like NCED/NCOP/creditless files,
// which lack one, are skipped in favour of a real episode), or the first file
// when none match (a movie/single release).
func representativeFile(files []seadex.File) string {
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
// content: it must be a video file and not a creditless extra.
func isContentMediaFile(name string) bool {
	return isMediaFile(name) && !creditlessExtra.MatchString(name)
}

// isMediaFile reports whether a file name carries a known video container
// extension, so title synthesis derives from a real episode/movie file rather
// than a sidecar (subtitles, fonts) that happens to carry the episode token.
func isMediaFile(name string) bool {
	return mediaExts[strings.ToLower(path.Ext(name))]
}

// mediaExts are the video container extensions used to tell an episode/movie
// file from a sidecar file (subtitles, samples) when scanning a torrent's files.
var mediaExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m2ts": true,
	".ts": true, ".ogm": true, ".mov": true, ".wmv": true, ".webm": true,
}

// stripExt drops a trailing known video extension from a file name, leaving any
// other trailing dotted token (a release name is not a path) intact.
func stripExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if mediaExts[ext] {
		return name[:len(name)-len(ext)]
	}
	return name
}

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

// entryURL is the SeaDex entry page for an AniList id, or "" when unknown.
func entryURL(alID int) string {
	if alID <= 0 {
		return ""
	}
	return seaDexEntryURL + strconv.Itoa(alID)
}

// validInfoHash returns h lowercased when it is a 40-char SHA-1 hex info hash,
// else "". SeaDex publishes the literal string "<redacted>" for AnimeBytes info
// hashes (private tracker), so this keeps a bogus value out of the feed's
// infohash attr; AB items are grabbed via their id-based download URL regardless.
func validInfoHash(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) != 40 {
		return ""
	}
	for i := range len(h) {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return h
}

// sortAndCap orders a journal feed newest-first by first-seen time (stable, so
// items journaled in the same rebuild keep catalogue order) and trims it to
// feedWindow, the secondary bound under the 14-day journal prune.
func sortAndCap(items []item) []item {
	slices.SortStableFunc(items, func(a, b item) int {
		return b.FirstSeen.Compare(a.FirstSeen)
	})
	if len(items) > feedWindow {
		items = items[:feedWindow]
	}
	return items
}

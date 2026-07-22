package indexer

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/classify"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// defaultSeaDexBaseURL is the fallback SeaDex site base for a FeedWriter
// constructed without one (tests, alternate wiring); production passes
// config.DefaultSeaDexBaseURL through FeedWriterConfig.SeaDexBaseURL. It
// references the canonical constant in internal/seadex (the package that owns
// the releases.moe contract) so the fallback cannot drift from it.
const defaultSeaDexBaseURL = seadex.DefaultBaseURL

// --- Per-show metadata and categories ---

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

// --- Assembled title synthesis (known show title) ---

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
//   - A single release keeps its own file marker (SxxExx, or the fansub
//     "- NN" absolute form) with its SEASON half relabeled to the Fribb TVDB
//     season when the entry maps one (see relabelSeason) - fansub episode
//     naming is cour-local, so the file's own season half routinely
//     disagrees with the season the arr tracks the entry under - and a
//     marker-less single file (a movie-shaped OVA) gets none.
func episodeMarker(t *seadex.Torrent, meta EntryInfo) string {
	if !isPack(t) {
		marker := singleEpisodeMarker(t.Files)
		if meta.IsSpecial && seasonPrefix.MatchString(marker) {
			// A Fribb-typed special's SeasonTvdb of 0 is a MAPPED season
			// zero, not an unknown mapping (IsSpecial is the discriminator
			// the pack arm below already uses), so a single-file special's
			// cour-local SxxExx half relabels to S00 - relabelSeason alone
			// would read the 0 as unmapped and keep the file's own season,
			// pointing the arr at the parent series. Markerless and
			// absolute-numbered specials pass through: only an SxxExx
			// season prefix is rewritten.
			return seasonPrefix.ReplaceAllString(marker, seasonLabel(0))
		}
		return relabelSeason(marker, meta.SeasonTvdb)
	}
	if meta.SeasonTvdb > 0 {
		return seasonLabel(meta.SeasonTvdb)
	}
	if s, ok := packSeason(t.Files); ok {
		return seasonLabel(s)
	}
	if meta.IsSpecial {
		return "S00"
	}
	return ""
}

// seasonPrefix matches the season half of an SxxExx marker for relabeling.
var seasonPrefix = regexp.MustCompile(`(?i)^S\d{1,2}`)

// seasonLabel renders a season number as the SNN token the arrs parse
// (the one wire format every season marker in this file must agree on).
func seasonLabel(s int) string { return fmt.Sprintf("S%02d", s) }

// relabelSeason rewrites the season half of a single release's SxxExx marker
// to the Fribb TVDB season, mirroring the pack arm's correction: fansub
// episode naming is cour-local (a second cour restarts at S01E01 under its
// own AniList entry), so a file's own season half routinely names a season
// the arr does not track this entry under - and a synthesized
// "{series} S01E07" would point the arr at a DIFFERENT episode of the parent
// series. The Fribb season is the arr's own numbering, so it wins; the
// episode number is kept as-is (Fribb maps seasons, not episode offsets -
// the same approximation the pack arm already accepts). An absolute "- NN"
// marker (series-scoped, nothing to relabel), an empty marker, and an
// unmapped entry (seasonTvdb <= 0) pass through unchanged.
func relabelSeason(marker string, seasonTvdb int) string {
	if seasonTvdb <= 0 {
		return marker
	}
	return seasonPrefix.ReplaceAllString(marker, seasonLabel(seasonTvdb))
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
	if toks := episodeToken.FindAllStringSubmatch(base, -1); len(toks) > 0 {
		return strings.ToUpper(toks[len(toks)-1][1])
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

// fileResolution classifies a torrent's resolution from its file names
// alone, via the shared classify.FileResolution (the ONE place a
// release.Input is built from SeaDex data, over the shared PayloadNames
// eligibility rule), so the RSS title's resolution flag and the daemon
// finding's classification can never disagree about which files vote (h-f3).
func fileResolution(files []seadex.File) string {
	return classify.FileResolution(files)
}

// --- Episode/pack heuristics: token regexes, feedTitle, packSeason ---

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
		// file from the S00 specials bucket cannot mislabel the pack. The
		// replacement spans the TOKEN group (l[2]:l[3]), never the full
		// match, whose trailing terminator character must survive the
		// collapse.
		locs := episodeToken.FindAllStringSubmatchIndex(base, -1)
		l := locs[len(locs)-1]
		label := base[l[4]:l[5]]
		if s, ok := packSeason(t.Files); ok {
			label = seasonLabel(s)
		}
		return strings.TrimSpace(base[:l[2]] + label + base[l[3]:])
	}
	if locs := absoluteEpisode.FindAllStringIndex(base, -1); len(locs) > 0 {
		// Collapse only the LAST absolute episode token (mirroring the SxxExx
		// arm above): a title segment that is itself " - NN"-shaped (e.g.
		// "Show - 07 (WEB) - 01") must be preserved, not stripped with the
		// real episode token.
		last := locs[len(locs)-1]
		collapsed := base[:last[0]] + " " + base[last[1]:]
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
	for i := range files {
		if !isContentMediaFile(files[i].Name) {
			continue
		}
		toks := episodeToken.FindAllStringSubmatch(stripExt(files[i].Name), -1)
		if len(toks) == 0 {
			continue
		}
		s, err := strconv.Atoi(toks[len(toks)-1][2][1:])
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
			all := episodeToken.FindAllStringSubmatch(base, -1)
			tok := strings.ToUpper(all[len(all)-1][1])
			seen["e"+episodeVersion.ReplaceAllString(tok, "")] = struct{}{}
		case absoluteEpisode.MatchString(base):
			all := absoluteEpisode.FindAllStringSubmatch(base, -1)
			tok := all[len(all)-1][1]
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
// content, delegating to the shared type predicate in classify (one home for
// "what counts as a content file", h-f3).
func isContentMediaFile(name string) bool {
	return classify.ContentMediaFile(name)
}

// stripExt drops a trailing known video extension from a file name, leaving any
// other trailing dotted token (a release name is not a path) intact.
func stripExt(name string) string {
	ext := path.Ext(name)
	if classify.IsMediaFile(name) && ext != "" {
		return name[:len(name)-len(ext)]
	}
	return name
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

// entryURL is the SeaDex entry page for an AniList id under the writer's
// configured site base, or "" when the id is unknown - the per-item info URL
// (the feed <comments>), so the operator can see why a release is curated.
// The URL rule is the shared releases.moe contract in internal/seadex; this
// is a thin delegate, like validInfoHash.
func (w *FeedWriter) entryURL(alID int) string {
	return seadex.EntryURL(w.seadexBaseURL, alID)
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

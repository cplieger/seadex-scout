package indexer

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/seadex"
)

// feedWindow caps each per-tracker synthesized RSS feed. A periodic RSS check
// only needs the most recent releases; SeaDex tracks a few thousand torrents, so
// the feed is trimmed to the most-recently-updated entries (sorted desc) to keep
// the rendered XML small and the arr's RSS scan quick.
const feedWindow = 300

// seaDexEntryURL is the SeaDex entry page base; the per-item info URL (the feed
// <comments>) is this plus the AniList id, so the operator can see why a release
// is curated.
const seaDexEntryURL = "https://releases.moe/"

// buildFeeds synthesizes the two per-tracker RSS feeds from the SeaDex catalogue.
//
// Unlike a search (which proxies Prowlarr's real tracker parse), the periodic
// RSS check has no query to match, so the feed must BE the SeaDex list: one item
// per curated torrent, addressed to its tracker's feed. Each item's download
// link is built directly - public Nyaa needs no credential; AnimeBytes embeds
// the operator's passkey - because there is no Prowlarr round-trip here.
//
// A torrent is included only when a grabbable link can be formed: a Nyaa/AB URL
// with a parseable id, and (for AB) a configured passkey. An AB release skipped
// solely for a missing passkey is counted in abSkippedNoPasskey so the caller
// can nudge the operator once, rather than emitting link-less items an arr would
// fail to grab. Trackers other than Nyaa/AB (a negligible SeaDex tail) are
// dropped. Both feeds are sorted newest-first and capped at feedWindow.
//
// classify sets each item's Torznab category from the entry's AniList id: a
// SeaDex file name cannot reliably tell a movie from a single-file OVA/special,
// so the caller resolves the real media type (Fribb/AniList) and returns the
// category (Movies for a film -> Radarr, Anime for everything else -> Sonarr).
// It is called once per entry (all of an entry's torrents share its category).
func buildFeeds(entries []seadex.Entry, abPasskey string, classify func(alID int) []int) (nyaaFeed, abFeed []item, abSkippedNoPasskey int) {
	for i := range entries {
		e := &entries[i]
		cats := classify(e.AniListID)
		for j := range e.Torrents {
			it, scope, ok, noPasskey := feedItemFor(e, &e.Torrents[j], abPasskey)
			if noPasskey {
				abSkippedNoPasskey++
			}
			if !ok {
				continue
			}
			it.Categories = cats
			switch scope {
			case upstreamNyaa:
				nyaaFeed = append(nyaaFeed, it)
			case upstreamAB:
				abFeed = append(abFeed, it)
			}
		}
	}
	return sortAndCap(nyaaFeed), sortAndCap(abFeed), abSkippedNoPasskey
}

// feedItemFor resolves one SeaDex torrent into a feed item and the scope it
// belongs to. ok is false when the torrent is not a Nyaa/AB release or has no
// grabbable link; noPasskey flags the specific case of an AB release that only
// lacks the passkey (a parseable id, no configured passkey), so the caller can
// nudge the operator once rather than emitting link-less items.
func feedItemFor(e *seadex.Entry, t *seadex.Torrent, abPasskey string) (it item, scope string, ok, noPasskey bool) {
	scope = trackerScope(t.Tracker)
	if scope == "" {
		return item{}, "", false, false
	}
	dl, resolved := downloadURL(t.Tracker, t.URL, abPasskey)
	if !resolved {
		return item{}, scope, false, scope == upstreamAB && abPasskey == "" && animeBytesID(t.URL) != ""
	}
	return synthItem(e, t, dl), scope, true, false
}

// synthItem builds one feed Item for a SeaDex torrent with an already-resolved
// download link. The title is derived from the file names, the size summed from
// them, the info URL points at the SeaDex entry page, and the SeaDex marker is
// stamped (best -> 0.75 Freeleech25, alt -> 0.25 Freeleech75). The GUID is the
// tracker page URL (unique per torrent). Seeders are left 0 (the render floors
// to 1) since a synthesized item has no live swarm count. The category is left
// unset here and stamped by buildFeeds from the entry's resolved media type.
func synthItem(e *seadex.Entry, t *seadex.Torrent, dl string) item {
	dvf := dvfAlt
	if t.IsBest {
		dvf = dvfBest
	}
	return item{
		Title:                feedTitle(t),
		GUID:                 t.UsableURL(),
		InfoURL:              entryURL(e.AniListID),
		DownloadURL:          dl,
		InfoHash:             validInfoHash(t.InfoHash),
		DownloadVolumeFactor: dvf,
		Size:                 totalSize(t.Files),
		PubDate:              e.Updated,
	}
}

// episodeToken matches a season+episode token (S01E01, S1E1, S01E01-E13,
// S01E15v2). Collapsing its episode half to just the season turns a season
// pack's per-episode file name into a whole-season release title, so the arr
// grabs the pack rather than treating it as a single episode.
var episodeToken = regexp.MustCompile(`(?i)(S\d{1,2})E\d{1,4}(?:-E?\d{1,4})?(?:v\d+)?`)

// absoluteEpisode matches an absolute episode number in the " - 07" fansub form
// (optional version suffix), used only to keep a multi-file pack from reading as
// episode 7 when there is no SxxExx token to collapse.
var absoluteEpisode = regexp.MustCompile(`\s-\s\d{1,4}(?:v\d+)?(?:\s|$)`)

// creditlessExtra matches bonus OP/ED files that may carry absolute-looking
// numbers but should not count as episode files or drive the synthesized title.
var creditlessExtra = regexp.MustCompile(`(?i)\b(?:NCOP|NCED|creditless)\b`)

// multiSpace collapses runs of whitespace left after removing a token.
var multiSpace = regexp.MustCompile(`\s{2,}`)

// feedTitle synthesizes an arr-parseable release title from a torrent's file
// names - the core RSS gap, since SeaDex stores file names, not clean titles.
// A real season pack (files spanning more than one episode) collapses the
// episode marker to the season (S01E07 -> S01) so the arr grabs it as a whole
// season; a single-episode torrent keeps its SxxExx so the arr grabs it as that
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
	base := stripExt(name)
	if !isPack(t) {
		// A single episode, movie, or single OVA: the file name is already the
		// release title the arr should parse (do not collapse its episode).
		return strings.TrimSpace(base)
	}
	if episodeToken.MatchString(base) {
		return strings.TrimSpace(episodeToken.ReplaceAllString(base, "${1}"))
	}
	if absoluteEpisode.MatchString(base) {
		return strings.TrimSpace(multiSpace.ReplaceAllString(absoluteEpisode.ReplaceAllString(base, " "), " "))
	}
	return strings.TrimSpace(base)
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
// the SxxExx token first and the " - NN" absolute-episode form as a fallback.
// Creditless extras (NCED/NCOP) and other sidecars carry neither token and are
// not counted, so an episode bundled with its creditless files still reads as a
// single episode.
func coveredEpisodes(files []seadex.File) int {
	seen := make(map[string]struct{})
	for i := range files {
		if creditlessExtra.MatchString(files[i].Name) {
			continue
		}
		switch {
		case episodeToken.MatchString(files[i].Name):
			seen["e"+strings.ToUpper(episodeToken.FindString(files[i].Name))] = struct{}{}
		case absoluteEpisode.MatchString(files[i].Name):
			seen["a"+strings.TrimSpace(absoluteEpisode.FindString(files[i].Name))] = struct{}{}
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
	for i := range files {
		if creditlessExtra.MatchString(files[i].Name) {
			continue
		}
		if episodeToken.MatchString(files[i].Name) {
			return files[i].Name
		}
	}
	// No SxxExx file: prefer an absolute-numbered episode ("[Grp] Show - 07")
	// over a leading creditless/extra (NCED/NCOP) file, so an absolute-numbered
	// pack derives its title from a real episode rather than an extra.
	for i := range files {
		if creditlessExtra.MatchString(files[i].Name) {
			continue
		}
		if absoluteEpisode.MatchString(files[i].Name) {
			return files[i].Name
		}
	}
	return files[0].Name
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

// totalSize sums the byte lengths of a torrent's files (the pack size).
func totalSize(files []seadex.File) int64 {
	var n int64
	for i := range files {
		n += files[i].Length
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

// sortAndCap orders a feed newest-first (by SeaDex entry update time) and trims
// it to feedWindow.
func sortAndCap(items []item) []item {
	slices.SortStableFunc(items, func(a, b item) int {
		return b.PubDate.Compare(a.PubDate)
	})
	if len(items) > feedWindow {
		items = items[:feedWindow]
	}
	return items
}

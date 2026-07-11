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
func buildFeeds(entries []seadex.Entry, abPasskey string) (nyaaFeed, abFeed []Item, abSkippedNoPasskey int) {
	for i := range entries {
		e := &entries[i]
		for j := range e.Torrents {
			it, scope, ok, noPasskey := feedItemFor(e, &e.Torrents[j], abPasskey)
			if noPasskey {
				abSkippedNoPasskey++
			}
			if !ok {
				continue
			}
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
func feedItemFor(e *seadex.Entry, t *seadex.Torrent, abPasskey string) (it Item, scope string, ok, noPasskey bool) {
	scope = trackerScope(t.Tracker)
	if scope == "" {
		return Item{}, "", false, false
	}
	dl, resolved := downloadURL(t.Tracker, t.URL, abPasskey)
	if !resolved {
		return Item{}, scope, false, scope == upstreamAB && abPasskey == "" && animeBytesID(t.URL) != ""
	}
	return synthItem(e, t, dl), scope, true, false
}

// synthItem builds one feed Item for a SeaDex torrent with an already-resolved
// download link. The title is derived from the file names, the size summed from
// them, the info URL points at the SeaDex entry page, and the SeaDex marker is
// stamped (best -> 0.75 Freeleech25, alt -> 0.25 Freeleech75). The GUID is the
// tracker page URL (unique per torrent). Seeders are left 0 (the render floors
// to 1) since a synthesized item has no live swarm count.
func synthItem(e *seadex.Entry, t *seadex.Torrent, dl string) Item {
	dvf := dvfAlt
	if t.IsBest {
		dvf = dvfBest
	}
	return Item{
		Title:                feedTitle(t),
		GUID:                 t.UsableURL(),
		InfoURL:              entryURL(e.AniListID),
		DownloadURL:          dl,
		InfoHash:             validInfoHash(t.InfoHash),
		DownloadVolumeFactor: dvf,
		Categories:           feedCategories(t),
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

// multiSpace collapses runs of whitespace left after removing a token.
var multiSpace = regexp.MustCompile(`\s{2,}`)

// feedTitle synthesizes an arr-parseable release title from a torrent's file
// names - the core RSS gap, since SeaDex stores file names, not clean titles.
// It uses a representative episode file and collapses the episode marker to the
// season (S01E07 -> S01) so a pack is grabbed as a full season; an absolute-
// numbered pack has the number dropped; a single-file release (movie/OVA) is
// used verbatim. With no files it falls back to the release group. The result
// carries the group + quality/source tokens the file name already encodes, which
// is what the arr parses to match a monitored series and quality.
func feedTitle(t *seadex.Torrent) string {
	name := representativeFile(t.Files)
	if name == "" {
		return strings.TrimSpace(t.ReleaseGroup)
	}
	base := stripExt(name)
	if episodeToken.MatchString(base) {
		return strings.TrimSpace(episodeToken.ReplaceAllString(base, "${1}"))
	}
	if len(t.Files) > 1 && absoluteEpisode.MatchString(base) {
		return strings.TrimSpace(multiSpace.ReplaceAllString(absoluteEpisode.ReplaceAllString(base, " "), " "))
	}
	return strings.TrimSpace(base)
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
		if episodeToken.MatchString(files[i].Name) {
			return files[i].Name
		}
	}
	return files[0].Name
}

// feedCategories picks the Torznab category for a synthesized item. With no
// AniList format available here, it infers from the files: a season+episode
// token or more than one video file means a series (Anime, 5070); a lone video
// file with no episode marker is treated as a movie (Movies, 2000) so Radarr can
// pick up anime films via RSS. The ambiguous single-file special is the known
// edge (it may mis-tag as a movie; the arr simply ignores an unmatched item).
func feedCategories(t *seadex.Torrent) []int {
	if episodeToken.MatchString(representativeFile(t.Files)) {
		return []int{catAnime}
	}
	if videoFileCount(t.Files) <= 1 {
		return []int{catMovies}
	}
	return []int{catAnime}
}

// mediaExts are the video container extensions used to tell episode files from
// sidecar files (subtitles, samples) when counting/scanning a torrent's files.
var mediaExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m2ts": true,
	".ts": true, ".ogm": true, ".mov": true, ".wmv": true, ".webm": true,
}

// videoFileCount counts the torrent's files with a known video extension.
func videoFileCount(files []seadex.File) int {
	n := 0
	for i := range files {
		if mediaExts[strings.ToLower(path.Ext(files[i].Name))] {
			n++
		}
	}
	return n
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
func sortAndCap(items []Item) []Item {
	slices.SortStableFunc(items, func(a, b Item) int {
		return b.PubDate.Compare(a.PubDate)
	})
	if len(items) > feedWindow {
		items = items[:feedWindow]
	}
	return items
}

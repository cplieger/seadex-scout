package compare

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"slices"
	"strings"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/payload"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// MaxDownloadsPerFinding bounds Finding.Downloads; a set that would need more
// leaves the finding's download side unknown.
const MaxDownloadsPerFinding = 64

// Download is one torrent a finding's download set selects, at its full size.
// ID is the torrent's identity across findings: its info hash, else a digest of
// its tracker and raw URL, so one torrent two entries share counts once.
type Download struct {
	ID    string
	Bytes int64
}

// Replacement is one library file a finding's download set replaces whole.
// Key is library.Item.FileKey, so two findings replacing one file share it.
type Replacement struct {
	Key   string
	Bytes int64
}

// AddBytes is checked int64 addition over non-negative sizes: ok is false
// when the sum would pass math.MaxInt64.
func AddBytes(a, b int64) (int64, bool) {
	if b > math.MaxInt64-a {
		return 0, false
	}
	return a + b, true
}

// sized fills f's size fields when f is an upgrade (better release or newer
// revision): the download set drawn from pool, then the files it replaces.
// Each side stays at its zero value when it is unknown.
func sized(m *match.Match, d *align.Decision, pool []candidate, f *Finding) *Finding {
	if f == nil || (f.Status != StatusBetter && f.Status != StatusNewerRevision) || len(pool) == 0 {
		return f
	}
	set, ok := downloadSet(pool)
	if !ok {
		return f
	}
	downloads, total, ok := downloadsOf(set)
	if !ok {
		return f
	}
	f.Downloads, f.ReleaseBytes = downloads, total
	if replaced, current, ok := replacedFiles(m, d, set); ok {
		f.Replaced, f.CurrentBytes = replaced, current
	}
	return f
}

// downloadSet is the one coherent download the headline h stands for. A pack,
// or a torrent whose files name no episode, is h alone. A single episode is
// every single-episode candidate of h's release family, one per episode. ok is
// false when that family cannot be read as one download: a pack beside the
// singles, a file naming no episode, two candidates naming one episode (which
// also covers two torrents sharing a file name), or a mix of SxxExx and
// absolute numbering.
func downloadSet(pool []candidate) ([]candidate, bool) {
	h := representative(pool)
	if payload.DistinctEpisodes(payload.Census(h.torrent.Files)) != 1 {
		return []candidate{h}, true
	}
	family := releaseFamily(&h.rel)
	var set []candidate
	claims := episodeClaims{claimed: map[[2]int]bool{}}
	for i := range pool {
		c := &pool[i]
		if releaseFamily(&c.rel) != family {
			continue
		}
		census := payload.Census(c.torrent.Files)
		if payload.DistinctEpisodes(census) != 1 {
			return nil, false
		}
		spans, ok := payload.Spans(census)
		if !ok || !claims.claim(spans) {
			return nil, false
		}
		set = append(set, *c)
	}
	return set, true
}

// episodeClaims is the episodes a single-episode family has already claimed,
// in the one numbering its first span chose.
type episodeClaims struct {
	claimed   map[[2]int]bool
	namespace int
}

// claim records spans and reports false when one names an episode already
// claimed or switches numbering.
func (c *episodeClaims) claim(spans []payload.EpisodeSpan) bool {
	for _, s := range spans {
		ns := spanNamespace(s)
		if c.namespace != 0 && c.namespace != ns {
			return false
		}
		c.namespace = ns
		for e := s.First; e <= s.Last; e++ {
			if c.claimed[[2]int{s.Season, e}] {
				return false
			}
			c.claimed[[2]int{s.Season, e}] = true
		}
	}
	return true
}

// spanNamespace keeps a single-episode family to one numbering: the SxxExx
// season (offset by 2 so that it is never 0) or the absolute numbering.
func spanNamespace(s payload.EpisodeSpan) int {
	return s.Season + 2
}

// family is the release identity two single-episode torrents must share to be
// read as one download.
type family struct {
	group, tracker, resolution, codec string
	kind                              release.Kind
	dualAudio                         bool
}

func releaseFamily(r *release.Release) family {
	return family{
		group:      release.NormalizeGroup(r.Group),
		tracker:    strings.ToLower(strings.TrimSpace(r.Tracker)),
		resolution: r.Resolution,
		codec:      r.Codec,
		kind:       r.Kind,
		dualAudio:  r.DualAudio,
	}
}

// downloadsOf turns a download set into its records and checked total. ok is
// false for an unknown torrent size, a torrent with no identity, an identity
// two candidates share (one torrent cannot be two downloads), or a set past
// MaxDownloadsPerFinding.
func downloadsOf(set []candidate) ([]Download, int64, bool) {
	if len(set) == 0 || len(set) > MaxDownloadsPerFinding {
		return nil, 0, false
	}
	downloads := make([]Download, 0, len(set))
	seen := make(map[string]bool, len(set))
	var total int64
	for i := range set {
		size := payload.TotalSize(set[i].torrent.Files)
		id := downloadID(&set[i].torrent)
		if size <= 0 || id == "" || seen[id] {
			return nil, 0, false
		}
		seen[id] = true
		sum, ok := AddBytes(total, size)
		if !ok {
			return nil, 0, false
		}
		total = sum
		downloads = append(downloads, Download{ID: id, Bytes: size})
	}
	return downloads, total, true
}

// downloadID is a torrent's identity: "btih:" plus its info hash, else
// "src:" plus a digest of its tracker and raw URL, else "" (no identity).
func downloadID(t *seadex.Torrent) string {
	if h := seadex.ValidInfoHash(t.InfoHash); h != "" {
		return "btih:" + h
	}
	if strings.TrimSpace(t.URL) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(t.Tracker)) + "\x00" + strings.TrimSpace(t.URL)))
	return "src:" + hex.EncodeToString(sum[:])
}

// replacedFiles lists the library files the download set replaces whole and
// their checked total. A movie replaces its one file. A season replaces each
// file all of whose episodes the set covers. ok is false for every other
// scope, a season another entry also maps, an episode the set covers that
// has no file or no clear number, an unknown file size, or nothing replaced.
func replacedFiles(m *match.Match, d *align.Decision, set []candidate) ([]Replacement, int64, bool) {
	item := m.Item
	if item == nil {
		return nil, 0, false
	}
	var files []int
	switch d.Kind {
	case align.ScopeMovie:
		for id := range item.FileBytes {
			files = append(files, id)
		}
	case align.ScopeSeason:
		if slices.Contains(m.SiblingSeasons, d.Season) {
			return nil, 0, false
		}
		covered, ok := coveredFiles(item, d.Season, set)
		if !ok {
			return nil, 0, false
		}
		files = covered
	default:
		return nil, 0, false
	}
	if len(files) == 0 {
		return nil, 0, false
	}
	slices.Sort(files)
	replaced := make([]Replacement, 0, len(files))
	var total int64
	for _, id := range files {
		size := item.FileBytes[id]
		if size <= 0 {
			return nil, 0, false
		}
		sum, ok := AddBytes(total, size)
		if !ok {
			return nil, 0, false
		}
		total = sum
		replaced = append(replaced, Replacement{Key: item.FileKey(id), Bytes: size})
	}
	return replaced, total, true
}

// coveredFiles maps the set's episodes onto season's files and returns the
// files whose every episode is covered. An SxxExx span must name season; an
// absolute number must be the Absolute of exactly one episode of the series,
// and that episode must sit in season.
func coveredFiles(item *library.Item, season int, set []candidate) ([]int, bool) {
	episodes := item.SeasonEpisodes[season]
	if episodes == nil {
		return nil, false
	}
	cover := seasonCover{item: item, season: season, episodes: episodes, covered: map[[2]int]bool{}, touched: map[int]bool{}}
	for i := range set {
		spans, ok := payload.Spans(payload.Census(set[i].torrent.Files))
		if !ok {
			return nil, false
		}
		for _, s := range spans {
			if !cover.add(s) {
				return nil, false
			}
		}
	}
	var files []int
	for file := range cover.touched {
		if fileCovered(item, file, cover.covered) {
			files = append(files, file)
		}
	}
	return files, true
}

type seasonCover struct {
	item     *library.Item
	episodes map[int]library.Episode
	covered  map[[2]int]bool
	touched  map[int]bool
	season   int
}

// add covers span s, or reports false when it names another season, an
// episode with no file, or an absolute number no single episode of season
// carries.
func (c *seasonCover) add(s payload.EpisodeSpan) bool {
	if s.Season != payload.AbsoluteSeason && s.Season != c.season {
		return false
	}
	for n := s.First; n <= s.Last; n++ {
		e := n
		if s.Season == payload.AbsoluteSeason {
			var ok bool
			if e, ok = absoluteEpisode(c.item, c.season, n); !ok {
				return false
			}
		}
		ep, ok := c.episodes[e]
		if !ok || ep.File <= 0 {
			return false
		}
		c.covered[[2]int{c.season, e}] = true
		c.touched[ep.File] = true
	}
	return true
}

// absoluteEpisode is the season-relative number of the one episode of the
// series whose absolute number is n. ok is false when none or several carry
// n, or the one that does sits outside season: Sonarr numbers episodes per
// season, so n itself is never a key into season's episodes.
func absoluteEpisode(item *library.Item, season, n int) (int, bool) {
	found, ok := 0, false
	for s, eps := range item.SeasonEpisodes {
		for e, ep := range eps {
			if ep.Absolute != n {
				continue
			}
			if ok || s != season {
				return 0, false
			}
			found, ok = e, true
		}
	}
	return found, ok
}

// fileCovered reports whether every episode file holds is in covered, so
// replacing the file loses nothing the download does not bring back.
func fileCovered(item *library.Item, file int, covered map[[2]int]bool) bool {
	for s, eps := range item.SeasonEpisodes {
		for e, ep := range eps {
			if ep.File == file && !covered[[2]int{s, e}] {
				return false
			}
		}
	}
	return true
}

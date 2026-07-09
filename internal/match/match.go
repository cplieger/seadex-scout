// Package match links SeaDex entries to library items. It resolves an entry's
// AniList ID to arr IDs through the Fribb mapping (overrides already applied),
// and on a miss falls back to an AniList title lookup plus a conservative
// normalized-title-plus-year match against the library. It also reports
// ID-mapping coverage and maintains a memo so a given AniList ID is fetched at
// most once (titles and format do not change).
package match

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/cplieger/seadex-scout/internal/anilist"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// arrUnknown labels coverage for an entry whose arr could not be determined.
const arrUnknown = "unknown"

// Source records how an entry was linked to a library item.
type Source string

const (
	// SourceID means the AniList ID resolved to an arr ID via the Fribb map.
	SourceID Source = "id"
	// SourceTitle means the AniList title fallback matched a library item.
	SourceTitle Source = "title"
	// SourceUnmapped means no library item was found for the entry.
	SourceUnmapped Source = "unmapped"
)

// AniListClient is the AniList fallback surface the matcher needs.
type AniListClient interface {
	Fetch(ctx context.Context, aniListID int) (anilist.Media, error)
}

// Match is the result of linking one SeaDex entry.
type Match struct {
	Item   *library.Item
	Arr    string
	Source Source
	Entry  seadex.Entry
	Record mapping.Record
}

// InLibrary reports whether the entry was matched to a library item.
func (m *Match) InLibrary() bool { return m.Item != nil }

// Coverage counts ID-mapping outcomes per arr for the coverage metrics.
type Coverage struct {
	Hits     map[string]int
	Unmapped map[string]int
}

// MemoEntry is a cached AniList lookup (titles/format/year), or a negative
// result, keyed by AniList ID in a Memo.
type MemoEntry struct {
	Format   string   `json:"format,omitempty"`
	Titles   []string `json:"titles,omitempty"`
	Year     int      `json:"year,omitempty"`
	NotFound bool     `json:"not_found,omitempty"`
}

// Memo persists AniList fallback lookups across cycles.
type Memo struct {
	Entries map[int]MemoEntry `json:"entries,omitempty"`
}

// Result bundles the per-entry matches, the coverage counts, and the updated
// memo to persist.
type Result struct {
	Coverage Coverage
	Memo     Memo
	Matches  []Match
}

// Matcher links entries using the mapping index and the AniList fallback.
type Matcher struct {
	anilist AniListClient
	log     *slog.Logger
}

// NewMatcher builds a Matcher. logger may be nil.
func NewMatcher(anilistClient AniListClient, logger *slog.Logger) *Matcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Matcher{anilist: anilistClient, log: logger}
}

// Match links every entry to a library item (where present), returning the
// matches, ID-mapping coverage, and the updated memo. It never fails as a
// whole: an AniList fallback error for one entry is logged and that entry is
// left unmatched.
func (m *Matcher) Match(ctx context.Context, entries []seadex.Entry, snap *library.Snapshot, idx *mapping.Index, memo Memo) Result {
	lib := buildLibIndex(snap)
	if memo.Entries == nil {
		memo.Entries = make(map[int]MemoEntry)
	}
	cov := Coverage{Hits: make(map[string]int), Unmapped: make(map[string]int)}
	matches := make([]Match, 0, len(entries))
	for i := range entries {
		matches = append(matches, m.matchEntry(ctx, &entries[i], lib, idx, &memo, &cov))
	}
	return Result{Coverage: cov, Memo: memo, Matches: matches}
}

// matchEntry links one entry: ID resolution first, AniList title fallback next.
func (m *Matcher) matchEntry(ctx context.Context, e *seadex.Entry, lib *libIndex, idx *mapping.Index, memo *Memo, cov *Coverage) Match {
	if rec, ok := idx.Lookup(e.AniListID); ok {
		arr := recordArr(&rec)
		cov.Hits[arr]++
		item := lib.findByID(&rec)
		return Match{Item: item, Entry: *e, Record: rec, Arr: arr, Source: sourceFor(item)}
	}

	media, ok := m.lookupAniList(ctx, e.AniListID, memo)
	if !ok {
		cov.Unmapped[arrUnknown]++
		return Match{Entry: *e, Arr: arrUnknown, Source: SourceUnmapped}
	}
	arr := formatArr(media.Format)
	cov.Unmapped[arr]++
	item := lib.findByTitle(media.Titles, media.Year, arr, m.log)
	if item == nil {
		return Match{Entry: *e, Arr: arr, Source: SourceUnmapped}
	}
	return Match{Item: item, Entry: *e, Arr: arr, Source: SourceTitle}
}

// lookupAniList consults the memo, then AniList. A not-found result is memoized
// (negatively) so it is not re-fetched; a transient error is not memoized so it
// is retried next cycle.
func (m *Matcher) lookupAniList(ctx context.Context, aniListID int, memo *Memo) (anilist.Media, bool) {
	if ent, ok := memo.Entries[aniListID]; ok {
		if ent.NotFound {
			return anilist.Media{}, false
		}
		return anilist.Media{Titles: ent.Titles, Format: ent.Format, Year: ent.Year}, true
	}
	media, err := m.anilist.Fetch(ctx, aniListID)
	if err != nil {
		if errors.Is(err, anilist.ErrNotFound) {
			memo.Entries[aniListID] = MemoEntry{NotFound: true}
		} else {
			m.log.Warn("anilist fallback failed", "al_id", aniListID, "error", err)
		}
		return anilist.Media{}, false
	}
	memo.Entries[aniListID] = MemoEntry{Titles: media.Titles, Format: media.Format, Year: media.Year}
	return media, true
}

// sourceFor reports the match source for an ID-resolved entry: SourceID when a
// library item was found, SourceUnmapped when it resolved but is not in the
// library.
func sourceFor(item *library.Item) Source {
	if item != nil {
		return SourceID
	}
	return SourceUnmapped
}

// recordArr routes a mapping record to its arr (MOVIE -> Radarr, else Sonarr).
func recordArr(r *mapping.Record) string {
	if r.IsMovie() {
		return library.ArrRadarr
	}
	return library.ArrSonarr
}

// formatArr routes an AniList format to its arr (MOVIE -> Radarr, else Sonarr).
// An empty format is unknown.
func formatArr(format string) string {
	switch strings.ToUpper(strings.TrimSpace(format)) {
	case "":
		return arrUnknown
	case "MOVIE":
		return library.ArrRadarr
	default:
		return library.ArrSonarr
	}
}

// libIndex indexes a library snapshot by external ID and normalized title.
type libIndex struct {
	byTvdb  map[int]*library.Item
	byTmdb  map[int]*library.Item
	byImdb  map[string]*library.Item
	byTitle map[string][]*library.Item
}

// buildLibIndex builds the lookup indexes over a snapshot's items.
func buildLibIndex(snap *library.Snapshot) *libIndex {
	li := &libIndex{
		byTvdb:  make(map[int]*library.Item),
		byTmdb:  make(map[int]*library.Item),
		byImdb:  make(map[string]*library.Item),
		byTitle: make(map[string][]*library.Item),
	}
	if snap == nil {
		return li
	}
	for i := range snap.Items {
		it := &snap.Items[i]
		if it.TvdbID != 0 {
			li.byTvdb[it.TvdbID] = it
		}
		if it.TmdbID != 0 {
			li.byTmdb[it.TmdbID] = it
		}
		if it.ImdbID != "" {
			li.byImdb[it.ImdbID] = it
		}
		li.indexTitles(it)
	}
	return li
}

// indexTitles adds an item's primary and alternate titles to the title index.
func (li *libIndex) indexTitles(it *library.Item) {
	li.addTitle(it.Title, it)
	for _, t := range it.AltTitles {
		li.addTitle(t, it)
	}
}

// addTitle indexes one title for an item under its normalized key.
func (li *libIndex) addTitle(title string, it *library.Item) {
	if key := normalizeTitle(title); key != "" {
		li.byTitle[key] = append(li.byTitle[key], it)
	}
}

// findByID looks up a library item by the arr IDs in a mapping record.
func (li *libIndex) findByID(rec *mapping.Record) *library.Item {
	if rec.IsMovie() {
		for _, id := range rec.TmdbMovies {
			if it := li.byTmdb[id]; it != nil {
				return it
			}
		}
		for _, imdb := range rec.IMDbIDs {
			if it := li.byImdb[imdb]; it != nil {
				return it
			}
		}
		return nil
	}
	if rec.TvdbID != 0 {
		return li.byTvdb[rec.TvdbID]
	}
	return nil
}

// findByTitle performs the conservative title fallback: it collects candidates
// matching any of the titles (restricted to the arr when known), narrows by
// year when known, and returns a match only when exactly one candidate remains.
// An ambiguous set is logged and treated as a miss.
func (li *libIndex) findByTitle(titles []string, year int, arr string, log *slog.Logger) *library.Item {
	candidates := li.titleCandidates(titles, arr)
	if year != 0 {
		if narrowed := filterByYear(candidates, year); len(narrowed) > 0 {
			candidates = narrowed
		}
	}
	switch len(candidates) {
	case 1:
		return candidates[0]
	case 0:
		return nil
	default:
		log.Debug("title fallback ambiguous, treating as unmapped", "titles", titles, "candidates", len(candidates))
		return nil
	}
}

// titleCandidates returns the distinct library items whose (normalized) title
// or alternate title equals any of titles, optionally restricted to arr.
func (li *libIndex) titleCandidates(titles []string, arr string) []*library.Item {
	seen := make(map[*library.Item]struct{})
	var candidates []*library.Item
	for _, t := range titles {
		key := normalizeTitle(t)
		if key == "" {
			continue
		}
		for _, it := range li.byTitle[key] {
			if arr != "" && arr != arrUnknown && it.Arr != arr {
				continue
			}
			if _, dup := seen[it]; dup {
				continue
			}
			seen[it] = struct{}{}
			candidates = append(candidates, it)
		}
	}
	return candidates
}

// filterByYear returns the candidates whose year equals year.
func filterByYear(candidates []*library.Item, year int) []*library.Item {
	var out []*library.Item
	for _, it := range candidates {
		if it.Year == year {
			out = append(out, it)
		}
	}
	return out
}

// reTitleStrip removes every character that is not a lowercase letter or digit.
var reTitleStrip = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeTitle lowercases a title and strips all non-alphanumeric characters
// so punctuation, spacing, and separators do not defeat an otherwise exact
// match. It is deliberately conservative (no transliteration or fuzzy edits).
func normalizeTitle(s string) string {
	return reTitleStrip.ReplaceAllString(strings.ToLower(s), "")
}

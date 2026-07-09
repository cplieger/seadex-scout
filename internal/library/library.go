// Package library ingests the Sonarr/Radarr anime library through arrapi into a
// snapshot: per item its external IDs, tags, current release groups, and a
// representative release fingerprint. It applies arr-side tag include/exclude
// and can diff two snapshots to report what changed on the arr side.
//
// A per-series episode fetch failure is logged and the series skipped (the
// snapshot is partial but usable); only a failure of the top-level series/movie
// list, or a cancelled context, fails the whole walk. This mirrors the
// "ingest succeeded == healthy" semantic the scout uses.
package library

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/arrapi"
	"github.com/cplieger/seadex-scout/internal/release"
)

// Arr names label an item's source instance.
const (
	ArrSonarr = "sonarr"
	ArrRadarr = "radarr"
)

// episodeConcurrency bounds concurrent per-series episode fetches.
const episodeConcurrency = 6

// SonarrClient is the arrapi Sonarr surface the walker needs (consumer-side
// interface; *arrapi.Sonarr satisfies it).
type SonarrClient interface {
	GetSeries(ctx context.Context) ([]arrapi.Series, error)
	GetEpisodes(ctx context.Context, seriesID int) ([]arrapi.Episode, error)
	ResolveTagIDs(ctx context.Context, labels ...string) (map[int]struct{}, []string, error)
}

// RadarrClient is the arrapi Radarr surface the walker needs.
type RadarrClient interface {
	GetMovies(ctx context.Context) ([]arrapi.Movie, error)
	ResolveTagIDs(ctx context.Context, labels ...string) (map[int]struct{}, []string, error)
}

// Item is one library entry (series or movie) in a snapshot. Fields are ordered
// for govet fieldalignment.
type Item struct {
	SeasonGroups map[int][]string `json:"season_groups,omitempty"`
	Arr          string           `json:"arr"`
	ImdbID       string           `json:"imdb_id,omitempty"`
	Title        string           `json:"title"`
	ArrURL       string           `json:"arr_url,omitempty"`
	Current      release.Release  `json:"current"`
	Tags         []int            `json:"tags,omitempty"`
	AltTitles    []string         `json:"alt_titles,omitempty"`
	Groups       []string         `json:"groups,omitempty"`
	ArrID        int              `json:"arr_id"`
	TvdbID       int              `json:"tvdb_id,omitempty"`
	TmdbID       int              `json:"tmdb_id,omitempty"`
	Year         int              `json:"year,omitempty"`
	MixedGroups  bool             `json:"mixed_groups,omitempty"`
	HasFile      bool             `json:"has_file"`
}

// Snapshot is one library walk.
type Snapshot struct {
	TakenAt time.Time `json:"taken_at"`
	Items   []Item    `json:"items,omitempty"`
}

// Diff summarizes what changed between two snapshots (by arr + arr id).
type Diff struct {
	Added   int
	Removed int
	Changed int
}

// Walker ingests the library through the configured arr clients.
type Walker struct {
	sonarr      SonarrClient
	radarr      RadarrClient
	log         *slog.Logger
	remuxGroups map[string]bool
	sonarrURL   string
	radarrURL   string
	includeTags []string
	excludeTags []string
}

// Config configures a Walker. Sonarr/Radarr may be nil to disable that side.
// SonarrURL / RadarrURL are the instance base URLs used to build per-item
// deep-link URLs (empty disables the link for that side).
type Config struct {
	Sonarr      SonarrClient
	Radarr      RadarrClient
	Logger      *slog.Logger
	RemuxGroups map[string]bool
	SonarrURL   string
	RadarrURL   string
	IncludeTags []string
	ExcludeTags []string
}

// NewWalker builds a Walker from cfg.
func NewWalker(cfg *Config) *Walker {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Walker{
		sonarr:      cfg.Sonarr,
		radarr:      cfg.Radarr,
		log:         log,
		remuxGroups: cfg.RemuxGroups,
		sonarrURL:   cfg.SonarrURL,
		radarrURL:   cfg.RadarrURL,
		includeTags: cfg.IncludeTags,
		excludeTags: cfg.ExcludeTags,
	}
}

// Walk ingests both arr sides into a single snapshot. It returns an error only
// when a top-level list call fails or ctx is cancelled; per-series episode
// failures are skipped with a warning.
func (w *Walker) Walk(ctx context.Context) (Snapshot, error) {
	var items []Item

	if w.sonarr != nil {
		series, err := w.walkSonarr(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		items = append(items, series...)
	}
	if w.radarr != nil {
		movies, err := w.walkRadarr(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		items = append(items, movies...)
	}

	w.log.Info("library walk complete", "items", len(items),
		"sonarr", w.sonarr != nil, "radarr", w.radarr != nil)
	return Snapshot{TakenAt: time.Now(), Items: items}, nil
}

// walkSonarr lists series, applies tag filters, and builds an item per kept
// series with its episode files fetched concurrently (bounded).
func (w *Walker) walkSonarr(ctx context.Context) ([]Item, error) {
	series, err := w.sonarr.GetSeries(ctx)
	if err != nil {
		return nil, err
	}
	includeIDs, excludeIDs := w.resolveTags(ctx, w.sonarr.ResolveTagIDs)

	var kept []arrapi.Series
	for i := range series {
		if keepByTags(series[i].Tags, includeIDs, excludeIDs) {
			kept = append(kept, series[i])
		}
	}

	items := make([]Item, len(kept))
	var wg sync.WaitGroup
	sem := make(chan struct{}, episodeConcurrency)
	for i := range kept {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s := &kept[i]
			eps, epErr := w.sonarr.GetEpisodes(ctx, s.ID)
			if epErr != nil {
				w.log.Warn("skipping series: episode fetch failed", "series", s.Title, "id", s.ID, "error", epErr)
				items[i] = w.seriesItem(s, nil)
				return
			}
			items[i] = w.seriesItem(s, eps)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// walkRadarr lists movies, applies tag filters, and builds an item per movie.
func (w *Walker) walkRadarr(ctx context.Context) ([]Item, error) {
	movies, err := w.radarr.GetMovies(ctx)
	if err != nil {
		return nil, err
	}
	includeIDs, excludeIDs := w.resolveTags(ctx, w.radarr.ResolveTagIDs)

	var items []Item
	for i := range movies {
		if keepByTags(movies[i].Tags, includeIDs, excludeIDs) {
			items = append(items, w.movieItem(&movies[i]))
		}
	}
	return items, nil
}

// resolveTags resolves the include and exclude tag labels to ID sets, logging
// any label that matched no tag. A resolution failure logs a warning and
// disables that side's filter (fail-open) rather than aborting the walk.
func (w *Walker) resolveTags(ctx context.Context,
	resolve func(context.Context, ...string) (map[int]struct{}, []string, error),
) (includeIDs, excludeIDs map[int]struct{}) {
	includeIDs = w.resolveOne(ctx, resolve, "INCLUDE_TAGS", w.includeTags)
	excludeIDs = w.resolveOne(ctx, resolve, "EXCLUDE_TAGS", w.excludeTags)
	return includeIDs, excludeIDs
}

// resolveOne resolves a single label set, logging unmatched labels and
// fail-opening (nil set) on error.
func (w *Walker) resolveOne(ctx context.Context,
	resolve func(context.Context, ...string) (map[int]struct{}, []string, error),
	which string, labels []string,
) map[int]struct{} {
	if len(labels) == 0 {
		return nil
	}
	ids, unmatched, err := resolve(ctx, labels...)
	if err != nil {
		w.log.Warn("tag resolution failed; filter disabled for this cycle", "which", which, "error", err)
		return nil
	}
	if len(unmatched) > 0 {
		w.log.Warn("configured tags matched no arr tag", "which", which, "unmatched", strings.Join(unmatched, ","))
	}
	return ids
}

// keepByTags applies include-then-exclude tag filtering. Include (when set)
// requires a match; exclude (when set) rejects a match.
func keepByTags(itemTags []int, includeIDs, excludeIDs map[int]struct{}) bool {
	if len(includeIDs) > 0 && !arrapi.HasAnyTag(itemTags, includeIDs) {
		return false
	}
	if len(excludeIDs) > 0 && arrapi.HasAnyTag(itemTags, excludeIDs) {
		return false
	}
	return true
}

// seriesItem builds a library Item from a series and its episodes, aggregating
// the distinct release groups present and a representative fingerprint.
func (w *Walker) seriesItem(s *arrapi.Series, eps []arrapi.Episode) Item {
	var files []fileInfo
	groupCounts := make(map[string]int)
	seasonCounts := make(map[int]map[string]int)
	for i := range eps {
		f := eps[i].EpisodeFile
		if f == nil {
			continue
		}
		fi := fileFromEpisode(f)
		files = append(files, fi)
		if fi.group != "" {
			groupCounts[fi.group]++
			addSeasonGroup(seasonCounts, eps[i].SeasonNumber, fi.group)
		}
	}
	groups := sortedKeys(groupCounts)
	rep := representative(files, groupCounts)
	return Item{
		Current:      w.fingerprint(&rep),
		SeasonGroups: seasonGroups(seasonCounts),
		Groups:       groups,
		Tags:         s.Tags,
		AltTitles:    altTitles(s.AlternateTitles),
		Arr:          ArrSonarr,
		Title:        s.Title,
		ImdbID:       s.ImdbID,
		ArrURL:       s.WebURL(w.sonarrURL),
		ArrID:        s.ID,
		TvdbID:       s.TvdbID,
		TmdbID:       s.TmdbID,
		Year:         s.Year,
		MixedGroups:  len(groups) > 1,
		HasFile:      len(files) > 0,
	}
}

// movieItem builds a library Item from a movie and its file.
func (w *Walker) movieItem(m *arrapi.Movie) Item {
	item := Item{
		Tags:      m.Tags,
		AltTitles: altTitles(m.AlternateTitles),
		Arr:       ArrRadarr,
		Title:     m.Title,
		ImdbID:    m.ImdbID,
		ArrURL:    m.WebURL(w.radarrURL),
		ArrID:     m.ID,
		TmdbID:    m.TmdbID,
		Year:      m.Year,
		HasFile:   m.HasFile && m.MovieFile != nil,
	}
	if item.HasFile {
		fi := fileFromMovie(m.MovieFile)
		if fi.group != "" {
			item.Groups = []string{fi.group}
		}
		item.Current = w.fingerprint(&fi)
	}
	return item
}

// fingerprint classifies a library file into a release.Release using the shared
// classifier, so the library and SeaDex sides compare in one vocabulary.
func (w *Walker) fingerprint(fi *fileInfo) release.Release {
	return release.Classify(&release.Input{
		Names:       nonEmpty(fi.sceneName, fi.relPath),
		Group:       fi.group,
		VideoCodec:  fi.videoCodec,
		DualAudio:   isDualAudio(fi.audioLanguages),
		RemuxGroups: w.remuxGroups,
	})
}

// fileInfo is the release-relevant subset of an arr file.
type fileInfo struct {
	group          string
	sceneName      string
	relPath        string
	videoCodec     string
	audioLanguages string
}

// fileFromEpisode extracts fileInfo from a Sonarr episode file.
func fileFromEpisode(f *arrapi.EpisodeFile) fileInfo {
	fi := fileInfo{
		group:     release.NormalizeGroup(f.ReleaseGroup),
		sceneName: f.SceneName,
		relPath:   f.RelativePath,
	}
	if f.MediaInfo != nil {
		fi.videoCodec = f.MediaInfo.VideoCodec
		fi.audioLanguages = f.MediaInfo.AudioLanguages
	}
	return fi
}

// fileFromMovie extracts fileInfo from a Radarr movie file.
func fileFromMovie(f *arrapi.MovieFile) fileInfo {
	fi := fileInfo{
		group:     release.NormalizeGroup(f.ReleaseGroup),
		sceneName: f.SceneName,
		relPath:   f.RelativePath,
	}
	if f.MediaInfo != nil {
		fi.videoCodec = f.MediaInfo.VideoCodec
		fi.audioLanguages = f.MediaInfo.AudioLanguages
	}
	return fi
}

// representative returns the file whose group is the most common on the item
// (ties broken by the first such file), so the reported current fingerprint
// reflects the dominant release rather than an outlier episode. It returns the
// zero fileInfo when there are no files.
func representative(files []fileInfo, groupCounts map[string]int) fileInfo {
	best := fileInfo{}
	bestCount := -1
	for _, f := range files {
		count := groupCounts[f.group]
		if count > bestCount {
			best, bestCount = f, count
		}
	}
	return best
}

// isDualAudio reports whether a MediaInfo audio-languages string names more
// than one language (e.g. "Japanese / English", "jpn/eng").
func isDualAudio(langs string) bool {
	fields := strings.FieldsFunc(langs, func(r rune) bool { return r == '/' || r == ',' })
	distinct := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(strings.ToLower(f)); f != "" {
			distinct[f] = struct{}{}
		}
	}
	return len(distinct) > 1
}

// addSeasonGroup records a release group under a season number.
func addSeasonGroup(counts map[int]map[string]int, season int, group string) {
	if counts[season] == nil {
		counts[season] = make(map[string]int)
	}
	counts[season][group]++
}

// seasonGroups converts per-season group counts into sorted group slices,
// returning nil when there are none (so the field stays omitempty).
func seasonGroups(counts map[int]map[string]int) map[int][]string {
	if len(counts) == 0 {
		return nil
	}
	out := make(map[int][]string, len(counts))
	for season, gc := range counts {
		out[season] = sortedKeys(gc)
	}
	return out
}

// sortedKeys returns the map keys sorted, for a stable groups slice.
func sortedKeys(m map[string]int) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// altTitles extracts the non-empty alternate-title strings from arr metadata,
// used by the AniList title-fallback matcher.
func altTitles(alts []arrapi.AlternateTitle) []string {
	var out []string
	for _, a := range alts {
		if t := strings.TrimSpace(a.Title); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// nonEmpty returns the non-empty strings among the arguments.
func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// DiffSnapshots reports what changed between prev and cur, keyed by arr + id.
// An item is Changed when its group set or current fingerprint differs.
func DiffSnapshots(prev, cur *Snapshot) Diff {
	prevByKey := indexByKey(prev)
	curByKey := indexByKey(cur)
	var d Diff
	for k, c := range curByKey {
		p, ok := prevByKey[k]
		switch {
		case !ok:
			d.Added++
		case !sameItem(p, c):
			d.Changed++
		}
	}
	for k := range prevByKey {
		if _, ok := curByKey[k]; !ok {
			d.Removed++
		}
	}
	return d
}

// indexByKey keys a snapshot's items by "arr:id" (values point into the
// snapshot's backing array, avoiding per-item copies).
func indexByKey(s *Snapshot) map[string]*Item {
	m := make(map[string]*Item, len(s.Items))
	for i := range s.Items {
		it := &s.Items[i]
		m[it.Arr+":"+strconv.Itoa(it.ArrID)] = it
	}
	return m
}

// sameItem reports whether two items have the same current release state
// (group set and fingerprint), for diff change detection.
func sameItem(a, b *Item) bool {
	if a.HasFile != b.HasFile || a.Current != b.Current || len(a.Groups) != len(b.Groups) {
		return false
	}
	for i := range a.Groups {
		if a.Groups[i] != b.Groups[i] {
			return false
		}
	}
	return true
}

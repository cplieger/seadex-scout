// Package library ingests the Sonarr/Radarr anime library through arrapi into a
// snapshot: per item its external IDs, tags, current release groups, and a
// representative release fingerprint. It applies arr-side tag include/exclude
// and can diff two snapshots to report what changed on the arr side.
//
// A per-series episode fetch failure is logged and the series kept as a
// Failed placeholder item (the snapshot is partial but usable); a failure of
// the top-level series/movie list, a tag-resolution failure, a cancelled
// context, or per-series failures hitting the walk failure budget fail the
// whole walk. This mirrors the "ingest succeeded == healthy" semantic the
// scout uses.
package library

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// episodeFailureBudget caps per-series episode-fetch failures per walk. arrapi
// retries every request with long timeouts, so a Sonarr outage mid-walk would
// otherwise grind through EVERY kept series (hours) before the walk finished;
// once this many series have failed the fault is the arr, not a per-series
// blip, so the remaining fan-out is cancelled and the walk fails as a whole
// (ingest failure, so the cycle is unhealthy). 5 tolerates isolated per-series
// oddities (the partial-snapshot path) while tripping fast on an outage.
const episodeFailureBudget = 5

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
	AltTitles    []string         `json:"alt_titles,omitempty"`
	Groups       []string         `json:"groups,omitempty"`
	ArrID        int              `json:"arr_id"`
	TvdbID       int              `json:"tvdb_id,omitempty"`
	TmdbID       int              `json:"tmdb_id,omitempty"`
	Year         int              `json:"year,omitempty"`
	HasFile      bool             `json:"has_file"`
	// Failed marks a series whose episode fetch failed this walk: the item
	// carries its arr identity (so consumers can tell WHICH items the partial
	// walk is missing) but no file data. Consumers must not read a Failed
	// item's absent groups as a real no-file library state.
	Failed bool `json:"failed,omitempty"`
}

// Key identifies the item by its arr source and arr ID ("arr:id") - the
// item's semantic identity across snapshots and packages. Snapshot diffing
// (indexByKey) and the audit's covered-item map both key on it, so the
// identity rule is written once here in the package that owns Item.
func (it *Item) Key() string {
	return it.Arr + ":" + strconv.Itoa(it.ArrID)
}

// Snapshot is one library walk.
type Snapshot struct {
	TakenAt time.Time `json:"taken_at"`
	Items   []Item    `json:"items,omitempty"`
	// Partial reports that at least one series' episode fetch failed: those
	// items are present with Failed=true and no file data, so the snapshot is
	// usable but not a complete library view, and consumers must not treat an
	// absent or Failed item as removed.
	Partial bool `json:"partial,omitempty"`
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
		sonarrURL:   cfg.SonarrURL,
		radarrURL:   cfg.RadarrURL,
		includeTags: cfg.IncludeTags,
		excludeTags: cfg.ExcludeTags,
	}
}

// Walk ingests both arr sides into a single snapshot. It returns an error when
// a top-level list call fails, tag resolution fails (fail closed), ctx is
// cancelled, or per-series episode failures hit the walk failure budget;
// sub-budget per-series failures are kept as Failed placeholder items with a
// warning (the snapshot is partial).
func (w *Walker) Walk(ctx context.Context) (Snapshot, error) {
	var items []Item
	partial := false

	if w.sonarr != nil {
		series, skipped, err := w.walkSonarr(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		items = append(items, series...)
		partial = skipped > 0
	}
	if w.radarr != nil {
		movies, err := w.walkRadarr(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		items = append(items, movies...)
	}

	// Final cancellation guard: when both sides are disabled (or the last side
	// returned just before cancellation), neither helper observed ctx, so an
	// already-cancelled walk must not publish a snapshot labelled complete.
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}

	w.log.Info("library walk complete", "items", len(items), "partial", partial,
		"sonarr", w.sonarr != nil, "radarr", w.radarr != nil)
	return Snapshot{TakenAt: time.Now().UTC(), Items: items, Partial: partial}, nil
}

// filterSeriesByTags returns the series that pass the include/exclude tag
// filters, in input order (the pure filtering step of the Sonarr walk).
func filterSeriesByTags(series []arrapi.Series, includeIDs, excludeIDs map[int]struct{}) []arrapi.Series {
	kept := make([]arrapi.Series, 0, len(series))
	for i := range series {
		if keepByTags(series[i].Tags, includeIDs, excludeIDs) {
			kept = append(kept, series[i])
		}
	}
	return kept
}

// walkSonarr lists series, applies tag filters, and builds an item per kept
// series with its episode files fetched concurrently (bounded). A per-series
// episode-fetch failure keeps the series' arr identity as a Failed placeholder
// item (the snapshot is partial); once episodeFailureBudget failures
// accumulate, the remaining fan-out is cancelled and the walk fails as a whole
// (an arr outage, not per-series blips).
func (w *Walker) walkSonarr(ctx context.Context) ([]Item, int, error) {
	series, err := w.sonarr.GetSeries(ctx)
	if err != nil {
		return nil, 0, err
	}
	includeIDs, excludeIDs, err := w.resolveTags(ctx, w.sonarr.ResolveTagIDs)
	if err != nil {
		return nil, 0, err
	}

	kept := filterSeriesByTags(series, includeIDs, excludeIDs)

	results, skipped := w.fetchEpisodeItems(ctx, kept)
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if skipped >= episodeFailureBudget {
		return nil, 0, fmt.Errorf("sonarr episode fetches: %d series failed, hitting the walk failure budget of %d", skipped, episodeFailureBudget)
	}
	items := make([]Item, 0, len(results))
	for _, item := range results {
		if item != nil {
			items = append(items, *item)
		}
	}
	if skipped > 0 {
		w.log.Warn("sonarr series skipped after episode-fetch failures; snapshot is partial",
			"skipped", skipped, "kept", len(kept))
	}
	return items, skipped, nil
}

// fetchEpisodeItems runs the bounded episode-fetch fan-out over the kept
// series, returning the per-series results (nil where a fetch was cancelled or
// skipped) and the failure count. The fan-out context is cancelled once the
// failure budget is hit, so in-flight fetches stop and queued ones skip
// instead of spending arrapi's per-request retries against a down Sonarr,
// series after series; the caller turns a budget-hitting count into a walk
// failure.
func (w *Walker) fetchEpisodeItems(ctx context.Context, kept []arrapi.Series) (results []*Item, failed int) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()
	var failures atomic.Int64
	results = make([]*Item, len(kept))
	var wg sync.WaitGroup
	sem := make(chan struct{}, episodeConcurrency)
	for i := range kept {
		if fanCtx.Err() != nil {
			break // budget tripped (or shutdown): stop feeding; remaining results stay nil (skipped)
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if fanCtx.Err() != nil {
				return // budget tripped (or shutdown): skip without fetching
			}
			item, fetchFailed := w.fetchSeriesItem(fanCtx, &kept[i])
			results[i] = item
			if fetchFailed && failures.Add(1) >= episodeFailureBudget {
				cancelFan()
			}
		})
	}
	wg.Wait()
	return results, int(failures.Load())
}

// fetchSeriesItem fetches one series' episodes and builds its Item. When the
// fan-out context is cancelled or expired (a shutdown/redeploy, or the walk
// failure budget tripping) it returns (nil, false) without a warning (Walk
// reports the cancellation or the budget); any other episode-fetch failure -
// including a per-request timeout while the walk context is still live - is
// logged and returns the series' identity as a Failed placeholder plus
// failed=true, so the snapshot records WHICH items the partial walk is missing
// and walkSonarr can count the failure against the budget.
func (w *Walker) fetchSeriesItem(ctx context.Context, s *arrapi.Series) (*Item, bool) {
	eps, err := w.sonarr.GetEpisodes(ctx, s.ID)
	if err != nil {
		// Stay quiet only when the fan-out context itself is done (a shutdown, or
		// the failure budget already tripped): that error is expected and Walk
		// reports it. arrapi wraps each request in its own context.WithTimeout,
		// so a slow GetEpisodes surfaces as DeadlineExceeded while ctx is still
		// live - a real fetch failure worth the per-series warning below, not
		// shutdown noise.
		if ctx.Err() != nil {
			return nil, false
		}
		w.log.Warn("skipping series: episode fetch failed", "series", s.Title, "id", s.ID, "error", err)
		// seriesItem with no episodes yields the identity fields and no file
		// data - exactly the Failed placeholder shape.
		item := w.seriesItem(s, nil)
		item.Failed = true
		return &item, true
	}
	item := w.seriesItem(s, eps)
	return &item, false
}

// walkRadarr lists movies, applies tag filters, and builds an item per movie.
func (w *Walker) walkRadarr(ctx context.Context) ([]Item, error) {
	movies, err := w.radarr.GetMovies(ctx)
	if err != nil {
		return nil, err
	}
	includeIDs, excludeIDs, err := w.resolveTags(ctx, w.radarr.ResolveTagIDs)
	if err != nil {
		return nil, err
	}

	items := make([]Item, 0, len(movies))
	for i := range movies {
		if keepByTags(movies[i].Tags, includeIDs, excludeIDs) {
			items = append(items, w.movieItem(&movies[i]))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// resolveTags resolves the include and exclude tag labels to ID sets, logging
// any label that matched no tag. Any resolution failure aborts the walk (fail
// closed; see resolveOne).
func (w *Walker) resolveTags(ctx context.Context,
	resolve func(context.Context, ...string) (map[int]struct{}, []string, error),
) (includeIDs, excludeIDs map[int]struct{}, err error) {
	includeIDs, err = w.resolveOne(ctx, resolve, "arr_tags.include", w.includeTags)
	if err != nil {
		return nil, nil, err
	}
	excludeIDs, err = w.resolveOne(ctx, resolve, "arr_tags.exclude", w.excludeTags)
	if err != nil {
		return nil, nil, err
	}
	return includeIDs, excludeIDs, nil
}

// resolveOne resolves a single label set, logging unmatched labels. Any
// resolution error fails the walk (fail closed): silently disabling the filter
// would admit every item past the configured arr_tags scoping for the cycle -
// a mass-resolve / report-noise blast radius from one transient tag-endpoint
// failure - and silently emptying the library would be just as wrong. A
// tag-resolution failure is an ingest failure (the cycle is unhealthy), and a
// cancellation keeps its existing semantics: it propagates and Walk reports
// the shutdown.
func (w *Walker) resolveOne(ctx context.Context,
	resolve func(context.Context, ...string) (map[int]struct{}, []string, error),
	which string, labels []string,
) (map[int]struct{}, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	ids, unmatched, err := resolve(ctx, labels...)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", which, err)
	}
	if len(unmatched) > 0 {
		w.log.Warn("configured tags matched no arr tag", "which", which, "unmatched_count", len(unmatched))
	}
	return ids, nil
}

// keepByTags applies include-then-exclude tag filtering. Include (when set)
// requires a match; exclude (when set) rejects a match.
func keepByTags(itemTags []int, includeIDs, excludeIDs map[int]struct{}) bool {
	if includeIDs != nil && !arrapi.HasAnyTag(itemTags, includeIDs) {
		return false
	}
	if excludeIDs != nil && arrapi.HasAnyTag(itemTags, excludeIDs) {
		return false
	}
	return true
}

// seriesItem builds a library Item from a series and its episodes, aggregating
// the distinct release groups present and a representative fingerprint. A
// series with no episode files keeps the zero fingerprint (Current.Group ""),
// matching the fileless-movie shape.
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
		// fi.group is never empty: fileInfoFrom normalizes it via
		// release.NormalizeGroup, which falls back to NOGRP for group-less files.
		groupCounts[fi.group]++
		addSeasonGroup(seasonCounts, eps[i].SeasonNumber, fi.group)
	}
	item := Item{
		SeasonGroups: seasonGroups(seasonCounts),
		Groups:       sortedKeys(groupCounts),
		AltTitles:    altTitles(s.AlternateTitles),
		Arr:          ArrSonarr,
		Title:        s.Title,
		ImdbID:       s.ImdbID,
		ArrURL:       s.WebURL(w.sonarrURL),
		ArrID:        s.ID,
		TvdbID:       s.TvdbID,
		TmdbID:       s.TmdbID,
		Year:         s.Year,
		HasFile:      len(files) > 0,
	}
	if item.HasFile {
		// A genuinely fileless series carries no comparable fingerprint: the
		// zero Current (Group "") mirrors the fileless-movie shape, and the
		// compare/audit paths read file presence before any group. Only a
		// PRESENT file with an unparseable group falls back to NOGRP (via
		// release.NormalizeGroup in fileInfoFrom).
		rep := representative(files, groupCounts)
		item.Current = fingerprint(&rep)
	}
	return item
}

// movieItem builds a library Item from a movie and its file.
func (w *Walker) movieItem(m *arrapi.Movie) Item {
	item := Item{
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
		// fi.group is never empty: fileInfoFrom normalizes it via
		// release.NormalizeGroup, which falls back to NOGRP for group-less files.
		item.Groups = []string{fi.group}
		item.Current = fingerprint(&fi)
	}
	return item
}

// fingerprint classifies a library file into a release.Release using the shared
// classifier, so the library and SeaDex sides compare in one vocabulary.
func fingerprint(fi *fileInfo) release.Release {
	return release.Classify(&release.Input{
		Names:      nonEmpty(fi.sceneName, fi.relPath),
		Group:      fi.group,
		VideoCodec: fi.videoCodec,
		DualAudio:  isDualAudio(fi.audioLanguages),
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

// fileInfoFrom builds a fileInfo from the release-relevant
// fields common to a Sonarr episode file and a Radarr movie file.
func fileInfoFrom(group, sceneName, relPath string, mi *arrapi.MediaInfo) fileInfo {
	fi := fileInfo{
		group:     release.NormalizeGroup(group),
		sceneName: sceneName,
		relPath:   relPath,
	}
	if mi != nil {
		fi.videoCodec = mi.VideoCodec
		fi.audioLanguages = mi.AudioLanguages
	}
	return fi
}

// fileFromEpisode extracts fileInfo from a Sonarr episode file.
func fileFromEpisode(f *arrapi.EpisodeFile) fileInfo {
	return fileInfoFrom(f.ReleaseGroup, f.SceneName, f.RelativePath, f.MediaInfo)
}

// fileFromMovie extracts fileInfo from a Radarr movie file.
func fileFromMovie(f *arrapi.MovieFile) fileInfo {
	return fileInfoFrom(f.ReleaseGroup, f.SceneName, f.RelativePath, f.MediaInfo)
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

// sortedKeys returns the map keys sorted (nil when the map is empty), for a
// stable groups slice.
func sortedKeys(m map[string]int) []string {
	return slices.Sorted(maps.Keys(m))
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

// countsPresenceChange reports whether an item present in only one snapshot
// counts as added/removed under the partial-transition policy: a Sonarr item
// absent from a partial snapshot is suppressed (Partial is set only by Sonarr
// episode-fetch failures), while Radarr items always count. This is the
// blanket Sonarr suppression documented on DiffSnapshots, extracted verbatim
// so the policy is written once instead of inside both directional scans.
func countsPresenceChange(partial bool, item *Item) bool {
	return !partial || item.Arr != ArrSonarr
}

// DiffSnapshots reports what changed between prev and cur, keyed by arr + id.
// An item is Changed when its group set, per-season group attribution, or
// current fingerprint differs. Per the Snapshot.Partial contract, a Sonarr
// item absent from a partial snapshot is not treated as removed (partial cur)
// or added (partial prev); Partial is set only by Sonarr episode-fetch
// failures, so Radarr additions/removals still count during a partial
// transition, and changes to items present in both always count.
func DiffSnapshots(prev, cur *Snapshot) Diff {
	prevByKey := indexByKey(prev)
	curByKey := indexByKey(cur)
	var d Diff
	for k, c := range curByKey {
		p, ok := prevByKey[k]
		if !ok {
			if countsPresenceChange(prev.Partial, c) {
				d.Added++
			}
			continue
		}
		if !sameItem(p, c) {
			d.Changed++
		}
	}
	for k, p := range prevByKey {
		if _, ok := curByKey[k]; !ok && countsPresenceChange(cur.Partial, p) {
			d.Removed++
		}
	}
	return d
}

// indexByKey keys a snapshot's items by "arr:id" (values point into the
// snapshot's backing array, avoiding per-item copies). Failed placeholder
// items are skipped: they carry no comparable file state (they exist so the
// compare pass can scope finding resolution), so the diff treats them exactly
// as the pre-marking walks did - absent, with the Partial suppression rules
// deciding added/removed.
func indexByKey(s *Snapshot) map[string]*Item {
	m := make(map[string]*Item, len(s.Items))
	for i := range s.Items {
		it := &s.Items[i]
		if it.Failed {
			continue
		}
		m[it.Key()] = it
	}
	return m
}

// sameItem reports whether two items have the same current release state
// (file presence, group set, per-season group attribution, and fingerprint),
// for diff change detection.
func sameItem(a, b *Item) bool {
	return a.HasFile == b.HasFile && a.Current == b.Current &&
		slices.Equal(a.Groups, b.Groups) &&
		maps.EqualFunc(a.SeasonGroups, b.SeasonGroups, slices.Equal)
}

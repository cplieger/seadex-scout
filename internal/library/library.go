// Package library ingests the Sonarr/Radarr anime library through arrapi into a
// snapshot: per item its external IDs, tags, current release groups, and a
// representative release fingerprint. It applies arr-side tag include/exclude
// and can diff two snapshots to report what changed on the arr side.
//
// A per-series episode fetch failure is logged and the series kept as a
// Failed placeholder item (the snapshot is partial but usable); a failure of
// the top-level series/movie list, a tag-resolution failure, a cancelled
// context, per-series failures hitting the walk failure budget, or every
// kept series failing its episode fetch fail the
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
	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/runesafe"
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
// Because the budget is an absolute count, a library with fewer kept series
// can never trip it - walkSonarr's companion total-failure rule (every kept
// series failed) covers that gap, so a total episode-file-endpoint outage is
// an ingest failure at any library size.
const episodeFailureBudget = 5

// SonarrClient is the arrapi Sonarr surface the walker needs (consumer-side
// interface; *arrapi.Sonarr satisfies it). GetEpisodeFiles lists exactly the
// episodes that have a file on disk - the walker only consumes episodes WITH
// files, so it needs no episode rows to skip.
type SonarrClient interface {
	GetSeries(ctx context.Context) ([]arrapi.Series, error)
	GetEpisodeFiles(ctx context.Context, seriesID int) ([]arrapi.EpisodeFile, error)
	GetTags(ctx context.Context) ([]arrapi.Tag, error)
}

// RadarrClient is the arrapi Radarr surface the walker needs.
type RadarrClient interface {
	GetMovies(ctx context.Context) ([]arrapi.Movie, error)
	GetTags(ctx context.Context) ([]arrapi.Tag, error)
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
	// usable but not a complete library view. Consumers must not read a
	// Failed item's missing file state as a real library state. A published
	// walk never drops a failed series (it keeps the placeholder, or fails
	// the walk outright), so an item absent from Items is genuinely gone
	// from the arr, Partial or not.
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
// cancelled, per-series episode failures hit the walk failure budget, or
// every kept series' episode fetch failed (a total outage is an ingest
// failure at any library size); sub-budget, sub-total per-series failures are
// kept as Failed placeholder items with a warning (the snapshot is partial).
func (w *Walker) Walk(ctx context.Context) (Snapshot, error) {
	var items []Item
	partial := false

	if w.sonarr != nil {
		series, failed, err := w.walkSonarr(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("walking sonarr: %w", err)
		}
		items = append(items, series...)
		partial = failed > 0
	}
	if w.radarr != nil {
		movies, err := w.walkRadarr(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("walking radarr: %w", err)
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
// accumulate - or every kept series has failed, whichever a given library
// size can reach - the walk fails as a whole (an arr outage, not per-series
// blips), the budget additionally cancelling the remaining fan-out.
func (w *Walker) walkSonarr(ctx context.Context) ([]Item, int, error) {
	series, err := w.sonarr.GetSeries(ctx)
	if err != nil {
		return nil, 0, err
	}
	includeIDs, excludeIDs, err := w.resolveTags(ctx, w.sonarr.GetTags)
	if err != nil {
		return nil, 0, err
	}

	kept := filterSeriesByTags(series, includeIDs, excludeIDs)

	results, failed := w.fetchEpisodeItems(ctx, kept)
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if failed >= episodeFailureBudget {
		return nil, 0, fmt.Errorf("sonarr episode fetches: %d series failed, hitting the walk failure budget of %d", failed, episodeFailureBudget)
	}
	// Sub-budget total failure: every kept series' episode fetch failed. The
	// budget above is an absolute count a library with fewer kept series can
	// never reach, and publishing a "partial" snapshot with zero usable file
	// data would let the cycle read healthy through a total
	// episode-file-endpoint outage - an arr ingest failure whatever the
	// library size (a restart or config fix could recover it, the app's
	// unhealthy semantic). The ctx check above keeps a shutdown from
	// masquerading as a total failure.
	if len(kept) > 0 && failed == len(kept) {
		return nil, 0, fmt.Errorf("sonarr episode fetches: all %d kept series failed", failed)
	}
	items := make([]Item, 0, len(results))
	for _, item := range results {
		if item != nil {
			items = append(items, *item)
		}
	}
	if failed > 0 {
		// The attr keys ("skipped", "kept") and the "snapshot is partial"
		// message substring are pinned by the library tests and Loki queries;
		// only the misleading "series skipped" prefix changes.
		w.log.Warn("sonarr episode fetches failed; failed series kept as placeholders; snapshot is partial",
			"skipped", failed, "kept", len(kept))
	}
	return items, failed, nil
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

// fetchSeriesItem fetches one series' episode files and builds its Item. When
// the fan-out context is cancelled or expired (a shutdown/redeploy, or the walk
// failure budget tripping) it returns (nil, false) without a warning (Walk
// reports the cancellation or the budget); any other episode-fetch failure -
// including a per-request timeout while the walk context is still live - is
// logged and returns the series' identity as a Failed placeholder plus
// failed=true, so the snapshot records WHICH items the partial walk is missing
// and walkSonarr can count the failure against the budget.
func (w *Walker) fetchSeriesItem(ctx context.Context, s *arrapi.Series) (*Item, bool) {
	files, err := w.sonarr.GetEpisodeFiles(ctx, s.ID)
	if err != nil {
		// Stay quiet only when the fan-out context itself is done (a shutdown, or
		// the failure budget already tripped): that error is expected and Walk
		// reports it. arrapi wraps each request in its own context.WithTimeout,
		// so a slow GetEpisodeFiles surfaces as DeadlineExceeded while ctx is
		// still live - a real fetch failure worth the per-series warning below,
		// not shutdown noise.
		if ctx.Err() != nil {
			return nil, false
		}
		// LogSafeError strips any userinfo-bearing request URL the arr client's
		// wrapped *url.Error carries: this recoverable per-series warning sits
		// outside the walk-level LogSafeError boundary, so a configured
		// credential must be redacted here too before the line reaches Loki.
		w.log.Warn("sonarr episode fetch failed; series kept as failed placeholder", "series", runesafe.Sanitize(s.Title), "id", s.ID, "error", httpx.LogSafeError(err))
		// seriesItem with no files yields the identity fields and no file
		// data - exactly the Failed placeholder shape.
		item := w.seriesItem(s, nil)
		item.Failed = true
		return &item, true
	}
	item := w.seriesItem(s, files)
	return &item, false
}

// walkRadarr lists movies, applies tag filters, and builds an item per movie.
func (w *Walker) walkRadarr(ctx context.Context) ([]Item, error) {
	movies, err := w.radarr.GetMovies(ctx)
	if err != nil {
		return nil, err
	}
	includeIDs, excludeIDs, err := w.resolveTags(ctx, w.radarr.GetTags)
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

// resolveTags fetches the arr's tag list once per walk and resolves the
// include and exclude label sets against it locally (arrapi.TagIDs /
// UnmatchedLabels), logging any label that matched no tag. With neither set
// configured no fetch is issued. A tag-list fetch failure aborts the walk
// (fail closed): silently disabling the filter would admit every item past
// the configured arr_tags scoping for the cycle - a mass-resolve /
// report-noise blast radius from one transient tag-endpoint failure - and
// silently emptying the library would be just as wrong. A tag fetch failure
// is an ingest failure (the cycle is unhealthy), and a cancellation keeps its
// existing semantics: it propagates and Walk reports the shutdown.
func (w *Walker) resolveTags(ctx context.Context,
	getTags func(context.Context) ([]arrapi.Tag, error),
) (includeIDs, excludeIDs map[int]struct{}, err error) {
	if len(w.includeTags) == 0 && len(w.excludeTags) == 0 {
		return nil, nil, nil
	}
	tags, err := getTags(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving arr_tags: %w", err)
	}
	includeIDs = w.resolveOne(tags, "arr_tags.include", w.includeTags)
	excludeIDs = w.resolveOne(tags, "arr_tags.exclude", w.excludeTags)
	return includeIDs, excludeIDs, nil
}

// resolveOne resolves a single label set against an already-fetched tag list,
// logging a count-only warning for unmatched labels (values withheld: they
// pass through ${VAR} expansion and could carry a secret - see the
// credential-safety test). It returns nil for an unconfigured set (keepByTags
// reads nil as "filter off") and a non-nil - possibly empty - set for a
// configured one, so a configured include list matching no tag still drops
// everything rather than admitting everything.
func (w *Walker) resolveOne(tags []arrapi.Tag, which string, labels []string) map[int]struct{} {
	if len(labels) == 0 {
		return nil
	}
	if unmatched := arrapi.UnmatchedLabels(tags, labels...); len(unmatched) > 0 {
		w.log.Warn("configured tags matched no arr tag", "which", which, "unmatched_count", len(unmatched))
	}
	return arrapi.TagIDs(tags, labels...)
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

// seriesItem builds a library Item from a series and its episode files (as
// listed by GetEpisodeFiles: exactly the episodes with a file on disk, each
// carrying its own SeasonNumber), aggregating the distinct release groups
// present and a representative fingerprint. A series with no episode files
// keeps the zero fingerprint (Current.Group ""), matching the fileless-movie
// shape.
func (w *Walker) seriesItem(s *arrapi.Series, epFiles []arrapi.EpisodeFile) Item {
	files := make([]fileInfo, 0, len(epFiles))
	groupCounts := make(map[string]int)
	seasonCounts := make(map[int]map[string]int)
	for i := range epFiles {
		fi := fileFromEpisode(&epFiles[i])
		files = append(files, fi)
		// fi.group is never empty: fileInfoFrom normalizes it via
		// release.NormalizeGroup, which falls back to NOGRP for group-less files.
		groupCounts[fi.group]++
		addSeasonGroup(seasonCounts, epFiles[i].SeasonNumber, fi.group)
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

// DiffSnapshots reports what changed between prev and cur, keyed by arr + id.
// An item is Changed when its group set, per-season group attribution, or
// current fingerprint differs. Partial-walk suppression is scoped to the
// known-Failed keys: an item that is a Failed placeholder in cur is not
// counted as removed (its file state is missing, not gone), and an item that
// was a Failed placeholder in prev is not counted as added when it walks
// clean again (a recovery, not an arrival). An item genuinely absent from a
// snapshot diffs as added/removed even when that snapshot is Partial - a
// published walk keeps every failed series as a placeholder, so absence
// means the arr itself no longer lists the item. A key that debuts as a
// Failed placeholder, or that disappears from the arr while Failed, still
// counts as added/removed - a placeholder asserts arr presence even though
// its file state is unknown. A key that is Failed on
// either side never counts as Changed (there is no comparable file state to
// compare).
func DiffSnapshots(prev, cur *Snapshot) Diff {
	prevByKey, prevFailed := indexByKey(prev)
	curByKey, curFailed := indexByKey(cur)
	var d Diff
	for k, c := range curByKey {
		if p, ok := prevByKey[k]; ok && !sameItem(p, c) {
			d.Changed++
		}
	}
	d.Added = countAbsent(curByKey, curFailed, prevByKey, prevFailed)
	d.Removed = countAbsent(prevByKey, prevFailed, curByKey, curFailed)
	return d
}

// countAbsent counts keys present on the "from" side (its live index or
// its Failed placeholders) but absent from the "other" side entirely
// (neither live nor Failed). A key that debuts as - or disappears while -
// a Failed placeholder still asserts arr presence, so it counts as a
// genuine arrival/departure exactly once regardless of which side holds it.
func countAbsent(fromByKey map[string]*Item, fromFailed map[string]struct{},
	otherByKey map[string]*Item, otherFailed map[string]struct{},
) int {
	n := 0
	for k := range fromByKey {
		if !presentIn(k, otherByKey, otherFailed) {
			n++
		}
	}
	for k := range fromFailed {
		if !presentIn(k, otherByKey, otherFailed) {
			n++
		}
	}
	return n
}

// presentIn reports whether key k appears on a side, counting both its
// comparable index and its Failed placeholders.
func presentIn(k string, byKey map[string]*Item, failed map[string]struct{}) bool {
	if _, ok := byKey[k]; ok {
		return true
	}
	_, ok := failed[k]
	return ok
}

// indexByKey keys a snapshot's comparable items by "arr:id" (values point
// into the snapshot's backing array, avoiding per-item copies) and returns
// its Failed placeholders' keys separately: a Failed item carries no
// comparable file state (it exists so the compare pass and the diff can
// scope their handling to the keys the partial walk actually missed), so it
// joins the failed set instead of the comparable index. failed is nil when
// the snapshot has no placeholders (every complete walk).
func indexByKey(s *Snapshot) (byKey map[string]*Item, failed map[string]struct{}) {
	byKey = make(map[string]*Item, len(s.Items))
	for i := range s.Items {
		it := &s.Items[i]
		if it.Failed {
			if failed == nil {
				failed = make(map[string]struct{})
			}
			failed[it.Key()] = struct{}{}
			continue
		}
		byKey[it.Key()] = it
	}
	return byKey, failed
}

// sameItem reports whether two items have the same current release state
// (file presence, group set, per-season group attribution, and fingerprint),
// for diff change detection.
func sameItem(a, b *Item) bool {
	return a.HasFile == b.HasFile && a.Current == b.Current &&
		slices.Equal(a.Groups, b.Groups) &&
		maps.EqualFunc(a.SeasonGroups, b.SeasonGroups, slices.Equal)
}

// Package arrwalk ingests the Sonarr/Radarr anime library through arrapi into a
// library snapshot: per item its external IDs, tags, current release groups, and
// a representative release fingerprint. It applies arr-side tag include/exclude
// and owns every arr-wire concern of the ingest.
package arrwalk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/arrapi/v2"
	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/release"
)

// episodeConcurrency bounds concurrent per-series episode fetches.
const episodeConcurrency = 6

// episodeFailureBudget caps per-series episode-fetch failures per walk.
const episodeFailureBudget = 5

// --- Arr client surfaces ---

// SonarrClient is the arrapi Sonarr surface the walker needs (consumer-side
// interface; *arrapi.Sonarr satisfies it). EpisodeFiles lists exactly the
// episodes that have a file on disk - the walker only consumes episodes WITH
// files, so it needs no episode rows to skip.
type SonarrClient interface {
	Series(ctx context.Context) ([]arrapi.Series, error)
	EpisodeFiles(ctx context.Context, seriesID int) ([]arrapi.EpisodeFile, error)
	Tags(ctx context.Context) ([]arrapi.Tag, error)
}

// RadarrClient is the arrapi Radarr surface the walker needs.
type RadarrClient interface {
	Movies(ctx context.Context) ([]arrapi.Movie, error)
	Tags(ctx context.Context) ([]arrapi.Tag, error)
}

// --- Walker and walk flow ---

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

// sideResult is one arr side's contribution to a walk: its items plus the
// walk-quality facts only that side observes (the caller cannot re-derive
// either from the items alone).
type sideResult struct {
	items []library.Item
	// failedEpisodeFetches counts series whose episode fetch failed (Sonarr
	// only). Each is kept as a placeholder item, and any failure makes the
	// published snapshot partial.
	failedEpisodeFetches int
	// filteredEmpty reports that arr_tags filtering kept NOTHING out of a
	// non-empty arr list, so this side contributes zero items for a
	// configuration reason (a dead include set, or labels no item carries).
	filteredEmpty bool
}

// walkTotals folds every enabled side's contribution into the one snapshot a
// walk publishes. Both sides fold through add, so no arm can disagree with its
// sibling about whether a fact accumulates.
type walkTotals struct {
	items             []library.Item
	filteredEmptyArrs []string
	partial           bool
}

// add folds one arr side's result in: its items, plus the walk-quality facts
// only that side observes. filteredEmpty is recorded as the arr's NAME, because
// a consumer alerting on it has to know which arr to fix.
func (t *walkTotals) add(arr string, side sideResult) {
	t.items = append(t.items, side.items...)
	t.partial = t.partial || side.failedEpisodeFetches > 0
	if side.filteredEmpty {
		t.filteredEmptyArrs = append(t.filteredEmptyArrs, arr)
	}
}

// Walk ingests both arr sides into a single snapshot.
func (w *Walker) Walk(ctx context.Context) (library.Snapshot, error) {
	var totals walkTotals

	if w.sonarr != nil {
		side, err := w.walkSonarr(ctx)
		if err != nil {
			return library.Snapshot{}, &walkSideError{arr: library.ArrSonarr, err: err}
		}
		totals.add(library.ArrSonarr, side)
	}
	if w.radarr != nil {
		side, err := w.walkRadarr(ctx)
		if err != nil {
			return library.Snapshot{}, &walkSideError{arr: library.ArrRadarr, err: err}
		}
		totals.add(library.ArrRadarr, side)
	}

	// Final cancellation guard: when both sides are disabled (or the last side
	// returned just before cancellation), neither helper observed ctx, so an
	// already-cancelled walk must not publish a snapshot labelled complete.
	if err := ctx.Err(); err != nil {
		return library.Snapshot{}, err
	}

	w.log.Info("library walk complete", "items", len(totals.items), "partial", totals.partial,
		"sonarr", w.sonarr != nil, "radarr", w.radarr != nil)
	return library.Snapshot{
		TakenAt:           time.Now().UTC(),
		Items:             totals.items,
		FilteredEmptyArrs: totals.filteredEmptyArrs,
		Partial:           totals.partial,
	}, nil
}

// EnabledArrs reports the arr sides this walker ingests, in a stable order
// (Sonarr then Radarr). A side is enabled exactly when its client is wired,
// which is the same fact Walk branches on - so the walker stays the ONE home
// of "which arrs does this deployment have", and a consumer never re-derives
// it from a config pair the walker was built from.
func (w *Walker) EnabledArrs() []string {
	arrs := make([]string, 0, 2)
	if w.sonarr != nil {
		arrs = append(arrs, library.ArrSonarr)
	}
	if w.radarr != nil {
		arrs = append(arrs, library.ArrRadarr)
	}
	return arrs
}

// walkSideError wraps a per-side walk failure with the failed arr's identity.
// Error preserves the exact "walking <arr>: <cause>" text the previous plain
// fmt.Errorf wrapper produced (report-mode CLI output reads it unchanged) and
// Unwrap keeps the cause chain intact for errors.Is/As.
type walkSideError struct {
	err error
	arr string
}

func (e *walkSideError) Error() string { return "walking " + e.arr + ": " + e.err.Error() }

// Unwrap exposes the underlying cause so the chain stays visible to
// errors.Is/As (context-cancellation checks, LogSafeError's *url.Error search).
func (e *walkSideError) Unwrap() error { return e.err }

// WalkErrArr returns the arr identity (ArrSonarr or ArrRadarr) a Walk error
// carries for its failed side, or "" for an error that names no side (Walk's
// final cancellation guard, or any non-walk error).
func WalkErrArr(err error) string {
	if side, ok := errors.AsType[*walkSideError](err); ok {
		return side.arr
	}
	return ""
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
// series with its episode files fetched concurrently (bounded).
func (w *Walker) walkSonarr(ctx context.Context) (sideResult, error) {
	series, err := w.sonarr.Series(ctx)
	if err != nil {
		return sideResult{}, err
	}
	w.warnEmptyArrList(library.ArrSonarr, len(series))
	includeIDs, excludeIDs, err := w.resolveTags(ctx, w.sonarr.Tags)
	if err != nil {
		return sideResult{}, err
	}

	kept := filterSeriesByTags(series, includeIDs, excludeIDs)
	filteredEmpty := w.warnFilteredEmpty(library.ArrSonarr, len(series), len(kept), includeIDs != nil || excludeIDs != nil)

	results, failed := w.fetchEpisodeItems(ctx, kept)
	if err := episodeFetchError(ctx, len(kept), failed); err != nil {
		return sideResult{}, err
	}
	items := make([]library.Item, 0, len(results))
	for _, item := range results {
		if item != nil {
			items = append(items, *item)
		}
	}
	if failed > 0 {
		// The attr keys ("skipped", "kept") and the "snapshot is partial"
		// message substring are pinned by the walker tests and Loki queries:
		// rename them only together with those consumers.
		w.log.Warn("sonarr episode fetches failed; failed series kept as placeholders; snapshot is partial",
			"skipped", failed, "kept", len(kept))
	}
	return sideResult{items: items, failedEpisodeFetches: failed, filteredEmpty: filteredEmpty}, nil
}

// episodeFetchError applies the episode-fetch failure policy over one walk's
// fan-out result: a cancelled caller context, the absolute failure budget, and
// the sub-budget total failure. It returns nil when the walk may publish a
// (possibly partial) snapshot.
func episodeFetchError(ctx context.Context, kept, failed int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed >= episodeFailureBudget {
		return fmt.Errorf("episode fetches: %d of %d kept series failed, hitting the walk failure budget of %d", failed, kept, episodeFailureBudget)
	}
	// Sub-budget total failure: every kept series' episode fetch failed.
	if kept > 0 && failed == kept {
		return fmt.Errorf("episode fetches: all %d kept series failed", failed)
	}
	return nil
}

// fetchEpisodeItems runs the bounded episode-fetch fan-out over the kept
// series, returning the per-series results (nil where a fetch was cancelled or
// skipped) and the failure count.
func (w *Walker) fetchEpisodeItems(ctx context.Context, kept []arrapi.Series) (results []*library.Item, failed int) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()
	var failures atomic.Int64
	results = make([]*library.Item, len(kept))
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

// fetchSeriesItem fetches one series' episode files and builds its Item.
func (w *Walker) fetchSeriesItem(ctx context.Context, s *arrapi.Series) (*library.Item, bool) {
	files, err := w.sonarr.EpisodeFiles(ctx, s.ID)
	if err != nil {
		// Stay quiet only when the fan-out context itself is done (a shutdown, or
		// the failure budget already tripped): that error is expected and Walk
		// reports it.
		if ctx.Err() != nil {
			return nil, false
		}
		// LogSafeError strips any userinfo-bearing request URL the arr client's
		// wrapped *url.Error carries: this recoverable per-series warning sits
		// outside the walk-level LogSafeError boundary, so a configured
		// credential must be redacted here too before the line reaches Loki.
		w.log.Warn("sonarr episode fetch failed; series kept as failed placeholder", "series", logattr.Cap(s.Title), "id", s.ID, "error", httpx.LogSafeError(err))
		// seriesItem with no files yields the identity fields and no file
		// data - exactly the Failed placeholder shape.
		item := w.seriesItem(s, nil)
		item.Failed = true
		return &item, true
	}
	item := w.seriesItem(s, files)
	// Sonarr's series list declared episode files for this series but its
	// episode-file list came back empty, so the item necessarily compares as
	// fileless (seriesItem's HasFile is len(files) > 0) - which reads
	// downstream as a genuine no-file library state: the daemon falls silent
	// and resolves the series' prior finding, and the report renders verdict
	// no_file. Record the degradation instead of letting it look like a
	// genuinely fileless series. Unlike walkRadarr's no-file-payload sibling
	// this stays a WARN rather than a Failed placeholder: there, HasFile and
	// the absent MovieFile arrive in ONE response and contradict each other, so
	// the file data is certainly missing; here the two facts come from
	// different responses (the series list once at the top of the walk, the
	// episode files per series later), so a whole series' files legitimately
	// deleted mid-walk lands here too and a placeholder would suppress a real
	// no-file state.
	if s.Statistics != nil && s.Statistics.EpisodeFileCount > 0 && len(files) == 0 {
		w.log.Warn("sonarr series declares episode files but its episode-file list came back empty; it compares as fileless",
			"series", logattr.Cap(s.Title), "id", s.ID, "declared_files", s.Statistics.EpisodeFileCount)
	}
	return &item, false
}

// walkRadarr lists movies, applies tag filters, and builds an item per movie. A
// movie Radarr reports as having a file but sends no file payload for is kept
// as a library placeholder (Failed set): its file state is MISSING, not empty,
// so the compare and the diff must scope it out exactly as they scope out a
// series whose episode fetch failed - see the placeholder note below.
func (w *Walker) walkRadarr(ctx context.Context) (sideResult, error) {
	movies, err := w.radarr.Movies(ctx)
	if err != nil {
		return sideResult{}, err
	}
	w.warnEmptyArrList(library.ArrRadarr, len(movies))
	includeIDs, excludeIDs, err := w.resolveTags(ctx, w.radarr.Tags)
	if err != nil {
		return sideResult{}, err
	}

	items := make([]library.Item, 0, len(movies))
	noPayload := 0
	for i := range movies {
		if !keepByTags(movies[i].Tags, includeIDs, excludeIDs) {
			continue
		}
		item := w.movieItem(&movies[i])
		if movies[i].HasFile && movies[i].MovieFile == nil {
			// Radarr says the movie has a file but sent no file payload, so the
			// item carries no comparable file state (movieItem's HasFile AND is
			// false and it has no groups).
			item.Failed = true
			noPayload++
			w.log.Debug("radarr movie reports a file but carries no file payload",
				"movie", logattr.Cap(movies[i].Title), "id", movies[i].ID)
		}
		items = append(items, item)
	}
	if noPayload > 0 {
		w.log.Warn("radarr movies report a file but carry no file payload; they are kept as placeholders with no comparable file state - re-scan those movies in Radarr (the per-movie ids are logged at debug level)",
			"movies", noPayload, "kept", len(items))
	}
	filteredEmpty := w.warnFilteredEmpty(library.ArrRadarr, len(movies), len(items), includeIDs != nil || excludeIDs != nil)
	if err := ctx.Err(); err != nil {
		return sideResult{}, err
	}
	return sideResult{items: items, filteredEmpty: filteredEmpty}, nil
}

// --- Tag resolution and filtering ---

// resolveTags fetches the arr's tag list once per walk and resolves the
// include and exclude label sets against it locally (arrapi.TagIDs /
// UnmatchedLabels), logging any label that matched no tag. With neither set
// configured no fetch is issued.
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
// credential-safety test).
func (w *Walker) resolveOne(tags []arrapi.Tag, which string, labels []string) map[int]struct{} {
	if len(labels) == 0 {
		return nil
	}
	if unmatched := arrapi.UnmatchedLabels(tags, labels...); len(unmatched) > 0 {
		w.log.Warn("configured tags matched no arr tag", "which", which, "unmatched_count", len(unmatched))
	}
	ids := arrapi.TagIDs(tags, labels...)
	if ids == nil {
		// Fail closed independently of arrapi's zero-match return shape: the
		// non-nil EMPTY set is what keepByTags reads as "filter on, nothing
		// matches".
		ids = map[int]struct{}{}
	}
	if len(ids) == 0 {
		// Every configured label missed.
		w.log.Warn("no configured tag resolved to an arr tag; an include set therefore admits nothing, an exclude set drops nothing",
			"which", which, "configured_count", len(labels))
	}
	return ids
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

// warnEmptyArrList warns when an enabled arr's own list call succeeded but
// returned nothing at all.
func (w *Walker) warnEmptyArrList(arr string, listed int) {
	if listed > 0 {
		return
	}
	w.log.Warn("arr listed no items; this side contributes nothing this cycle - check the arr url and that the instance holds the expected library",
		"arr", arr)
}

// warnFilteredEmpty warns when tag filtering kept nothing out of a non-empty
// arr list, and reports whether it did.
func (w *Walker) warnFilteredEmpty(arr string, listed, kept int, filtered bool) bool {
	if !filtered || listed == 0 || kept > 0 {
		return false
	}
	w.log.Warn("arr_tags filtering kept no items from a non-empty arr library; this side contributes nothing this cycle",
		"arr", arr, "listed", listed)
	return true
}

// --- Item construction and fingerprinting ---

// seriesItem builds a library Item from a series and its episode files (as
// listed by EpisodeFiles: exactly the episodes with a file on disk, each
// carrying its own SeasonNumber), aggregating the distinct release groups
// present and a representative fingerprint.
func (w *Walker) seriesItem(s *arrapi.Series, epFiles []arrapi.EpisodeFile) library.Item {
	files := make([]fileInfo, 0, len(epFiles))
	groupCounts := make(map[string]int)
	seasonCounts := make(map[int]map[string]int)
	for i := range epFiles {
		fi := fileFromEpisode(&epFiles[i])
		files = append(files, fi)
		// fi.group is never empty: fileInfoFrom normalizes it via
		// release.NormalizeGroup, which falls back to the LOWERCASED NOGRP
		// sentinel ("nogrp") for group-less files.
		groupCounts[fi.group]++
		addSeasonGroup(seasonCounts, epFiles[i].SeasonNumber, fi.group)
	}
	item := library.Item{
		SeasonGroups: seasonGroups(seasonCounts),
		Groups:       sortedKeys(groupCounts),
		AltTitles:    altTitles(s.AlternateTitles),
		Arr:          library.ArrSonarr,
		Title:        s.Title,
		ImdbID:       s.ImdbID,
		ArrURL:       library.SafeLogURL(s.WebURL(w.sonarrURL)),
		ArrID:        s.ID,
		TvdbID:       s.TvdbID,
		TmdbID:       s.TmdbID,
		Year:         s.Year,
		HasFile:      len(files) > 0,
	}
	if item.HasFile {
		// A genuinely fileless series carries no comparable fingerprint: the
		// zero Current (Group "") mirrors the fileless-movie shape, and the
		// compare/audit paths read file presence before any group.
		rep := representative(files, groupCounts)
		item.Current = fingerprint(&rep)
	}
	return item
}

// movieItem builds a library Item from a movie and its file.
func (w *Walker) movieItem(m *arrapi.Movie) library.Item {
	item := library.Item{
		AltTitles: altTitles(m.AlternateTitles),
		Arr:       library.ArrRadarr,
		Title:     m.Title,
		ImdbID:    m.ImdbID,
		ArrURL:    library.SafeLogURL(m.WebURL(w.radarrURL)),
		ArrID:     m.ID,
		TmdbID:    m.TmdbID,
		Year:      m.Year,
		HasFile:   m.HasFile && m.MovieFile != nil,
	}
	if item.HasFile {
		fi := fileFromMovie(m.MovieFile)
		// fi.group is never empty: fileInfoFrom normalizes it via
		// release.NormalizeGroup, which falls back to the LOWERCASED NOGRP
		// sentinel ("nogrp") for group-less files.
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
	// Stream the tokens instead of materializing them: AudioLanguages is
	// arr-supplied and arrapi admits a 64 MiB list body, so the slice of
	// substring headers plus a map pre-sized to that token count amplifies one
	// hostile MediaInfo field far past the 256 MiB container budget (CWE-400).
	distinct := make(map[string]struct{}, 2)
	for f := range strings.FieldsFuncSeq(langs, func(r rune) bool { return r == '/' || r == ',' }) {
		if f = strings.TrimSpace(strings.ToLower(f)); f != "" {
			distinct[f] = struct{}{}
			if len(distinct) > 1 {
				return true
			}
		}
	}
	return false
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

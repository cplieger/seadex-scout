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

	"github.com/cplieger/arrapi/v3"
	"github.com/cplieger/arrapi/v3/radarr"
	"github.com/cplieger/arrapi/v3/sonarr"
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

// SonarrPass is the arrapi Sonarr surface one walk reads (consumer-side
// interface; *sonarr.Pinned satisfies it). EpisodeFiles lists exactly the
// files on disk, which every group and revision reading comes from;
// Episodes names the file each episode holds by FileID, which only the
// per-episode comparisons (season 0 and a positive season's listing) and the
// size readings need.
type SonarrPass interface {
	Series(ctx context.Context) ([]sonarr.Series, error)
	EpisodeFiles(ctx context.Context, seriesID int) ([]sonarr.EpisodeFile, error)
	Episodes(ctx context.Context, seriesID int) ([]sonarr.Episode, error)
	Tags(ctx context.Context) ([]arrapi.Tag, error)
}

var _ SonarrPass = (*sonarr.Pinned)(nil)

// SonarrClient opens the pass of one walk. A pass joins each series' order and
// AniList ids with its episodes, so all of its reads must come from one Sonarr
// version; FromSonarr's passes are arrapi pins.
type SonarrClient interface {
	Pin(ctx context.Context) (SonarrPass, error)
}

// FromSonarr adapts c to SonarrClient: every walk reads through one c.Pin.
func FromSonarr(c *sonarr.Client) SonarrClient { return sonarrPins{c} }

type sonarrPins struct{ c *sonarr.Client }

func (s sonarrPins) Pin(ctx context.Context) (SonarrPass, error) {
	p, err := s.c.Pin(ctx)
	if err != nil {
		return nil, err // a nil *sonarr.Pinned would be a non-nil SonarrPass
	}
	return p, nil
}

const specialsSeason = 0

// RadarrClient is the arrapi Radarr surface the walker needs.
type RadarrClient interface {
	Movies(ctx context.Context) ([]radarr.Movie, error)
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
// any of them from the items alone).
type sideResult struct {
	items []library.Item
	// failedEpisodeFetches counts series whose episode fetch failed (Sonarr
	// only). Each is kept as a placeholder item, and any failure makes the
	// published snapshot partial.
	failedEpisodeFetches int
	// nonAiredOrder counts the kept series in a non-aired episode order (Sonarr
	// only), which no comparison reads.
	nonAiredOrder int
	// readFailed reports that a read of this side returned an error, though
	// the side still published (Sonarr only).
	readFailed bool
	// filteredEmpty reports that arr_tags filtering kept NOTHING out of a
	// non-empty arr list, so this side contributes zero items for a
	// configuration reason (a dead include set, or labels no item carries).
	filteredEmpty bool
}

// walkTotals folds every enabled side's items and walk-quality facts into the
// one snapshot a walk publishes. Both sides fold through add, so no arm can
// disagree with its sibling about whether a fact accumulates.
type walkTotals struct {
	items             []library.Item
	filteredEmptyArrs []string
	nonAiredOrder     int
	partial           bool
}

// add folds one arr side's result in: its items, plus the walk-quality facts
// only that side observes. filteredEmpty is recorded as the arr's NAME, because
// a consumer alerting on it has to know which arr to fix.
func (t *walkTotals) add(arr string, side *sideResult) {
	t.items = append(t.items, side.items...)
	t.partial = t.partial || side.failedEpisodeFetches > 0
	t.nonAiredOrder += side.nonAiredOrder
	if side.filteredEmpty {
		t.filteredEmptyArrs = append(t.filteredEmptyArrs, arr)
	}
}

// SonarrOrders is what one walk's Sonarr series list, read in full, shows of
// episode orders: the positive TVDB ids of every listed series in a non-aired
// and in the aired order, ascending, whether or not arr_tags keeps it (the
// feed serves every series to Sonarr). The zero value is a walk that read no
// list. A list shows nothing of a series it does not name, so absence never
// removes an id; only a clean walk's aired orders release one.
type SonarrOrders struct {
	NonAired []int
	Aired    []int
	// Clean reports that the walk returned no error and none of its reads
	// failed.
	Clean bool
}

// Joined returns, ascending, prior plus o's non-aired-order ids.
func (o *SonarrOrders) Joined(prior []int) []int {
	set := idSet(prior)
	for _, id := range o.NonAired {
		set[id] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// Released returns Joined less each id only an aired-order series lists.
func (o *SonarrOrders) Released(prior []int) []int {
	set := idSet(prior)
	for _, id := range o.Aired {
		delete(set, id)
	}
	for _, id := range o.NonAired {
		set[id] = true
	}
	return slices.Sorted(maps.Keys(set))
}

func idSet(ids []int) map[int]bool {
	set := make(map[int]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// listedOrders sorts the list's series with a positive TVDB id by order.
func listedOrders(series []sonarr.Series) SonarrOrders {
	nonAired, aired := map[int]bool{}, map[int]bool{}
	for i := range series {
		switch id := series[i].IDs.TVDB; {
		case id <= 0:
		case series[i].Order.Official:
			aired[id] = true
		default:
			nonAired[id] = true
		}
	}
	return SonarrOrders{NonAired: slices.Sorted(maps.Keys(nonAired)), Aired: slices.Sorted(maps.Keys(aired))}
}

// Walk ingests both arr sides into a single snapshot, and returns what the
// walk's Sonarr series list showed of episode orders once that list is read,
// whatever fails after it. A failed walk's snapshot is the zero Snapshot.
func (w *Walker) Walk(ctx context.Context) (library.Snapshot, SonarrOrders, error) {
	var orders SonarrOrders
	var totals walkTotals
	readFailed := false
	if w.sonarr != nil {
		pass, series, err := w.listSonarr(ctx)
		if err != nil {
			return library.Snapshot{}, orders, &walkSideError{arr: library.ArrSonarr, err: err}
		}
		orders = listedOrders(series)
		side, err := w.walkSonarr(ctx, pass, series)
		if err != nil {
			return library.Snapshot{}, orders, &walkSideError{arr: library.ArrSonarr, err: err}
		}
		readFailed = side.readFailed
		totals.add(library.ArrSonarr, &side)
	}
	if w.radarr != nil {
		side, err := w.walkRadarr(ctx)
		if err != nil {
			return library.Snapshot{}, orders, &walkSideError{arr: library.ArrRadarr, err: err}
		}
		totals.add(library.ArrRadarr, &side)
	}

	// Final cancellation guard: when both sides are disabled (or the last side
	// returned just before cancellation), neither helper observed ctx, so an
	// already-cancelled walk must not publish a snapshot labelled complete.
	if err := ctx.Err(); err != nil {
		return library.Snapshot{}, orders, err
	}
	orders.Clean = !readFailed

	w.log.Info("library walk complete", "items", len(totals.items), "partial", totals.partial,
		"sonarr", w.sonarr != nil, "radarr", w.radarr != nil, "non_aired_order", totals.nonAiredOrder)
	return library.Snapshot{
		TakenAt:           time.Now().UTC(),
		Items:             totals.items,
		FilteredEmptyArrs: totals.filteredEmptyArrs,
		Partial:           totals.partial,
	}, orders, nil
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
func filterSeriesByTags(series []sonarr.Series, includeIDs, excludeIDs map[int]struct{}) []sonarr.Series {
	kept := make([]sonarr.Series, 0, len(series))
	for i := range series {
		if keepByTags(series[i].Tags, includeIDs, excludeIDs) {
			kept = append(kept, series[i])
		}
	}
	return kept
}

// listSonarr opens the walk's pass and reads its series list.
func (w *Walker) listSonarr(ctx context.Context) (SonarrPass, []sonarr.Series, error) {
	pass, err := w.sonarr.Pin(ctx)
	if err != nil {
		return nil, nil, err
	}
	series, err := pass.Series(ctx)
	if err != nil {
		return nil, nil, err
	}
	return pass, series, nil
}

// walkSonarr applies tag filters to the listed series and builds an item per
// kept series with its episode files fetched concurrently (bounded), every
// read through pass.
func (w *Walker) walkSonarr(ctx context.Context, pass SonarrPass, series []sonarr.Series) (sideResult, error) {
	w.warnEmptyArrList(library.ArrSonarr, len(series))
	includeIDs, excludeIDs, err := w.resolveTags(ctx, pass.Tags)
	if err != nil {
		return sideResult{}, err
	}

	kept := filterSeriesByTags(series, includeIDs, excludeIDs)
	filteredEmpty := w.warnFilteredEmpty(library.ArrSonarr, len(series), len(kept), includeIDs != nil || excludeIDs != nil)
	nonAired := w.logNonAiredOrders(kept)

	fan := w.fetchEpisodeItems(ctx, pass, kept)
	failed := fan.failed
	if err := episodeFetchError(ctx, len(kept), failed); err != nil {
		return sideResult{}, err
	}
	if fan.changed != nil {
		return sideResult{}, fmt.Errorf("sonarr changed version during the walk; the next walk reads it again: %w", fan.changed)
	}
	items := make([]library.Item, 0, len(fan.results))
	for _, item := range fan.results {
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
	return sideResult{items: items, failedEpisodeFetches: failed, filteredEmpty: filteredEmpty, nonAiredOrder: nonAired, readFailed: fan.readFailed}, nil
}

// logNonAiredOrders logs one Info line per kept series in a non-aired episode
// order and returns their count. Info, not Warn: the order is the operator's
// own Sonarr setting, and the walk's completion line carries the count.
func (w *Walker) logNonAiredOrders(kept []sonarr.Series) int {
	n := 0
	for i := range kept {
		s := &kept[i]
		if s.Order.Official {
			continue
		}
		n++
		order := library.Order{Type: s.Order.Type, Name: s.Order.Name}
		w.log.Info("sonarr series uses a non-aired episode order; seadex-scout does not compare it",
			"series", logattr.Cap(s.Title), "id", s.ID, "order", logattr.Cap(order.Label()), "order_type", logattr.Cap(s.Order.Type))
	}
	return n
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

// episodeFanOut is the result of one walk's episode-fetch fan-out.
type episodeFanOut struct {
	// changed is the read error that stopped the fan-out because Sonarr
	// answered as another version.
	changed error
	// results holds each kept series' item, nil where its fetch was cancelled
	// or skipped.
	results    []*library.Item
	failed     int
	readFailed bool
}

// fetchEpisodeItems runs the bounded episode-fetch fan-out over the kept
// series.
func (w *Walker) fetchEpisodeItems(ctx context.Context, pass SonarrPass, kept []sonarr.Series) episodeFanOut {
	fanCtx, cancelFan := context.WithCancelCause(ctx)
	defer cancelFan(nil)
	watched := &watchedPass{SonarrPass: pass, stop: cancelFan}
	pass = watched
	var failures atomic.Int64
	results := make([]*library.Item, len(kept))
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
			item, fetchFailed := w.fetchSeriesItem(fanCtx, pass, &kept[i])
			results[i] = item
			if fetchFailed && failures.Add(1) >= episodeFailureBudget {
				cancelFan(nil)
			}
		})
	}
	wg.Wait()
	fan := episodeFanOut{results: results, failed: int(failures.Load()), readFailed: watched.failed.Load()}
	if cause := context.Cause(fanCtx); versionChanged(cause) {
		fan.changed = cause
	}
	return fan
}

// watchedPass records that a fan-out read failed, and cancels the fan-out with
// a read's error when it names another Sonarr version, so a pass that met
// another version stops reading and does not publish.
type watchedPass struct {
	SonarrPass
	stop   context.CancelCauseFunc
	failed atomic.Bool
}

func (p *watchedPass) EpisodeFiles(ctx context.Context, seriesID int) ([]sonarr.EpisodeFile, error) {
	files, err := p.SonarrPass.EpisodeFiles(ctx, seriesID)
	p.check(err)
	return files, err
}

func (p *watchedPass) Episodes(ctx context.Context, seriesID int) ([]sonarr.Episode, error) {
	episodes, err := p.SonarrPass.Episodes(ctx, seriesID)
	p.check(err)
	return episodes, err
}

func (p *watchedPass) check(err error) {
	if err == nil {
		return
	}
	p.failed.Store(true)
	if versionChanged(err) {
		p.stop(err)
	}
}

// versionChanged reports an arrapi error naming another server version. One
// with To "" only leaves the version unconfirmed, and the pass still reads that
// version, so it is an ordinary read failure.
func versionChanged(err error) bool {
	e, ok := errors.AsType[*arrapi.ServerChangedError](err)
	return ok && e.To != ""
}

// fetchSeriesItem fetches one series' episode files and builds its Item.
func (w *Walker) fetchSeriesItem(ctx context.Context, pass SonarrPass, s *sonarr.Series) (*library.Item, bool) {
	files, err := pass.EpisodeFiles(ctx, s.ID)
	if err != nil {
		// Stay quiet only when the fan-out context itself is done (a shutdown, or
		// the failure budget already tripped): that error is expected and Walk
		// reports it.
		if ctx.Err() != nil {
			return nil, false
		}
		// arrapi's errors carry no request URL; LogSafeError is defence in depth,
		// as at the walk-level boundary.
		w.log.Warn("sonarr episode fetch failed; series kept as failed placeholder", "series", logattr.Cap(s.Title), "id", s.ID, "error", httpx.LogSafeError(err))
		// seriesItem with no files yields the identity fields and no file
		// data - exactly the Failed placeholder shape.
		item := w.seriesItem(s, nil)
		item.Failed = true
		return &item, true
	}
	item := w.seriesItem(s, files)
	w.addEpisodes(ctx, pass, s, files, &item)
	// A declared-but-empty episode list makes the item compare as genuinely
	// fileless (seriesItem's HasFile is len(files) > 0), so record the
	// degradation rather than let it look like a real no-file series. Stays a
	// WARN, not a Failed placeholder: unlike walkRadarr's sibling, the two
	// facts come from separate responses, so a legitimate mid-walk deletion
	// can also produce this shape and a placeholder would suppress it.
	if s.FileCount > 0 && len(files) == 0 {
		w.log.Warn("sonarr series declares episode files but its episode-file list came back empty; it compares as fileless",
			"series", logattr.Cap(s.Title), "id", s.ID, "declared_files", s.FileCount)
	}
	return &item, false
}

// addEpisodes reads the episode list of s and joins each episode to files, the
// series' file list: the input of its upgrade sizes, its positive seasons'
// listing and, for a series holding a season-0 file or naming one files lacks,
// its Specials. A failed read leaves them unknown rather than the series
// failed, because its groups and revisions come from the file list.
func (w *Walker) addEpisodes(ctx context.Context, pass SonarrPass, s *sonarr.Series, files []sonarr.EpisodeFile, item *library.Item) {
	holdsSpecials := slices.ContainsFunc(files, func(f sonarr.EpisodeFile) bool { return f.Season == specialsSeason })
	episodes, err := pass.Episodes(ctx, s.ID)
	switch {
	case err != nil && ctx.Err() != nil:
		return
	case err != nil && holdsSpecials:
		w.log.Warn("sonarr episode list read failed; this series' upgrade sizes and episode listing are unknown and its films and specials stay uncompared",
			"series", logattr.Cap(s.Title), "id", s.ID, "error", httpx.LogSafeError(err))
		return
	case err != nil:
		w.log.Warn("sonarr episode list read failed; this series' upgrade sizes and episode listing are unknown",
			"series", logattr.Cap(s.Title), "id", s.ID, "error", httpx.LogSafeError(err))
		return
	}
	byID := filesByID(files)
	seasonEpisodes, placed := episodeFiles(episodes, byID)
	if !placed {
		w.log.Warn("sonarr episode list marks a file it does not place; this series' upgrade sizes are unknown",
			"series", logattr.Cap(s.Title), "id", s.ID)
	}
	item.SeasonEpisodes = seasonEpisodes
	item.Listed = listedEpisodes(episodes, byID)
	specials, known := specialEpisodes(episodes, byID)
	item.SpecialsFileUnknown = !known
	if holdsSpecials || !known {
		item.Specials = w.specials(s, specials, known)
	}
}

// filesByID indexes a series' file list by file id for the episode join. A
// listed file no episode names, such as one part of an episode split across
// files, is in no episode's sizes: Sonarr's upgrade replaces only the
// files linked to the episodes it imports.
func filesByID(files []sonarr.EpisodeFile) map[int]*sonarr.EpisodeFile {
	out := make(map[int]*sonarr.EpisodeFile, len(files))
	for i := range files {
		out[files[i].ID] = &files[i]
	}
	return out
}

// episodeFile is the file ep holds: nil with ok true when it holds none, and
// ok false when its file state is unknown, a file with no id or an id files
// lacks. The file list is read first and Sonarr clears an episode's file id
// when it deletes the file, so a file imported (an upgrade's replacement
// included) after the file list was read gives the second.
func episodeFile(ep *sonarr.Episode, files map[int]*sonarr.EpisodeFile) (f *sonarr.EpisodeFile, ok bool) {
	switch {
	case ep.FileID > 0:
		f, ok = files[ep.FileID]
		return f, ok
	case ep.HasFile:
		return nil, false
	default:
		return nil, true
	}
}

// specials returns specialEpisodes' result for the series, with a WARN when
// it is unknown or names no season-0 episode beside a season-0 file.
func (w *Walker) specials(s *sonarr.Series, specials map[int]library.ListedEpisode, ok bool) map[int]library.ListedEpisode {
	switch {
	case !ok:
		w.log.Warn("sonarr specials episode list marks a file it does not send; the series' films and specials stay uncompared",
			"series", logattr.Cap(s.Title), "id", s.ID)
	case specials == nil:
		w.log.Warn("sonarr specials episode list names no season-0 episode beside a season-0 file; the series' films and specials stay uncompared",
			"series", logattr.Cap(s.Title), "id", s.ID)
	}
	return specials
}

// specialEpisodes maps each listed season-0 episode to its file in files. A
// file spanning several episodes is named by each. ok is false, and the map
// nil, when an episode's file state is unknown (episodeFile), not absent.
func specialEpisodes(eps []sonarr.Episode, files map[int]*sonarr.EpisodeFile) (specials map[int]library.ListedEpisode, ok bool) {
	out := make(map[int]library.ListedEpisode, len(eps))
	for i := range eps {
		ep := &eps[i]
		if ep.Season != specialsSeason || ep.Number <= 0 {
			continue
		}
		f, known := episodeFile(ep, files)
		if !known {
			return nil, false
		}
		out[ep.Number] = listedEpisode(f)
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

// listedEpisode is an episode's listing entry for f, the zero entry when the
// episode holds no file.
func listedEpisode(f *sonarr.EpisodeFile) library.ListedEpisode {
	if f == nil {
		return library.ListedEpisode{}
	}
	fi := fileFromEpisode(f)
	return library.ListedEpisode{Group: fi.group, Revision: fi.revision, HasFile: true}
}

// listedEpisodes maps each positive season to every episode Sonarr lists in
// it and its file in files, nil when there are none. A season holding an
// episode whose file state is unknown (episodeFile) is left out, as
// specialEpisodes refuses one.
func listedEpisodes(eps []sonarr.Episode, files map[int]*sonarr.EpisodeFile) map[int]map[int]library.ListedEpisode {
	out := make(map[int]map[int]library.ListedEpisode)
	unknown := make(map[int]bool)
	for i := range eps {
		ep := &eps[i]
		if ep.Season <= specialsSeason || ep.Number <= 0 {
			continue
		}
		f, known := episodeFile(ep, files)
		if !known {
			unknown[ep.Season] = true
			continue
		}
		if out[ep.Season] == nil {
			out[ep.Season] = make(map[int]library.ListedEpisode)
		}
		out[ep.Season][ep.Number] = listedEpisode(f)
	}
	for season := range unknown {
		delete(out, season)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// episodeFiles maps each season to its episodes that have a file in files. ok
// is false, and the map nil, when an episode's file state is unknown
// (episodeFile) or its file sits on no episode number: that file may be one a
// download covers, so counting it replaced could drop the episode.
func episodeFiles(episodes []sonarr.Episode, files map[int]*sonarr.EpisodeFile) (seasons map[int]map[int]library.Episode, ok bool) {
	out := make(map[int]map[int]library.Episode)
	for i := range episodes {
		ep := &episodes[i]
		f, known := episodeFile(ep, files)
		switch {
		case !known:
			return nil, false
		case f == nil:
			continue
		case ep.Number <= 0:
			return nil, false
		}
		if out[ep.Season] == nil {
			out[ep.Season] = make(map[int]library.Episode)
		}
		out[ep.Season][ep.Number] = library.Episode{File: f.ID, Absolute: max(ep.Absolute, 0)}
	}
	return out, true
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
		if movies[i].HasFile && movies[i].File == nil {
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
// include and exclude label sets against it locally. With neither set
// configured no fetch is issued. A label matching no arr tag is not reported;
// warnFilteredEmpty reads the outcome instead, from arrapi.TagIDs' non-nil
// empty set for labels that match nothing.
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
	return arrapi.TagIDs(tags, w.includeTags...), arrapi.TagIDs(tags, w.excludeTags...), nil
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

// seriesItem builds a library Item from a series and its EpisodeFile rows,
// each carrying its own Season (one row can hold several episodes), and
// aggregates the distinct release groups present and a representative
// fingerprint.
func (w *Walker) seriesItem(s *sonarr.Series, epFiles []sonarr.EpisodeFile) library.Item {
	files := make([]fileInfo, 0, len(epFiles))
	groupCounts := make(map[string]int)
	seasonCounts := make(map[int]map[string]int)
	var revs revisionFold
	var fileBytes map[int]int64
	for i := range epFiles {
		if epFiles[i].ID > 0 {
			if fileBytes == nil {
				fileBytes = make(map[int]int64, len(epFiles))
			}
			fileBytes[epFiles[i].ID] = epFiles[i].Size
		}
		fi := fileFromEpisode(&epFiles[i])
		files = append(files, fi)
		// fi.group is never empty: fileInfoFrom normalizes it via
		// release.NormalizeGroup, which falls back to the LOWERCASED NOGRP
		// sentinel ("nogrp") for group-less files.
		groupCounts[fi.group]++
		addSeasonGroup(seasonCounts, epFiles[i].Season, fi.group)
		revs.add(epFiles[i].Season, fi.group, fi.revision)
	}
	item := library.Item{
		SeasonGroups:    seasonGroups(seasonCounts),
		SeasonRevisions: revs.seasons(),
		Revisions:       revs.groups(),
		FileBytes:       fileBytes,
		Groups:          sortedKeys(groupCounts),
		AltTitles:       altTitles(s.AlternateTitles),
		AniListIDs:      s.IDs.AniList,
		Arr:             library.ArrSonarr,
		Title:           s.Title,
		ImdbID:          s.IDs.IMDb,
		ArrURL:          library.SafeLogURL(s.WebURL(w.sonarrURL)),
		ArrID:           s.ID,
		TvdbID:          s.IDs.TVDB,
		TmdbID:          s.IDs.TMDB,
		Year:            s.Year,
		HasFile:         len(files) > 0,
	}
	if !s.Order.Official {
		item.Order = &library.Order{Type: s.Order.Type, Name: s.Order.Name}
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
func (w *Walker) movieItem(m *radarr.Movie) library.Item {
	item := library.Item{
		AltTitles: altTitles(m.AlternateTitles),
		Arr:       library.ArrRadarr,
		Title:     m.Title,
		ImdbID:    m.IDs.IMDb,
		ArrURL:    library.SafeLogURL(m.WebURL(w.radarrURL)),
		ArrID:     m.ID,
		TmdbID:    m.IDs.TMDB,
		Year:      m.Year,
		HasFile:   m.HasFile && m.File != nil,
	}
	if item.HasFile {
		fi := fileFromMovie(m.File)
		// fi.group is never empty: fileInfoFrom normalizes it via
		// release.NormalizeGroup, which falls back to the LOWERCASED NOGRP
		// sentinel ("nogrp") for group-less files.
		item.Groups = []string{fi.group}
		item.Revisions = map[string]release.Revision{fi.group: fi.revision}
		if m.File.ID > 0 {
			item.FileBytes = map[int]int64{m.File.ID: m.File.Size}
		}
		item.Current = fingerprint(&fi)
	}
	return item
}

// fingerprint classifies a library file into a release.Release using the shared
// classifier, so the library and SeaDex sides compare in one vocabulary.
func fingerprint(fi *fileInfo) release.Release {
	rel := release.Classify(&release.Input{
		Names:      nonEmpty(fi.sceneName, fi.relPath),
		Group:      fi.group,
		VideoCodec: fi.videoCodec,
		DualAudio:  isDualAudio(fi.audioLanguages),
	})
	rel.Revision = fi.revision
	return rel
}

// fileInfo is the release-relevant subset of an arr file.
type fileInfo struct {
	group          string
	sceneName      string
	relPath        string
	videoCodec     string
	audioLanguages []string
	revision       release.Revision
}

// fileInfoFrom builds a fileInfo from the release-relevant
// fields common to a Sonarr episode file and a Radarr movie file.
func fileInfoFrom(group, sceneName, relPath string, mi *arrapi.MediaInfo, rev *arrapi.Revision) fileInfo {
	fi := fileInfo{
		group:     release.NormalizeGroup(group),
		sceneName: sceneName,
		relPath:   relPath,
		revision:  fileRevision(rev, sceneName, relPath),
	}
	if mi != nil {
		fi.videoCodec = mi.VideoCodec
		fi.audioLanguages = mi.AudioLanguages
	}
	return fi
}

// fileRevision is the revision the arr recorded for the file at import, which
// is the highest of its readings of the file, folder and grab title and
// survives a rename. Only without one do the names count, and then only an
// explicit token: a renamed file has lost its token, so a name without one is
// missing evidence rather than proof of an original release.
func fileRevision(rev *arrapi.Revision, sceneName, relPath string) release.Revision {
	if rev != nil {
		return release.ArrRevision(rev.Version, rev.IsRepack)
	}
	return release.ExplicitRevision(sceneName, relPath)
}

// fileFromEpisode extracts fileInfo from a Sonarr episode file.
func fileFromEpisode(f *sonarr.EpisodeFile) fileInfo {
	return fileInfoFrom(f.ReleaseGroup, f.SceneName, f.RelativePath, f.MediaInfo, f.Revision)
}

// fileFromMovie extracts fileInfo from a Radarr movie file.
func fileFromMovie(f *radarr.MovieFile) fileInfo {
	return fileInfoFrom(f.ReleaseGroup, f.SceneName, f.RelativePath, f.MediaInfo, f.Revision)
}

// revisionFold collects each file's revision per (season, group) and per group
// so a series item can carry the newest reading of each.
type revisionFold struct {
	bySeason map[int]map[string][]release.Revision
	byGroup  map[string][]release.Revision
}

func (f *revisionFold) add(season int, group string, rev release.Revision) {
	if f.bySeason == nil {
		f.bySeason = make(map[int]map[string][]release.Revision)
		f.byGroup = make(map[string][]release.Revision)
	}
	if f.bySeason[season] == nil {
		f.bySeason[season] = make(map[string][]release.Revision)
	}
	f.bySeason[season][group] = append(f.bySeason[season][group], rev)
	f.byGroup[group] = append(f.byGroup[group], rev)
}

// seasons folds each (season, group) to its newest reading, or nil when no
// file was added.
func (f *revisionFold) seasons() map[int]map[string]release.Revision {
	if len(f.bySeason) == 0 {
		return nil
	}
	out := make(map[int]map[string]release.Revision, len(f.bySeason))
	for season, groups := range f.bySeason {
		out[season] = newestPerGroup(groups)
	}
	return out
}

// groups folds each group to its newest reading across the item, or nil when
// no file was added.
func (f *revisionFold) groups() map[string]release.Revision {
	if len(f.byGroup) == 0 {
		return nil
	}
	return newestPerGroup(f.byGroup)
}

func newestPerGroup(byGroup map[string][]release.Revision) map[string]release.Revision {
	out := make(map[string]release.Revision, len(byGroup))
	for group, revs := range byGroup {
		out[group] = release.NewestRevision(revs...)
	}
	return out
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

// isDualAudio reports whether langs, a file's tagged audio-track languages,
// holds more than one distinct language.
func isDualAudio(langs []string) bool {
	for _, l := range langs {
		if l != langs[0] {
			return true
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

// altTitles returns the distinct non-empty alternate titles from arr metadata,
// trimmed, in first-seen order, for the AniList title-fallback matcher.
func altTitles(alts []string) []string {
	var out []string
	for _, a := range alts {
		if t := strings.TrimSpace(a); t != "" && !slices.Contains(out, t) {
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

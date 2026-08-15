// Package mapping bridges AniList IDs (what SeaDex keys on) to the arr IDs
// Sonarr and Radarr key on (TVDB, TMDB, IMDb), using the Fribb anime-lists
// dataset plus a local overrides file the operator can pin misses in.
//
// The Fribb file is fetched with a conditional GET and cached; overrides are
// re-read every load and overlaid on top, so an operator entry always wins.
package mapping

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/mediatype"
)

// DefaultURL is the Fribb anime-list-mini.json endpoint - the AniList<->arr ID
// bridge. The variant choice is Fribb contract knowledge, so it lives here.
const DefaultURL = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"

const (
	// DefaultRefresh is the reuse-if-fresh window for the Fribb map. 0 revalidates
	// every cycle: a conditional GET makes an unchanged map a cheap 304, while a
	// change is picked up within one cycle. A failed revalidation is harmless -
	// the persisted cache is reused stale-on-error and the next cycle retries.
	DefaultRefresh = 0

	// maxMapBytes bounds the Fribb download before decode (~2.7x the real ~5.9MB body).
	maxMapBytes = 16 << 20
	// mapSizeWarnBytes is maxMapBytes' pre-cliff warning threshold (80%, the
	// app-wide degradation fraction). A body past the cap is a PERSISTENT refresh
	// refusal that never self-heals, so warn while refreshes still succeed. It
	// cannot fold into the record-cap warning: body size and record count move
	// independently.
	mapSizeWarnBytes = maxMapBytes / degradation.SizeWarnDenominator * degradation.SizeWarnNumerator
	// maxOverrideBytes bounds the local overrides file.
	maxOverrideBytes = 4 << 20
	maxAttempts      = 3
	baseDelay        = time.Second
)

// Loader fetches and caches the Fribb map and overlays the overrides file.
type Loader struct {
	http          *http.Client
	log           *slog.Logger
	url           string
	overridesPath string
	refresh       time.Duration
}

// Record is the resolved mapping for one AniList entry: its media type and the
// arr IDs it corresponds to. Fields are ordered for govet fieldalignment.
//
// The json tags below are read by TWO decoders: the persisted Cache decodes
// through encoding/json, while overrides.json is walked key-by-key by
// decodeOverrideRecord, whose switch restates these six names as literals.
// Adding, renaming or removing a field here REQUIRES the matching edit there.
type Record struct {
	Type    string   `json:"type"`
	IMDbIDs []string `json:"imdb_ids,omitempty"`
	// TmdbMovies is the record's themoviedb_id.movie list, decoded for EVERY
	// record type and read by the ID bridge regardless of the type label: a TMDB
	// movie id is a Radarr id by construction. Deliberately NOT the IMDb list,
	// since TVDB reuses a film's IMDb id on the parent series. Canonical form
	// (positive ids only) is a Record invariant, so readers take the slice as-is.
	TmdbMovies []int `json:"tmdb_movies,omitempty"`
	AniListID  int   `json:"anilist_id"`
	TvdbID     int   `json:"tvdb_id,omitempty"`
	SeasonTvdb int   `json:"season_tvdb,omitempty"`
}

// IsMovie reports whether the entry maps to a Radarr movie (Fribb type MOVIE).
// Every other type maps to a Sonarr series.
func (r *Record) IsMovie() bool { return mediatype.IsMovie(r.Type) }

// RoutedIDs returns the identifiers the record's routed arr consumes, per the
// HasArrIdentifier routing decision: a MOVIE record yields its TMDB-movie and
// IMDb ids (tvdb zero); every other type yields its TVDB id. The TYPE LABEL is
// what routes here - a record's unambiguous movie TMDB ids stay separately
// reachable as Record.TmdbMovies regardless of that label.
func (r *Record) RoutedIDs() (tvdbID int, tmdbMovies []int, imdbIDs []string) {
	if r.IsMovie() {
		return 0, r.TmdbMovies, r.IMDbIDs
	}
	// Zero out a non-usable TVDB id here so the usability rule has ONE home:
	// callers do a presence check, never a policy check.
	if r.TvdbID <= 0 {
		return 0, nil, nil
	}
	return r.TvdbID, nil, nil
}

// HasArrIdentifier reports whether the record carries a USABLE identifier
// consumed by the arr selected by its type: TMDB-movie/IMDb for movies, TVDB
// for series. The canonical arr-routing predicate shared by the refresh
// acceptance guard, the matcher and the report's reverse catalogue. Usability
// is a Record INVARIANT (canonicalize), so this is a presence check.
func (r *Record) HasArrIdentifier() bool {
	tvdb, tmdbMovies, imdbIDs := r.RoutedIDs()
	return tvdb > 0 || len(tmdbMovies) > 0 || len(imdbIDs) > 0
}

// IsSpecial reports whether the entry is an OVA/ONA/special/music video rather
// than a standard TV season or movie, so it can be excluded when the operator
// turns specials off. A match with no type is treated as non-special.
func (r *Record) IsSpecial() bool { return mediatype.IsSpecial(r.Type) }

// HasMappedSeason reports whether the record carries a positive Fribb TVDB
// season - the predicate season-exact comparison and the season floor key on.
func (r *Record) HasMappedSeason() bool { return r.SeasonTvdb > 0 }

// canonicalize applies Record's canonical field forms - the single home of the
// rule both producers must agree on, so exact-key lookups and the reverse
// arr-ID catalogue cannot see two differently-shaped Records for one anime.
// Idempotent: on the Fribb path the tolerant decoders already emit these forms.
func (r *Record) canonicalize() {
	r.Type = mediatype.Normalize(r.Type)
	r.IMDbIDs = trimmed(r.IMDbIDs)
	r.TmdbMovies = positiveInts(r.TmdbMovies)
	r.TvdbID = max(0, r.TvdbID)
	r.SeasonTvdb = max(0, r.SeasonTvdb)
}

// Cache is the persisted mapping state: the parsed Fribb records plus the HTTP
// validators and timestamp needed for the next conditional GET.
type Cache struct {
	FetchedAt    time.Time `json:"fetched_at"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	// RefusedETag / RefusedLastModified are the validators of the last body a
	// refresh REFUSED. While they are set the conditional GET asks about THAT
	// body, so a persistent refusal costs one 304 per cycle instead of a ~5.9 MB
	// download. Cleared by any accepted refresh and by a 304 that revalidates the
	// accepted body; empty when the refusal carried no validators.
	RefusedETag         string   `json:"refused_etag,omitempty"`
	RefusedLastModified string   `json:"refused_last_modified,omitempty"`
	Records             []Record `json:"records,omitempty"`
	// RejectedRefreshes is the persisted streak of consecutive persistent refresh
	// refusals, reset by any accepted refresh or a 304 that revalidates a USABLE
	// cache. It is the ONE carrier: escalation and the logged
	// stale_consecutive_rejections both read this field, so the number an operator
	// reads is the number escalation acted on. It advances even when no usable
	// stale cache exists, because the streak describes the upstream rather than
	// the cache. WHICH failures advance it is isPersistentRefreshFailure's call.
	RejectedRefreshes int `json:"rejected_refreshes,omitempty"`
}

// Index is an AniList-ID-keyed lookup over mapping records.
type Index struct {
	byAniList map[int]Record
}

// Lookup returns the record for an AniList ID and whether it was present.
func (i *Index) Lookup(aniListID int) (Record, bool) {
	if i == nil {
		return Record{}, false
	}
	r, ok := i.byAniList[aniListID]
	return r, ok
}

// Len returns the number of indexed records.
func (i *Index) Len() int {
	if i == nil {
		return 0
	}
	return len(i.byAniList)
}

// ForEachRecord calls fn once per indexed record, in unspecified order. It backs
// the report's reverse (arr-ID) catalogue without materializing a slice copy of
// all ~40k records.
func (i *Index) ForEachRecord(fn func(Record)) {
	if i == nil {
		return
	}
	for _, r := range i.byAniList {
		fn(r)
	}
}

// NewIndex builds an index over records already decoded elsewhere, keyed by
// AniList ID. Production code obtains an Index from Loader.Load.
func NewIndex(records []Record) *Index {
	return buildIndex(records)
}

// deduplicateRecords returns one effective record per AniList ID, preserving
// buildIndex's last-record-wins semantics and stable order. Records without a
// positive AniList ID are omitted, so the acceptance validators cannot count a
// larger population than the effective served index.
func deduplicateRecords(records []Record) []Record {
	last := make(map[int]int, len(records))
	for i := range records {
		if records[i].AniListID > 0 {
			last[records[i].AniListID] = i
		}
	}
	out := make([]Record, 0, len(last))
	for i := range records {
		if records[i].AniListID > 0 && last[records[i].AniListID] == i {
			out = append(out, records[i])
		}
	}
	return out
}

// buildIndex keys records by AniList ID, admitting only positive IDs (real
// SeaDex lookups use positive AniList IDs, so a zero or negative key could
// never resolve an entry). A later record with the same ID overwrites an
// earlier one; overrides are applied on top afterwards.
func buildIndex(records []Record) *Index {
	byAniList := make(map[int]Record, len(records))
	for _, r := range records {
		if r.AniListID > 0 {
			// Canonical form is an INDEX invariant, applied once here rather than
			// re-checked by every accessor: a cache read back from state.json reaches
			// buildIndex through plain encoding/json. canonicalize is idempotent.
			r.canonicalize()
			byAniList[r.AniListID] = r
		}
	}
	return &Index{byAniList: byAniList}
}

// indexedRecordCount returns how many records survive into the served index
// (distinct positive AniList IDs, per buildIndex). It is the single spelling of
// the count every records / stale_records attribute reports, so a cache written
// before deduplication cannot over-report the map consumers receive.
func indexedRecordCount(records []Record) int {
	seen := make(map[int]struct{}, len(records))
	for i := range records {
		if records[i].AniListID > 0 {
			seen[records[i].AniListID] = struct{}{}
		}
	}
	return len(seen)
}

// IndexedRecords reports how many of the cached records survive into the served
// index (distinct positive AniList IDs) - the one count every mapping log
// attribute reports. Exported for internal/state's "state loaded" line.
func (c *Cache) IndexedRecords() int {
	if c == nil {
		return 0
	}
	return indexedRecordCount(c.Records)
}

// cacheUsable reports whether a cached record set is usable as an effective
// AniList-keyed mapping: after deduplication the effective set must be
// non-empty and meet the same conservative 1% arr-identifier coverage floor a
// newly accepted refresh must meet, without the previous-relative type and
// shrink checks. Every cache-state gate keys on this predicate, so "has cached
// bytes" can never diverge from "has a mapping the consumers can use".
func cacheUsable(records []Record) bool {
	records = deduplicateRecords(records)
	if len(records) == 0 {
		return false
	}
	return arrIdentifierCount(records) >= coverageFloor(len(records))
}

// coverageFloor returns the conservative 1% acceptance floor (ceiling division,
// minimum 1) shared by cacheUsable and validateRefreshedRecords.
func coverageFloor(n int) int { return max(1, (n+99)/100) }

// coverageLost reports the shared loss-relative floor decision the type, scope
// and routing floors apply: the previously accepted cache met its own floor for
// the population AND the candidate falls below both the candidate floor and the
// prior count - so an additive refresh that merely grows never fires it.
func coverageLost(prevCount, count, previousMinimum, minimum int) bool {
	return prevCount >= previousMinimum && count < minimum && count < prevCount
}

// populationExtinct is the per-population EXTINCTION guard, deliberately
// without the significance gate its siblings carry: the previously accepted
// cache had a population at all (prevCount > 0) and the candidate has none of
// it. Going from N to exactly zero is never a sampling artifact, so this is the
// one guard that reaches BELOW previousMinimum; its siblings stay gated on that
// share, so a sparse population's PARTIAL shrink keeps its exemption.
func populationExtinct(prevCount, count int) bool { return prevCount > 0 && count == 0 }

// populationCollapsed is the per-population shrink guard the type, scope and
// routing validators apply beside their loss-relative floors: the previously
// accepted cache carried a meaningful population and the candidate retains less
// than half of it (degradation.Shrunk). The 1% floors catch total loss; this
// catches the MID-BAND, where a corrupted refresh guts most of ONE population
// while every floor stays green. Deliberately NO auto-accept after a streak:
// the documented remedy is removing state.json to cold-start onto the new shape.
func populationCollapsed(prevCount, count, previousMinimum int) bool {
	return prevCount >= previousMinimum && degradation.Shrunk(count, prevCount)
}

// NewLoader returns a mapping loader. httpClient must be non-nil for any loader
// that will fetch, url is the Fribb JSON source, overridesPath is the local
// override file (may be absent), refresh is the conditional re-download
// cadence, and logger may be nil.
func NewLoader(httpClient *http.Client, url, overridesPath string, refresh time.Duration, logger *slog.Logger) *Loader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Loader{
		http:          httpClient,
		log:           logger,
		url:           url,
		overridesPath: overridesPath,
		refresh:       refresh,
	}
}

// Load returns the mapping index to use this cycle and the cache to persist. It
// reuses prev when it is still fresh, otherwise issues a conditional GET and
// refreshes on a 200 (or bumps the timestamp on a 304). Overrides are always
// re-read and applied on top. When a refresh fails but prev holds a usable
// record set (cacheUsable), it returns the stale index with a *StaleMapError
// (match with errors.As); any other non-nil error means no usable map at all.
// The persisted records are canonicalized at this INPUT boundary, on a private
// copy, so every refresh decision and the served Index read ONE representation.
func (l *Loader) Load(ctx context.Context, prev *Cache) (Cache, *Index, error) {
	canonicalPrev := prev
	if prev != nil {
		clone := *prev
		clone.Records = slices.Clone(prev.Records)
		for i := range clone.Records {
			clone.Records[i].canonicalize()
		}
		canonicalPrev = &clone
	}
	next, err := l.refreshCache(ctx, canonicalPrev)
	idx := buildIndex(next.Records)
	l.applyOverrides(ctx, idx)
	return next, idx, err
}

// StaleMapError reports a refresh failure where a stale-but-usable cached map
// was returned, so the cycle may still compare against the stale index.
// Consumers discriminate a degraded-but-comparable load with errors.As rather
// than probing the Cache's Records: the loader owns the usability judgment.
// Fields are ordered for govet fieldalignment.
type StaleMapError struct {
	// cause is the underlying refresh failure; nil for the shrunk-refresh guard.
	cause error
	// msg describes which refresh step failed (e.g. "refresh failed").
	msg string
	// age is how long ago the stale map was fetched, rounded for logging.
	age time.Duration
	// records is the size of the stale-but-usable record set.
	records int
	// shrunkReturned/shrunkPrevious carry the shrink guard's counts as structured
	// facts, zero for every other class. Keeping the live counts OUT of msg keeps
	// stale_reason an equality-queryable, fixed-cardinality class discriminator.
	shrunkReturned int
	shrunkPrevious int
}

// Error renders the degradation facts as the single prose line the degraded
// cycle logs. The message shape is a pinned log contract (stale_map_error_test).
func (e *StaleMapError) Error() string {
	reason := e.msg
	if e.shrunkPrevious > 0 {
		reason = fmt.Sprintf("%s (returned %d, previous %d)", e.msg, e.shrunkReturned, e.shrunkPrevious)
	}
	if e.cause != nil {
		return fmt.Sprintf("mapping: %s, using stale map (%d records, fetched %s ago): %v", reason, e.records, e.age, e.cause)
	}
	return fmt.Sprintf("mapping: %s, using stale map (%d records, fetched %s ago)", reason, e.records, e.age)
}

// Unwrap exposes the underlying refresh failure for errors.Is/As chains.
func (e *StaleMapError) Unwrap() error { return e.cause }

// LogAttrs returns the degradation facts Error() flattens into prose as
// structured slog key/value pairs, so callers can emit a queryable degraded-
// cycle line without parsing the message text. It deliberately does NOT carry
// the rejection streak: that lives on Cache.RejectedRefreshes, which is what
// escalation reads, and a second copy contradicted it on a transient failure.
func (e *StaleMapError) LogAttrs() []any {
	attrs := []any{
		"stale_reason", e.msg,
		"stale_age_seconds", e.age.Seconds(),
		"stale_records", e.records,
	}
	if e.shrunkPrevious > 0 {
		attrs = append(attrs, "stale_returned", e.shrunkReturned, "stale_previous", e.shrunkPrevious)
	}
	return attrs
}

// staleOrFail returns the stale cache wrapped in a *StaleMapError when prev
// holds a usable record set (cacheUsable; carrying cause when non-nil),
// otherwise the no-cache error - an unusable cache must degrade like no cache
// at all, so the scout preserves findings instead of comparing against an empty
// map. The age is clamped to zero, so a future FetchedAt cannot report a
// misleading negative age.
func staleOrFail(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	if cacheUsable(prev.Records) {
		return *prev, &StaleMapError{
			cause:   cause,
			msg:     staleMsg,
			age:     max(time.Duration(0), time.Since(prev.FetchedAt).Round(time.Second)),
			records: indexedRecordCount(prev.Records),
		}
	}
	return *prev, noCache
}

// rejectRefresh degrades a persistent refresh refusal to the stale map via
// staleOrFail, additionally advancing the persisted consecutive-rejection
// streak (Cache.RejectedRefreshes) that escalation reads. It is reached ONLY
// through degradeRefresh, on a failure isPersistentRefreshFailure graded
// persistent; a transient failure takes plain staleOrFail there. The streak
// advances even when there is NO usable stale cache to return: it is state
// about the upstream, so gating it would freeze it at 0 on a first boot whose
// every refresh is refused - the never-self-heals case it exists to escalate.
func rejectRefresh(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	next, err := staleOrFail(prev, staleMsg, cause, noCache)
	next.RejectedRefreshes = prev.RejectedRefreshes + 1
	return next, err
}

// refreshFailureClass names WHICH step of a refresh failed. It is the
// vocabulary isPersistentRefreshFailure grades, so a call site says what
// happened and never what the streak should do about it.
type refreshFailureClass int

const (
	// failureFetch is a conditional GET that returned no usable response; the
	// error value decides the disposition.
	failureFetch refreshFailureClass = iota
	// failureParse is a 200 body that would not decode; the error value decides
	// the disposition.
	failureParse
	// failureValidation is a decoded body the acceptance invariants refused
	// (validateRefreshedRecords).
	failureValidation
	// failureShrunk is a decoded body the below-half whole-map shrink guard
	// refused. It carries no cause; its live counts ride as structured fields.
	failureShrunk
	// failureNotModifiedUnusable is a 304 answered to a request that carried no
	// validators (conditionalGet suppresses them for an unusable cache).
	failureNotModifiedUnusable
	// failureRefusedUnchanged is a 304 answered to a request that carried the
	// REFUSED body's validators: the refusal stands and the streak keeps advancing.
	failureRefusedUnchanged
)

// isPersistentRefreshFailure is the ONE home of the transient-vs-persistent
// refresh classification: every failure arm of refreshCache reaches the streak
// through it (via degradeRefresh), so the documented set and the code cannot
// drift apart. PERSISTENT (advances Cache.RejectedRefreshes), because each
// re-refuses identically every cycle: a record-cap or identifier-budget breach,
// a body over the size cap, a non-array document, either 304 failure class, an
// acceptance or shrink refusal, and any status whose remedy is the OPERATOR.
// Everything else is TRANSIENT and neither advances nor resets the streak.
func isPersistentRefreshFailure(class refreshFailureClass, cause error) bool {
	switch class {
	case failureValidation, failureShrunk, failureNotModifiedUnusable, failureRefusedUnchanged:
		return true
	case failureFetch:
		return isPersistentFetchFailure(cause)
	case failureParse:
		return errors.Is(cause, errRecordCapExceeded) || errors.Is(cause, errIdentifierBudgetExceeded) || errors.Is(cause, errNotJSONArray)
	}
	return false
}

// isPersistentFetchFailure grades a failed conditional GET, the one failure
// class whose disposition is carried entirely by the error VALUE: httpx maps
// 401/403 to *AuthError, 429 to *RateLimitError, and every other non-2xx to
// *HTTPStatusError. Split out so isPersistentRefreshFailure stays a dispatcher.
func isPersistentFetchFailure(cause error) bool {
	if _, ok := errors.AsType[*httpx.ResponseTooLargeError](cause); ok {
		return true
	}
	if _, ok := errors.AsType[*httpx.AuthError](cause); ok {
		return true
	}
	if _, ok := errors.AsType[*httpx.RateLimitError](cause); ok {
		return false
	}
	if status, ok := errors.AsType[*httpx.HTTPStatusError](cause); ok {
		// A come-back-later status is httpx's own rule, READ from the library rather
		// than restated, so this verdict and the library door's cannot drift when
		// the set moves. Any other status on a constant URL needs the operator.
		return !httpx.IsRetryableStatus(status.Code)
	}
	return false
}

// degradeRefresh is the single exit from every refreshCache failure arm: it
// degrades to the stale map (or returns noCache when there is no usable one)
// and lets isPersistentRefreshFailure - never the call site - decide whether
// the failure advances the persisted rejection streak.
func degradeRefresh(prev *Cache, class refreshFailureClass, staleMsg string, cause, noCache error) (Cache, error) {
	if isPersistentRefreshFailure(class, cause) {
		return rejectRefresh(prev, staleMsg, cause, noCache)
	}
	return staleOrFail(prev, staleMsg, cause, noCache)
}

// logSafeCause reduces an untrusted-input-derived parse error to log-safe text
// (single-line, control-stripped, capped at maxLoggedErrorBytes) WITHOUT
// discarding its wrap chain, so isPersistentRefreshFailure can still recognize
// a sentinel class on the value the caller passes it.
func logSafeCause(err error) error {
	return &sanitizedError{err: err, text: runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes)}
}

// sanitizedError renders pre-sanitized log-safe text while preserving the
// wrapped cause for errors.Is/As classification. Only Error() reaches a log
// boundary, so the untrusted original text stays out of the log stream.
type sanitizedError struct {
	err  error
	text string
}

func (e *sanitizedError) Error() string { return e.text }
func (e *sanitizedError) Unwrap() error { return e.err }

// refreshCache decides whether to reuse, re-validate, or re-download the Fribb
// map and returns the cache to persist. Validator hygiene lives in
// httpx.DoConditional, both directions: a poisoned persisted validator is
// skipped at replay, and captured validators arrive pre-sanitized.
func (l *Loader) refreshCache(ctx context.Context, prev *Cache) (Cache, error) {
	// Normalize the optional previous cache: Load(ctx, nil) is the natural
	// representation of "no persisted cache", and an empty Cache makes nil take
	// the ordinary initial-fetch route instead of panicking on prev.FetchedAt.
	if prev == nil {
		prev = &Cache{}
	}
	age := time.Since(prev.FetchedAt)
	// age >= 0 rejects a future FetchedAt (clock skew or a corrupt state file): a
	// negative age is never fresh, forcing a revalidating fetch.
	if l.refresh > 0 && age >= 0 && age < l.refresh && cacheUsable(prev.Records) {
		l.log.Debug("mapping: cache fresh, skipping fetch", "records", indexedRecordCount(prev.Records), "age", age.Round(time.Second))
		return *prev, nil
	}

	askedAboutRefused := refusedValidators(prev) != (httpx.Validators{})
	res, err := l.conditionalGet(ctx, prev)
	if err != nil {
		if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok {
			return degradeRefresh(prev, failureFetch, "refresh exceeded size cap", err,
				fmt.Errorf("mapping: refresh exceeded size cap and no cache available: %w", err))
		}
		return degradeRefresh(prev, failureFetch, "refresh failed", err,
			fmt.Errorf("mapping: initial fetch failed and no cache available: %w", err))
	}
	if res.NotModified {
		if askedAboutRefused {
			// The body a previous cycle already refused is unchanged: keep serving the
			// stale map, keep the streak advancing, and do not re-download the body.
			return degradeRefresh(prev, failureRefusedUnchanged, "refused body unchanged", nil,
				errors.New("mapping: refused body unchanged and no cache available"))
		}
		return l.reuseCachedRecords(prev)
	}
	return l.acceptRefresh(prev, res)
}

// reuseCachedRecords handles a 304: the upstream is unchanged, so the cached
// records are reused with a bumped timestamp. A cache with no usable record set
// errors instead of affirming an unusable map.
func (l *Loader) reuseCachedRecords(prev *Cache) (Cache, error) {
	if !cacheUsable(prev.Records) {
		// A 304 answered to a request that carried NO validators is an upstream or
		// intermediary protocol violation, not a transient outage: it repeats
		// identically every cycle and never self-heals without the operator.
		return degradeRefresh(prev, failureNotModifiedUnusable, "not modified without a usable cache", nil,
			errors.New("mapping: not modified but no cache available"))
	}
	l.log.Debug("mapping: not modified, reusing cache", "records", indexedRecordCount(prev.Records))
	refreshed := *prev
	refreshed.FetchedAt = time.Now()
	// A 304 is upstream affirmation that the cached map is current, so any
	// acceptance-guard rejection streak ends here.
	if prev.RejectedRefreshes > 0 {
		l.log.Info("mapping: rejection streak ended by 304 revalidation", "ended_rejection_streak", prev.RejectedRefreshes, "records", indexedRecordCount(prev.Records))
	}
	refreshed.RejectedRefreshes = 0
	// A 304 against the ACCEPTED validators affirms the cached body is current, so
	// any remembered refused body is history.
	refreshed.RefusedETag, refreshed.RefusedLastModified = "", ""
	return refreshed, nil
}

// acceptRefresh evaluates a fresh 200 body and, when the acceptance pipeline
// REFUSED it persistently, remembers that body's validators so the next cycle
// asks about it with a conditional GET instead of re-downloading it.
func (l *Loader) acceptRefresh(prev *Cache, res httpx.ConditionalResult) (Cache, error) {
	next, err := l.evaluateRefresh(prev, res)
	if err != nil && next.RejectedRefreshes > prev.RejectedRefreshes {
		next.RefusedETag = res.Validators.ETag
		next.RefusedLastModified = res.Validators.LastModified
	}
	return next, err
}

// evaluateRefresh parses a fresh 200 body and runs the cache-acceptance
// invariants (the record cap, deduplication, the validation floor and the
// shrink guard), degrading to the stale map when any step rejects the refresh.
func (l *Loader) evaluateRefresh(prev *Cache, res httpx.ConditionalResult) (Cache, error) {
	if n := len(res.Body); n >= mapSizeWarnBytes {
		l.log.Warn("mapping: Fribb body approaching the download size cap; a body past it refuses every refresh and freezes the map stale",
			"bytes", n, "cap", maxMapBytes)
	}
	parsed, err := parseFribbForRefresh(res.Body, l.log)
	if err != nil {
		if errors.Is(err, errRecordCapExceeded) {
			return degradeRefresh(prev, failureParse, "refresh exceeded record cap", err,
				fmt.Errorf("%w and no cache available", err))
		}
		if errors.Is(err, errIdentifierBudgetExceeded) {
			return degradeRefresh(prev, failureParse, "refresh exceeded identifier budget", err,
				fmt.Errorf("%w and no cache available", err))
		}
		// The no-first-token case (an empty or whitespace-only body) never reaches
		// here: parseFribbForRefresh classifies it as a transient parse failure.
		if errors.Is(err, errNotJSONArray) {
			err = logSafeCause(err)
			return degradeRefresh(prev, failureParse, "refresh not a JSON array", err,
				fmt.Errorf("mapping: %w and no cache available", err))
		}
		err = logSafeCause(err)
		return degradeRefresh(prev, failureParse, "parse failed", err,
			fmt.Errorf("mapping: parse failed and no cache available: %w", err))
	}
	// Collapse duplicate AniList IDs BEFORE any acceptance invariant runs:
	// buildIndex keeps only the last record per ID, so size-comparing the raw row
	// count would let a body repeating one ID pass every guard and index to little.
	records := deduplicateRecords(parsed.records)
	if validationErr := validateRefreshedRecords(prev.Records, records, parsed.elements); validationErr != nil {
		return degradeRefresh(prev, failureValidation, "refresh validation failed", validationErr,
			fmt.Errorf("mapping: %w and no cache available", validationErr))
	}
	// A syntactically valid but sharply truncated refresh (one record replacing
	// ~40k) can pass the coverage floor above yet silently erase most mappings, so
	// a below-half refresh (degradation.Shrunk) keeps the stale map.
	if prevCount := indexedRecordCount(prev.Records); cacheUsable(prev.Records) && degradation.Shrunk(len(records), prevCount) {
		// The noCache argument is unreachable here (cacheUsable guarantees the stale
		// branch). The reason string is FIXED (class-queryable in Loki); the live
		// counts ride as structured fields on the error instead.
		next, err := degradeRefresh(prev, failureShrunk, "refresh shrank below half of previous",
			nil, errors.New("mapping: refresh shrank unexpectedly and no cache available"))
		if stale, ok := errors.AsType[*StaleMapError](err); ok {
			stale.shrunkReturned, stale.shrunkPrevious = len(records), prevCount
		}
		return next, err
	}
	// A first boot (or a cache reset) onto a body that routes nothing to one arr
	// is accepted, not refused - and reported. See logAcceptedWithoutBaseline.
	l.logAcceptedWithoutBaseline(prev, records)
	// previous_records is the baseline the absolute count needs: degradation.Shrunk
	// rejects only BELOW half, so an accepted refresh may legitimately retain
	// exactly half of the previous map and would otherwise read like any other
	// success. The five per-population guards refuse only a below-half collapse
	// too, so the census attributes carry that same reasoning to the populations
	// they are defined over - a routing loss is one queryable line rather than an
	// inference. revalidatable reports whether a validator was persisted.
	pop := censusOf(records)
	attrs := []any{
		"records", len(records),
		"previous_records", indexedRecordCount(prev.Records),
		"elements", parsed.elements,
		"routed_identifiers", pop.identifiers,
		"movie_routed", pop.movieRouted,
		"series_routed", pop.seriesRouted,
		"typed_records", pop.typed,
		"season_scoped_records", pop.positiveSeason,
		"special_records", pop.special,
		"revalidatable", res.Validators.ETag != "" || res.Validators.LastModified != "",
	}
	if prev.RejectedRefreshes > 0 {
		attrs = append(attrs, "ended_rejection_streak", prev.RejectedRefreshes)
	}
	l.log.Info("mapping: refreshed", attrs...)
	// The fresh Cache literal deliberately omits RejectedRefreshes: an accepted
	// refresh resets the streak (see Cache.RejectedRefreshes).
	return Cache{
		FetchedAt:    time.Now(),
		Records:      records,
		ETag:         res.Validators.ETag,
		LastModified: res.Validators.LastModified,
	}, nil
}

// logAcceptedWithoutBaseline reports an ACCEPTED refresh that carries no
// resolvable record for one arr while there was no usable baseline to judge it
// against. It is a diagnostic only: it never refuses the refresh and never
// touches the rejection streak. With no usable previous cache every
// loss-relative guard is skipped and nothing can prove a LOSS, while an
// unconditional zero-population refusal would make a legitimately one-sided
// catalogue refuse to refresh to ITSELF. The harm this closes is the SILENCE.
func (l *Loader) logAcceptedWithoutBaseline(prev *Cache, records []Record) {
	if cacheUsable(prev.Records) {
		return
	}
	absent := absentRoutingClasses(records)
	if len(absent) == 0 {
		return
	}
	l.log.Error("mapping: accepted a refresh with no records for an arr; that arr will match nothing this cycle, pin the affected entries in overrides.json",
		"absent_routing_classes", absent,
		"records", len(records),
		"routed_identifiers", arrIdentifierCount(records))
}

// validateRefreshedRecords is acceptRefresh's acceptance invariant for a fresh
// 200 body: it rejects a refresh below the AniList-key, arr-identifier or type
// coverage floors, and one whose individual populations collapse below half of
// the previously accepted cache's (populationCollapsed) or vanish entirely
// (populationExtinct). The conservative 1% floor has ~19x headroom against the
// real body (8279/~42868 measured 2026-07). records MUST already be
// deduplicated, and sourceElements is the body's top-level element count, so
// destructive filtering cannot shrink numerator and denominator together.
func validateRefreshedRecords(previous, records []Record, sourceElements int) error {
	// No zero-record special case: coverageFloor is >= 1 for every input, so
	// the AniList-key floor below refuses an empty candidate on its own.
	keyMinimum := coverageFloor(sourceElements)
	if len(records) < keyMinimum {
		return fmt.Errorf("AniList-key coverage %d/%d is below minimum %d", len(records), sourceElements, keyMinimum)
	}
	minimum := coverageFloor(len(records))
	// Anchor the arr floor on the SOURCE element count, like the AniList-key floor
	// above: deriving it from the already-key-filtered candidate would let the two
	// floors compose multiplicatively (0.01% coverage reading as healthy).
	if covered := arrIdentifierCount(records); covered < keyMinimum {
		return fmt.Errorf("arr identifier coverage %d/%d is below minimum %d", covered, sourceElements, keyMinimum)
	}
	// An unusable previous cache must degrade like no cache here too: the loader
	// refuses to serve it, so it must not anchor the loss-relative guards either -
	// a corrupted state file could otherwise reject a healthy smaller refresh.
	if !cacheUsable(previous) {
		return nil
	}
	previous = deduplicateRecords(previous)
	// One significance gate for every population: the previously accepted cache's
	// own 1% floor, derived once so the guards cannot drift on that basis.
	previousMinimum := coverageFloor(len(previous))
	floors := acceptanceFloors{total: len(records), previousMinimum: previousMinimum, minimum: minimum}
	// One census per side, counted once: every population guard below reads it,
	// so five separate passes over ~40k records collapse into two.
	prevPop, pop := censusOf(previous), censusOf(records)
	if err := validateTypeCoverage(prevPop, pop, floors); err != nil {
		return err
	}
	if err := validateScopeCoverage(prevPop, pop, floors); err != nil {
		return err
	}
	return validateRoutingCoverage(prevPop, pop, floors)
}

// validateTypeCoverage rejects a candidate refresh that lost type coverage
// relative to the previously accepted cache. A wholesale loss of the type field
// re-routes every MOVIE record to Sonarr via its parent tvdb_id while still
// passing the arr-identifier floor and the shrink guard - but only a LOSS is a
// degradation, so the floor is relative: it fires only when that cache was
// itself type-rich AND the candidate carries fewer typed records. A type-sparse
// cache or catalogue is a valid shape, not a regression.
func validateTypeCoverage(previous, candidate populations, f acceptanceFloors) error {
	return validatePopulation("type", "typed", previous.typed, candidate.typed, f)
}

// acceptanceFloors carries the three per-refresh quantities every population
// guard shares: the candidate record total the rejection messages quote, the
// previous cache's 1% significance gate, and the candidate's own 1% floor. One
// named value, so a transposed member cannot compile and invert a guard.
type acceptanceFloors struct {
	total           int
	previousMinimum int
	minimum         int
}

// populations is the per-refresh census of the five semantic populations every
// acceptance guard is defined over, counted in ONE pass. Both consumers read
// it: the guards compare candidate against previous, and the accepted-refresh
// log line reports the candidate's census, which is what makes a loss below
// each guard's threshold visible.
type populations struct {
	typed          int
	positiveSeason int
	special        int
	movieRouted    int
	seriesRouted   int
	identifiers    int
}

// censusOf counts every population in one pass over records. The routed
// populations count only records that can actually RESOLVE in their arr
// (HasArrIdentifier), so a candidate that keeps every type but loses one side's
// usable ids reads as a collapse of that side. identifiers is their sum.
func censusOf(records []Record) populations {
	var p populations
	for i := range records {
		r := &records[i]
		if r.Type != "" {
			p.typed++
		}
		if r.HasMappedSeason() {
			p.positiveSeason++
		}
		if r.IsSpecial() {
			p.special++
		}
		if r.HasArrIdentifier() {
			p.identifiers++
			if r.IsMovie() {
				p.movieRouted++
			} else {
				p.seriesRouted++
			}
		}
	}
	return p
}

// validatePopulation applies the shared guards every semantic population is
// checked with: the extinction guard, the loss-relative floor and the
// below-half shrink guard. floorNoun and collapseNoun carry each population's
// own error vocabulary; the three floor quantities travel as one value.
func validatePopulation(floorNoun, collapseNoun string, prevCount, count int, f acceptanceFloors) error {
	if populationExtinct(prevCount, count) {
		return fmt.Errorf("%s records went extinct (previous cache carried %d)", collapseNoun, prevCount)
	}
	if coverageLost(prevCount, count, f.previousMinimum, f.minimum) {
		return fmt.Errorf("%s coverage %d/%d is below minimum %d (previous cache carried %d %s records)", floorNoun, count, f.total, f.minimum, prevCount, collapseNoun)
	}
	if populationCollapsed(prevCount, count, f.previousMinimum) {
		return fmt.Errorf("%s records collapsed below half of previous (%d of previous %d)", collapseNoun, count, prevCount)
	}
	return nil
}

// validateScopeCoverage rejects a candidate refresh that wholesale lost the
// mapping metadata controlling comparison scope. The typed and routing floors
// cannot see it: a body whose season objects all decode to SeasonTvdb=0, or
// whose OVA/SPECIAL labels all became the still-valid TV, keeps every other
// population healthy while ordinary cours compare whole-series and
// exclude_specials bucketing is bypassed. Same loss-relative shape as the rest.
func validateScopeCoverage(previous, candidate populations, f acceptanceFloors) error {
	if err := validatePopulation("positive-season", "season-scoped", previous.positiveSeason, candidate.positiveSeason, f); err != nil {
		return err
	}
	return validatePopulation("special-type", "special", previous.special, candidate.special, f)
}

// validateRoutingCoverage rejects a candidate refresh that collapsed a routing
// population relative to the previously accepted cache. The typed floor
// validates syntactic presence of Type, but routing recognizes only MOVIE - so
// a wrong-but-string schema change (every movie renamed FILM) retains 100%
// typed coverage while routing an entire side of the catalogue to the wrong
// arr. Guard the operational invariant instead: preservation of both routing
// populations, so future non-movie labels stay legal.
func validateRoutingCoverage(previous, candidate populations, f acceptanceFloors) error {
	if err := validatePopulation("movie-routed", "movie-routed", previous.movieRouted, candidate.movieRouted, f); err != nil {
		return err
	}
	return validatePopulation("series-routed", "series-routed", previous.seriesRouted, candidate.seriesRouted, f)
}

// absentRoutingClasses names the routing classes a body carries no record for
// ("movie-routed" / "series-routed"), movie side first. It reads censusOf, so a
// class counts as present only when a record can actually RESOLVE in that arr.
// It reports a fact and applies no threshold: zero records for a routing class
// means one whole arr can match nothing, which needs no baseline to read.
// Deliberately limited to the two routing classes - a legitimately small body
// can carry zero typed, season-scoped or special records.
func absentRoutingClasses(records []Record) []string {
	pop := censusOf(records)
	var absent []string
	if pop.movieRouted == 0 {
		absent = append(absent, "movie-routed")
	}
	if pop.seriesRouted == 0 {
		absent = append(absent, "series-routed")
	}
	return absent
}

// arrIdentifierCount returns how many records retain an arr identifier the
// lookup paths actually consume (per HasArrIdentifier). It backs acceptRefresh's
// acceptance guard: the tolerant decoders never fail a record for a missing id.
func arrIdentifierCount(records []Record) int {
	n := 0
	for i := range records {
		if records[i].HasArrIdentifier() {
			n++
		}
	}
	return n
}

// refusedValidators returns the validators of the body the last refusal
// refused, or the zero value when there is none to ask about. Gated on a USABLE
// cache, so the suppression invariant is unchanged: an unusable cache sends no
// validators, and a 304 can never revalidate a map the loader refuses to serve.
func refusedValidators(prev *Cache) httpx.Validators {
	if !cacheUsable(prev.Records) {
		return httpx.Validators{}
	}
	if prev.RefusedETag == "" && prev.RefusedLastModified == "" {
		return httpx.Validators{}
	}
	return httpx.Validators{ETag: prev.RefusedETag, LastModified: prev.RefusedLastModified}
}

// conditionalGet issues a GET with the cached ETag / Last-Modified validators
// via httpx.DoConditional, retrying transient failures. A 304 reports
// NotModified; a 200 returns the bounded body and fresh validators. Validators
// are sent only when there is a usable cached record set (cacheUsable).
func (l *Loader) conditionalGet(ctx context.Context, prev *Cache) (httpx.ConditionalResult, error) {
	// Ask about the REFUSED body when there is one: sending the accepted
	// validators re-downloads the whole ~5.9 MB list for as long as the refusal
	// lasts.
	validators := refusedValidators(prev)
	if validators == (httpx.Validators{}) && cacheUsable(prev.Records) {
		validators = httpx.Validators{ETag: prev.ETag, LastModified: prev.LastModified}
	}
	return httpx.Do(ctx,
		func(ctx context.Context) (httpx.ConditionalResult, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, http.NoBody)
			if err != nil {
				return httpx.ConditionalResult{}, err
			}
			req.Header.Set("User-Agent", appinfo.UserAgent)
			return httpx.DoConditional(l.http, req, validators, maxMapBytes)
		},
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithLabel("mapping"),
		httpx.WithLogger(l.log),
		// Demote httpx's terminal "http retries exhausted" line to Debug: a refresh
		// whose retries ran out always surfaces again from the caller with strictly
		// more context, and demoting keeps the per-attempt retry diagnostics.
		httpx.WithExhaustedLevel(slog.LevelDebug))
}

// maxLoggedUnknownKeys bounds how many unknown override keys the diagnostic
// WARN names: a malformed but accepted-size overrides file can carry enough
// unique keys to render a multi-megabyte log record every cycle, which
// downstream limits may truncate or reject. unknown_key_count carries the
// retained count, keys_truncated marks a non-verbatim list, and count_capped
// marks a count that is a lower bound.
const maxLoggedUnknownKeys = 20

// maxRetainedUnknownKeys bounds how many distinct unknown-key strings the
// parser RETAINS, not just how many the WARN displays: a valid sub-cap file can
// carry hundreds of thousands of skipped rows with distinct unknown keys. One
// extra slot beyond the logged prefix keeps the keys_truncated arithmetic
// truthful; further keys only set overrideSet.unknownOverflow.
const maxRetainedUnknownKeys = maxLoggedUnknownKeys + 1

// maxLoggedKeyBytes bounds one displayed unknown-key name - the whole rendered
// name, keyTruncMarker included, since the marker is charged inside the budget.
const maxLoggedKeyBytes = 64

// keyTruncMarker is the suffix a byte-capped unknown-key name carries so a
// reader can tell a truncated name from an honest one. It is charged INSIDE
// maxLoggedKeyBytes; keys_truncated carries the truncation as a fact, which the
// marker alone cannot prove (a key can end in "..." on its own).
const keyTruncMarker = "..."

// maxLoggedErrorBytes bounds untrusted-input-derived parse-error text before
// it reaches a log emit boundary (the anilist sanitizeUpstreamMessage policy).
const maxLoggedErrorBytes = 200

// maxLoggedDuplicateIDs bounds how many distinct duplicated AniList IDs the
// duplicate-override WARN names; the full distinct count still rides in
// duplicate_count.
const maxLoggedDuplicateIDs = 20

// applyOverrides reads the operator overrides file (if present) and overlays
// each effective record onto the index, keyed by AniList ID. A missing file is
// not an error; an unreadable or malformed file is logged at ERROR and ignored.
// The overlay is WHOLESALE, not a merge, so a record carrying no identifier its
// routed arr consumes replaces a mapped Fribb record with one that resolves to
// nothing. That is left applied - an operator entry wins by design - but it is
// reported, because a mistyped id key is otherwise invisible.
func (l *Loader) applyOverrides(ctx context.Context, idx *Index) {
	if l.overridesPath == "" {
		return
	}
	set, ok := l.readOverrides(ctx)
	if !ok {
		return
	}
	unroutable := 0
	var unroutableIDs []int
	for i := range set.records {
		record := set.records[i]
		if !record.HasArrIdentifier() {
			unroutable++
			if len(unroutableIDs) < maxLoggedDuplicateIDs {
				unroutableIDs = append(unroutableIDs, record.AniListID)
			}
		}
		idx.byAniList[record.AniListID] = record
	}
	if len(set.duplicates) > 0 {
		shown := min(len(set.duplicates), maxLoggedDuplicateIDs)
		l.log.Warn("mapping: duplicate override anilist_ids, last record wins",
			"ids", set.duplicates[:shown],
			"duplicate_count", len(set.duplicates),
			"path", l.overridesPath)
	}
	if set.skipped > 0 {
		l.log.Warn("mapping: overrides with missing or invalid anilist_id skipped", "skipped", set.skipped, "path", l.overridesPath)
	}
	if set.oversized > 0 {
		l.log.Warn("mapping: overrides with oversized id arrays skipped",
			"skipped", set.oversized, "ids", set.oversizedIDs,
			"max_ids", maxOverrideIDsPerRecord, "path", l.overridesPath)
	}
	if unroutable > 0 {
		l.log.Warn("mapping: overrides carry no arr identifier and un-map their entry; check for a mistyped tvdb_id/tmdb_movies/imdb_ids key, and restate the ids when overriding only a type or season",
			"count", unroutable, "ids", unroutableIDs, "path", l.overridesPath)
	}
	if set.applied > 0 {
		l.log.Info("mapping: applied overrides", "count", set.applied)
	}
}

// readOverridesFile reads the overrides file through an os.Root over its own
// directory, so the read can neither be redirected out of /config nor block:
// atomicfile.ReadBoundedInRoot opens with O_NONBLOCK and refuses a non-regular
// inode, where a plain os.Open follows a symlink and blocks indefinitely on a
// writer-less FIFO, past its only context check. A missing file or /config
// still surfaces as fs.ErrNotExist, so the silent no-overrides case is unchanged.
func (l *Loader) readOverridesFile(ctx context.Context) ([]byte, error) {
	dir := filepath.Dir(l.overridesPath)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if clErr := root.Close(); clErr != nil {
			l.log.Warn("mapping: could not close overrides directory handle", "dir", dir, "error", clErr)
		}
	}()
	return atomicfile.ReadBoundedInRoot(ctx, root, filepath.Base(l.overridesPath), maxOverrideBytes)
}

// readOverrides reads and parses the overrides file, returning ok=false for
// every ignored outcome: a cancelled read, a missing file (silently), an
// unreadable or malformed file (logged at ERROR). Unknown keys are diagnosed
// with a bounded WARN but never reject the file. A file-level refusal is an
// ERROR because the file is opt-in - its existence means the operator intends
// those mappings to apply - and the failure persists until they act. NOT wired
// into Cache.RejectedRefreshes, which counts UPSTREAM refresh refusals.
func (l *Loader) readOverrides(ctx context.Context) (overrideSet, bool) {
	data, err := l.readOverridesFile(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return overrideSet{}, false
		}
		if !errors.Is(err, fs.ErrNotExist) {
			l.log.Error("mapping: overrides.json unreadable, pinned mappings not applied; fix the file's permissions or contents, or remove it", "path", l.overridesPath, "error", err)
		}
		return overrideSet{}, false
	}
	set, err := parseOverrides(data)
	if err != nil {
		l.log.Error("mapping: overrides.json malformed, pinned mappings not applied; fix the file's JSON, or remove it", "path", l.overridesPath,
			"error", errors.New(runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes)))
		return overrideSet{}, false
	}
	if len(set.unknown) > 0 {
		l.logUnknownKeys(set.unknown, set.unknownOverflow)
	}
	return set, true
}

// logUnknownKeys emits the bounded unknown-key diagnostic. Full log-bound text
// policy for an operator-controlled JSON key, not just a length bound:
// SanitizeSingleLineCapped replaces unsafe control, bidi and separator runes
// before the byte cap. It also RETURNS the truncation fact, which is what
// keys_truncated needs, since a marker cannot prove a cut. The marker is
// charged inside maxLoggedKeyBytes.
func (l *Loader) logUnknownKeys(unknown []string, capped bool) {
	shown := min(len(unknown), maxLoggedUnknownKeys)
	logged := make([]string, 0, shown)
	shortened := false
	for _, k := range unknown[:shown] {
		safe, cut := runesafe.SanitizeSingleLineCapped(k, maxLoggedKeyBytes, keyTruncMarker)
		shortened = shortened || cut
		logged = append(logged, safe)
	}
	l.log.Warn("mapping: overrides contain unknown keys, ignored",
		"keys", logged,
		"unknown_key_count", len(unknown),
		"count_capped", capped,
		"keys_truncated", capped || len(unknown) > maxLoggedUnknownKeys || shortened,
		"path", l.overridesPath)
}

// overrideSet is parseOverrides' result: the effective overlay plus the
// diagnostics applyOverrides logs. records holds only effective records
// (positive AniList ID, deduplicated last-record-wins); applied counts the
// positive-ID, non-oversized transport rows; skipped counts the non-positive-ID
// rows; duplicates lists each distinct duplicated ID once, on its first repeat,
// so one heavily repeated ID cannot fill the bounded log prefix; unknown is the
// sorted, deduplicated, BOUNDED set of non-canonical keys, with unknownOverflow
// marking further distinct keys seen but not retained.
type overrideSet struct {
	records    []Record
	unknown    []string
	duplicates []int
	// oversizedIDs names the first maxLoggedDuplicateIDs AniList IDs whose record
	// was skipped for an over-cap id array; oversized carries the exact total.
	oversizedIDs    []int
	applied         int
	skipped         int
	oversized       int
	unknownOverflow bool
}

// maxOverrideIDsPerRecord caps one override record's tmdb_movies and imdb_ids
// array lengths, enforced during the token walk BEFORE the element past the cap
// is decoded (decodeCappedArray). The 4 MiB wire bound caps the FILE, not the
// decode amplification. One record maps ONE anime, so 64 is generous headroom;
// an over-cap record is skipped loudly, never silently truncated.
const maxOverrideIDsPerRecord = 64

// maxOverrideRecords caps the effective records parseOverrides retains,
// mirroring the Fribb parser's maxFribbRecords: the 4 MiB wire bound caps the
// file, not the retained amplification of ~250k tiny distinct-ID records. An
// over-cap file routes through readOverrides' malformed-file ERROR, refusing
// the whole overlay - unlike an over-cap RECORD, which is skipped at WARN.
const maxOverrideRecords = 1 << 16

// recordUnknownKey retains one unknown override key for the diagnostic (seen
// dedupes across records), bounded at maxRetainedUnknownKeys so a file of many
// skipped rows cannot amplify diagnostic state; once full, further distinct keys
// only set set.unknownOverflow. Keys arrive in document order.
func (set *overrideSet) recordUnknownKey(key string, seen map[string]struct{}) {
	if set.unknownOverflow {
		return
	}
	if _, dup := seen[key]; dup {
		return
	}
	if len(set.unknown) >= maxRetainedUnknownKeys {
		set.unknownOverflow = true
		return
	}
	seen[key] = struct{}{}
	set.unknown = append(set.unknown, key)
}

// decodeCappedArray decodes one override id array under the
// maxOverrideIDsPerRecord cap, preserving encoding/json's duplicate-key slice
// semantics through bounded.Array's prior argument. An over-cap array reports
// oversized=true after token-skipping its remaining elements and consuming the
// closing bracket, so the record walk stays aligned.
func decodeCappedArray[T any](dec *bounded.Decoder, target *[]T, what string) (oversized bool, err error) {
	decoded, err := bounded.Array(dec, *target, maxOverrideIDsPerRecord, what, func(v *T) error { return dec.Decode(v) })
	if err == nil {
		*target = decoded
		return false, nil
	}
	if !errors.Is(err, bounded.ErrArrayCap) {
		return false, err
	}
	for dec.More() {
		if skipErr := dec.Skip(); skipErr != nil {
			return true, skipErr
		}
	}
	return true, dec.Close()
}

// decodeOverrideRecord walks one override object off the token stream in a
// single bounded pass: the six canonical keys - Record's own json tags, restated
// here because a token walk cannot read them, so a field added to Record must be
// added here too - decode directly into the Record, the id arrays are capped
// BEFORE their 65th element allocates, and an unknown key retains only its name.
// Each array's over-cap state follows the same last-wins rule as its value, and
// the two are tracked independently.
func decodeOverrideRecord(dec *bounded.Decoder, set *overrideSet, seenKeys map[string]struct{}) (record Record, oversized bool, err error) {
	var tmdbOversized, imdbOversized bool
	err = dec.Object(func(key string) error {
		switch {
		case strings.EqualFold(key, "anilist_id"):
			return dec.Decode(&record.AniListID)
		case strings.EqualFold(key, "type"):
			return dec.Decode(&record.Type)
		case strings.EqualFold(key, "tvdb_id"):
			return dec.Decode(&record.TvdbID)
		case strings.EqualFold(key, "season_tvdb"):
			return dec.Decode(&record.SeasonTvdb)
		case strings.EqualFold(key, "tmdb_movies"):
			var arrErr error
			tmdbOversized, arrErr = decodeCappedArray(dec, &record.TmdbMovies, "tmdb_movies")
			return arrErr
		case strings.EqualFold(key, "imdb_ids"):
			var arrErr error
			imdbOversized, arrErr = decodeCappedArray(dec, &record.IMDbIDs, "imdb_ids")
			return arrErr
		default:
			set.recordUnknownKey(key, seenKeys)
			return dec.Skip()
		}
	})
	return record, tmdbOversized || imdbOversized, err
}

// applyRecord decodes the next override record from the token stream and folds
// it into the set: Type is normalized, IMDb ids trimmed and TMDB movie ids
// reduced to positives - the same canonical forms the Fribb decoder produces -
// a zero-AniList-ID record counts as skipped, an over-cap one as oversized, and
// a duplicate ID replaces its earlier record while being reported once.
func (set *overrideSet) applyRecord(dec *bounded.Decoder, seenKeys map[string]struct{}, position map[int]int, reported map[int]struct{}) error {
	record, oversized, err := decodeOverrideRecord(dec, set, seenKeys)
	if err != nil {
		return err
	}
	// Canonical form is Record's own rule (canonicalize), shared with the Fribb
	// producer, so the two paths cannot diverge.
	record.canonicalize()
	if record.AniListID <= 0 {
		// Zero (missing) and negative alike: an indexed negative key matches no
		// SeaDex lookup while still leaking into the reverse arr-ID catalogue.
		set.skipped++
		return nil
	}
	if oversized {
		set.oversized++
		if len(set.oversizedIDs) < maxLoggedDuplicateIDs {
			set.oversizedIDs = append(set.oversizedIDs, record.AniListID)
		}
		return nil
	}
	set.applied++
	if at, dup := position[record.AniListID]; dup {
		if _, done := reported[record.AniListID]; !done {
			reported[record.AniListID] = struct{}{}
			set.duplicates = append(set.duplicates, record.AniListID)
		}
		set.records[at] = record
		return nil
	}
	// Reject BEFORE retaining: a new distinct record allocates both a set.records
	// slot and a position map entry, so the cardinality cap has to fire here. A
	// duplicate at the cap still replaces its earlier record, retaining nothing new.
	if len(set.records) >= maxOverrideRecords {
		return fmt.Errorf("mapping: overrides exceed cap %d records", maxOverrideRecords)
	}
	position[record.AniListID] = len(set.records)
	set.records = append(set.records, record)
	return nil
}

// positiveInts returns in with non-positive entries dropped, matching the
// canonical TmdbMovies form the Fribb decoders guarantee, so an override record
// and a Fribb record agree on the exact TMDB keys downstream lookups use.
func positiveInts(in []int) []int {
	var out []int
	for _, v := range in {
		if v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// parseOverrides decodes the overrides file - a JSON array of Record objects,
// each keyed by its AniList ID - streaming one record at a time so the peak
// allocation tracks the effective overlay, not the transport row count.
// maxOverrideBytes bounds the wire size, but a compact array of semantically
// empty records or a single near-cap record fits under it while whole-value
// materialization would multiply it past the container's memory budget. The
// top-level value must be a JSON array with no trailing data: encoding/json
// would otherwise accept a literal null as a valid empty overlay.
func parseOverrides(data []byte) (overrideSet, error) {
	trimmedData := bytes.TrimSpace(data)
	if len(trimmedData) == 0 || trimmedData[0] != '[' {
		return overrideSet{}, errors.New("mapping: overrides must be a JSON array")
	}
	dec := bounded.NewDecoder(bytes.NewReader(trimmedData), 0)
	if _, err := dec.Open('['); err != nil { // the '['-first-byte guard above rules out null
		return overrideSet{}, err
	}
	var set overrideSet
	seenKeys := make(map[string]struct{})
	position := make(map[int]int) // AniList ID -> index in set.records
	reported := make(map[int]struct{})
	for dec.More() {
		if err := set.applyRecord(dec, seenKeys, position, reported); err != nil {
			return overrideSet{}, err
		}
	}
	if err := dec.Close(); err != nil { // the closing ']'
		return overrideSet{}, err
	}
	if err := dec.End(); err != nil {
		return overrideSet{}, fmt.Errorf("mapping: overrides carry data after the JSON array: %w", err)
	}
	slices.Sort(set.unknown)
	return set, nil
}

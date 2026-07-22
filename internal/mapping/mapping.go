// Package mapping bridges AniList IDs (what SeaDex keys on) to the arr IDs
// Sonarr and Radarr key on (TVDB, TMDB, IMDb), using the Fribb anime-lists
// dataset plus a local overrides file the operator can pin misses in.
//
// The Fribb file is fetched with a conditional GET (ETag / If-Modified-Since)
// and cached: once the caller's refresh window lapses (the app wires a zero
// window, i.e. every cycle) the map is revalidated, so an unchanged multi-MB
// file is a cheap 304 and is never re-downloaded. Overrides are read every
// load and overlaid on top of the built Fribb index (applied last, so an
// operator entry always wins over the upstream mapping).
package mapping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/httpx/v3"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/degradation"
)

const (
	// maxMapBytes bounds the Fribb download before decode (~2.7x the real ~5.9MB body).
	maxMapBytes = 16 << 20
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

// --- Record: the per-entry mapping and its arr-routing predicates ---

// Record is the resolved mapping for one AniList entry: its media type and the
// arr IDs it corresponds to. Fields are ordered for govet fieldalignment.
type Record struct {
	Type       string   `json:"type"`
	IMDbIDs    []string `json:"imdb_ids,omitempty"`
	TmdbMovies []int    `json:"tmdb_movies,omitempty"`
	AniListID  int      `json:"anilist_id"`
	TvdbID     int      `json:"tvdb_id,omitempty"`
	SeasonTvdb int      `json:"season_tvdb,omitempty"`
}

// IsMovie reports whether the entry maps to a Radarr movie (Fribb type MOVIE).
// Every other type (TV, OVA, ONA, SPECIAL, ...) maps to a Sonarr series.
func (r *Record) IsMovie() bool { return r.Type == typeMovie }

// RoutedIDs returns the identifiers the record's routed arr consumes, per the
// HasArrIdentifier routing decision: a MOVIE record yields its TMDB-movie and
// IMDb ids (tvdb zero); every other type yields its TVDB id (movie ids empty).
// It is the single home of the field-to-arr routing branch, so consumers never
// re-implement which identifier fields belong to which arr.
func (r *Record) RoutedIDs() (tvdbID int, tmdbMovies []int, imdbIDs []string) {
	if r.IsMovie() {
		return 0, r.TmdbMovies, r.IMDbIDs
	}
	return r.TvdbID, nil, nil
}

// HasArrIdentifier reports whether the record carries a USABLE identifier
// consumed by the arr selected by its type: TMDB-movie/IMDb for movies, TVDB
// for series. It is the canonical arr-routing predicate shared by the refresh
// acceptance guard, the matcher, and the report's reverse catalogue, so all
// three agree on which identifier fields are meaningful for a record's routed
// arr. Usability is checked per value, not per field shape: the Fribb
// decoders guarantee positive/non-blank ids, but operator overrides construct
// Record through plain encoding/json, so a negative tvdb_id, a zero
// tmdb_movies entry, or a blank imdb id must read as id-less — otherwise it
// would suppress the AniList title fallback while FindByID can never match it.
func (r *Record) HasArrIdentifier() bool {
	tvdb, tmdbMovies, imdbIDs := r.RoutedIDs()
	if tvdb > 0 {
		return true
	}
	for _, id := range tmdbMovies {
		if id > 0 {
			return true
		}
	}
	for _, id := range imdbIDs {
		if strings.TrimSpace(id) != "" {
			return true
		}
	}
	return false
}

// IsSpecial reports whether the entry is an OVA/ONA/special/music video rather
// than a standard TV season or movie, so it can be excluded when the operator
// turns specials off. A match with no type (an entry that resolved to no arr
// item, or one whose AniList format was empty) is treated as non-special; the
// AniList title fallback now sets Type from the AniList format, so a
// title-matched OVA/ONA/special IS filtered when specials are off.
func (r *Record) IsSpecial() bool {
	switch r.Type {
	case "OVA", "ONA", "SPECIAL", "MUSIC":
		return true
	default:
		return false
	}
}

// --- Cache + Index: persisted state and the AniList-ID lookup ---

// Cache is the persisted mapping state: the parsed Fribb records plus the HTTP
// validators and timestamp needed for the next conditional GET.
type Cache struct {
	FetchedAt    time.Time `json:"fetched_at"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Records      []Record  `json:"records,omitempty"`
	// RejectedRefreshes counts consecutive fresh-200 refreshes the acceptance
	// guards (the validation floor, the below-half-size shrink guard, the
	// parse-time record cap) rejected in favour of the stale map. It persists
	// across cycles and restarts, resets to 0 on any accepted refresh or 304,
	// and rides on the *StaleMapError (ConsecutiveRejections) so the scout
	// can escalate its degraded-mapping log at degradation.EscalationThreshold.
	// Transient fetch or parse failures neither advance nor reset it — with
	// one exception: a record-cap breach (errRecordCapExceeded) surfaces as a
	// parse error but is a persistent guard refusal (an over-cap upstream
	// list never self-heals), so it advances the streak like any other guard
	// rejection.
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
// the report's reverse (arr-ID) catalogue — used to tell a library item that is
// recognized anime (present in the Fribb map) but absent from SeaDex from an
// arbitrary non-anime library entry — without materializing a slice copy of all
// ~40k records, which keeps the memory-tight report path lean.
func (i *Index) ForEachRecord(fn func(Record)) {
	if i == nil {
		return
	}
	for _, r := range i.byAniList {
		fn(r)
	}
}

// NewIndex builds an index over records already decoded elsewhere, keyed by
// AniList ID. Production code obtains an Index from Loader.Load; this exists for
// callers (and tests) that already hold a record set.
func NewIndex(records []Record) *Index {
	return buildIndex(records)
}

// deduplicateRecords returns one effective record per AniList ID while
// preserving buildIndex's existing last-record-wins semantics and stable order.
// Records without a positive AniList ID are omitted: buildIndex drops them
// (real AniList IDs are positive, the same contract overrideSet.applyRecord
// enforces), so keeping them here would let cacheUsable and the acceptance
// validators count a larger population (rows and arr identifiers) than the
// effective served index. acceptRefresh runs it before the acceptance
// invariants so row counts and identifier coverage measure the AniList-keyed
// dataset consumers actually receive, not the transport representation.
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

// buildIndex keys records by AniList ID, admitting only positive IDs (the
// same positive-key contract overrideSet.applyRecord enforces; real SeaDex
// lookups use positive AniList IDs, so a zero or negative key could never
// resolve an entry). A later record with the same AniList ID overwrites an
// earlier one (overrides are applied on top afterwards).
func buildIndex(records []Record) *Index {
	byAniList := make(map[int]Record, len(records))
	for _, r := range records {
		if r.AniListID > 0 {
			byAniList[r.AniListID] = r
		}
	}
	return &Index{byAniList: byAniList}
}

// cacheUsable reports whether a cached record set is usable as an effective
// AniList-keyed mapping: after deduplication (which drops non-positive AniList IDs,
// so a JSON-valid state cache such as records:[{}] is not a usable map - and
// whose output indexes bijectively, pinned by
// TestDeduplicateRecordsIndexOracle) the effective set must be non-empty and
// meet the same conservative 1% arr-identifier coverage floor a newly
// accepted refresh must meet (validateRefreshedRecords), without the
// previous-relative type and shrink checks. Every cache-state gate (the
// fresh-cache fast path, staleOrFail, reuseCachedRecords, conditionalGet, and
// acceptRefresh's shrink guard) keys on this predicate so "has cached bytes"
// can never diverge from "has a mapping the consumers can use".
func cacheUsable(records []Record) bool {
	records = deduplicateRecords(records)
	if len(records) == 0 {
		return false
	}
	return arrIdentifierCount(records) >= coverageFloor(len(records))
}

// coverageFloor returns the conservative 1% acceptance floor (ceiling
// division, minimum 1) shared by cacheUsable and validateRefreshedRecords.
func coverageFloor(n int) int { return max(1, (n+99)/100) }

// coverageLost reports the shared loss-relative floor decision applied by the
// type, scope, and routing floors: the previously accepted cache met its own
// floor for the population (prevCount >= previousMinimum) AND the candidate
// falls below the candidate floor (count < minimum) AND below the prior count
// (count < prevCount) - so an additive refresh that merely grows the record
// count never fires it.
func coverageLost(prevCount, count, previousMinimum, minimum int) bool {
	return prevCount >= previousMinimum && count < minimum && count < prevCount
}

// populationCollapsed is the per-population shrink guard the type, scope, and
// routing validators apply beside their loss-relative floors: the previously
// accepted cache carried a meaningful population (prevCount >=
// previousMinimum, the same significance gate coverageLost uses) and the
// candidate retains less than half of it (degradation.ShrinkGuardFactor, the
// shared below-half policy home; multiplication avoids integer-division
// rounding). The 1%-of-body floors catch total loss; this catches the
// MID-BAND, where a corrupted refresh guts most of ONE population (typed
// records 10000 -> 450 in a 40k body) while the record count and every 1%
// floor stay green - accepted, it would silently erase most of the library's
// routing for that population. Deliberately NO auto-accept after a streak:
// by duration alone a persistent poisoning is indistinguishable from a
// legitimate upstream restructuring, and the guard exists for the persistent
// case - the rejection streak escalates to ERROR within ~a day and the
// documented remedy (remove state.json to cold-start onto the new shape)
// applies, exactly like the whole-map shrink guard.
func populationCollapsed(prevCount, count, previousMinimum int) bool {
	return prevCount >= previousMinimum && count*degradation.ShrinkGuardFactor < prevCount
}

// --- Loader: conditional fetch, acceptance guards, stale-map degradation ---

// NewLoader returns a mapping loader. httpClient must be non-nil for any
// loader that will fetch (a loader whose cache is always fresh never touches
// it), url is the Fribb JSON source, overridesPath is the local override file
// (may be absent), refresh is the conditional re-download cadence, and logger
// may be nil.
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
// (match with errors.As) so the caller can log a degraded cycle while still
// comparing against the last good map; any other non-nil error means no
// usable map was returned at all.
func (l *Loader) Load(ctx context.Context, prev *Cache) (Cache, *Index, error) {
	next, err := l.refreshCache(ctx, prev)
	// Build from whatever records survived (fresh, refreshed, or stale prev).
	idx := buildIndex(next.Records)
	l.applyOverrides(ctx, idx)
	return next, idx, err
}

// StaleMapError reports a refresh failure where a stale-but-usable cached map
// was returned: the previous cache held Records, so the cycle may still compare
// against the stale index. Consumers discriminate a degraded-but-comparable
// load from an unusable one via errors.As on this type, instead of probing the
// Cache's Records themselves — the loader owns the usability judgment, so
// operator overrides overlaid on an empty index can never make an unusable map
// look comparable. Fields are ordered for govet fieldalignment.
type StaleMapError struct {
	// cause is the underlying refresh failure; nil for the shrunk-refresh
	// guard, which degrades without a wrapped error.
	cause error
	// msg describes which refresh step failed (e.g. "refresh failed").
	msg string
	// age is how long ago the stale map was fetched, rounded for logging.
	age time.Duration
	// records is the size of the stale-but-usable record set.
	records int
	// rejections is the consecutive acceptance-guard rejection streak
	// (Cache.RejectedRefreshes) including this rejection; 0 when the
	// degradation is a fetch or parse failure rather than a guard rejection.
	rejections int
	// shrunkReturned/shrunkPrevious carry the shrink guard's counts as
	// structured facts (the stale_returned/stale_previous attrs and the
	// Error() parenthetical); zero for every other class. Keeping the live
	// counts OUT of msg keeps stale_reason a fixed-cardinality class
	// discriminator - equality-queryable in Loki like its sibling classes,
	// instead of needing a regex.
	shrunkReturned int
	shrunkPrevious int
}

// Error renders the degradation facts as the single prose line the degraded
// cycle logs. The exact message shape is a pinned log contract
// (stale_map_error_test.go locks it), so edits here change log content.
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
// structured slog key/value pairs (stale_reason, stale_age_seconds,
// stale_records, stale_consecutive_rejections), so callers can emit a
// queryable degraded-cycle log line without parsing the message text.
func (e *StaleMapError) LogAttrs() []any {
	attrs := []any{
		"stale_reason", e.msg,
		"stale_age_seconds", e.age.Seconds(),
		"stale_records", e.records,
		"stale_consecutive_rejections", e.rejections,
	}
	if e.shrunkPrevious > 0 {
		attrs = append(attrs, "stale_returned", e.shrunkReturned, "stale_previous", e.shrunkPrevious)
	}
	return attrs
}

// ConsecutiveRejections reports how many refresh cycles in a row the
// acceptance guards rejected a fresh 200 body, including this one; 0 when the
// degradation is a fetch or parse failure rather than a guard rejection. The
// scout reads it to escalate its existing degraded-mapping log line to ERROR
// at degradation.EscalationThreshold - carrying the streak here keeps that the
// single log site instead of adding a second log line in this package.
func (e *StaleMapError) ConsecutiveRejections() int { return e.rejections }

// staleOrFail returns the stale cache wrapped in a *StaleMapError when prev
// holds a usable record set (cacheUsable; carrying cause when non-nil),
// otherwise the no-cache error — an unusable cache (e.g. a non-empty record
// set that indexes to nothing) must degrade like no cache at all so the scout
// preserves findings instead of comparing against an empty map.
// It collapses refreshCache's repeated degrade-to-stale-or-fail branches into
// one call so each failure site stays flat. The age is clamped to zero: a
// future FetchedAt (clock skew or a corrupt state file) correctly forces
// revalidation, and when that fetch fails the degradation telemetry must not
// report a misleading negative age ("fetched -2h ago").
func staleOrFail(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	if cacheUsable(prev.Records) {
		return *prev, &StaleMapError{
			cause:   cause,
			msg:     staleMsg,
			age:     max(time.Duration(0), time.Since(prev.FetchedAt).Round(time.Second)),
			records: len(prev.Records),
		}
	}
	return *prev, noCache
}

// rejectRefresh degrades an acceptance-guard rejection of a fresh 200 body to
// the stale map via staleOrFail, additionally advancing the persisted
// consecutive-rejection streak (Cache.RejectedRefreshes) and carrying it on
// the *StaleMapError so the scout can escalate its degraded-mapping log after
// degradation.EscalationThreshold consecutive rejections. Only guard rejections
// route here: a fetch or parse failure is a transient outage, not a persistent
// guard refusal, so it neither advances the streak (plain staleOrFail) nor
// resets it (only an accepted refresh or a 304 does).
func rejectRefresh(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	next, err := staleOrFail(prev, staleMsg, cause, noCache)
	if stale, ok := errors.AsType[*StaleMapError](err); ok {
		next.RejectedRefreshes = prev.RejectedRefreshes + 1
		stale.rejections = next.RejectedRefreshes
	}
	return next, err
}

// refreshCache decides whether to reuse, re-validate, or re-download the Fribb
// map and returns the cache to persist. Validator hygiene (the RFC 9110
// field-value grammar plus a 1 KiB cap) lives in httpx.DoConditional, both
// directions: a poisoned validator loaded from a persisted Cache is skipped at
// replay (the refresh degrades to an unconditional GET instead of failing
// net/http's request-write validation forever), and captured validators
// arrive pre-sanitized, so the next accepted 200 replaces any poison still
// sitting in state.json. Until then a bad persisted validator is inert: 304
// and stale returns re-persist it, but it is never sent.
func (l *Loader) refreshCache(ctx context.Context, prev *Cache) (Cache, error) {
	age := time.Since(prev.FetchedAt)
	// age >= 0 rejects a future FetchedAt (clock skew or a corrupt state file):
	// a negative age is never fresh, forcing a revalidating fetch rather than
	// trusting the bad timestamp until it drifts back into range.
	if l.refresh > 0 && age >= 0 && age < l.refresh && cacheUsable(prev.Records) {
		l.log.Debug("mapping: cache fresh, skipping fetch", "records", len(prev.Records), "age", age.Round(time.Second))
		return *prev, nil
	}

	res, err := l.conditionalGet(ctx, prev)
	if err != nil {
		return staleOrFail(prev, "refresh failed", err,
			fmt.Errorf("mapping: initial fetch failed and no cache available: %w", err))
	}
	if res.NotModified {
		return l.reuseCachedRecords(prev)
	}
	return l.acceptRefresh(prev, res)
}

// reuseCachedRecords handles a 304: the upstream is unchanged, so the cached
// records are reused with a bumped timestamp. A cache with no usable record
// set (validator-only, or records that index to nothing) errors instead of
// affirming an unusable map.
func (l *Loader) reuseCachedRecords(prev *Cache) (Cache, error) {
	if !cacheUsable(prev.Records) {
		return *prev, errors.New("mapping: not modified but no cache available")
	}
	l.log.Debug("mapping: not modified, reusing cache", "records", len(prev.Records))
	refreshed := *prev
	refreshed.FetchedAt = time.Now()
	// A 304 is upstream affirmation that the cached map is current, so any
	// acceptance-guard rejection streak ends here.
	if prev.RejectedRefreshes > 0 {
		l.log.Info("mapping: rejection streak ended by 304 revalidation", "ended_rejection_streak", prev.RejectedRefreshes, "records", len(prev.Records))
	}
	refreshed.RejectedRefreshes = 0
	return refreshed, nil
}

// acceptRefresh parses a fresh 200 body and runs the cache-acceptance
// invariants (the parse-time record cap, deduplication, the validation floor,
// and the shrink guard), degrading to the stale map when any step rejects the
// refresh.
func (l *Loader) acceptRefresh(prev *Cache, res httpx.ConditionalResult) (Cache, error) {
	parsed, err := parseFribbForRefresh(res.Body, l.log)
	if err != nil {
		if errors.Is(err, errRecordCapExceeded) {
			// A record-cap breach is a guard rejection, not a transient parse
			// failure: a permanently over-cap upstream list re-downloads the
			// multi-MB body and rejects it every cycle, never self-healing, so
			// the streak must advance for the scout to escalate at
			// degradation.EscalationThreshold instead of degrading at WARN forever.
			return rejectRefresh(prev, "refresh exceeded record cap", err,
				fmt.Errorf("%w and no cache available", err))
		}
		return staleOrFail(prev, "parse failed", err,
			fmt.Errorf("mapping: parse failed and no cache available: %w", err))
	}
	// Collapse duplicate AniList IDs BEFORE any acceptance invariant runs:
	// buildIndex later keeps only the last record per ID, so validating or
	// size-comparing the raw row count would let a body that repeats one ID
	// thousands of times pass every guard and then index to almost nothing.
	records := deduplicateRecords(parsed.records)
	if validationErr := validateRefreshedRecords(prev.Records, records, parsed.elements); validationErr != nil {
		return rejectRefresh(prev, "refresh validation failed", validationErr,
			fmt.Errorf("mapping: %w and no cache available", validationErr))
	}
	// A syntactically valid but sharply truncated refresh (e.g. one record
	// replacing ~40k) can pass the coverage floor above yet silently erase most
	// mappings; treat a below-half-size refresh (degradation.ShrinkGuardFactor,
	// the shared below-half policy home) as part of the cache-acceptance
	// invariant and keep the stale map (multiplication avoids integer-division
	// rounding for odd counts).
	if prevCount := buildIndex(prev.Records).Len(); cacheUsable(prev.Records) && len(records)*degradation.ShrinkGuardFactor < prevCount {
		// The noCache argument is unreachable here (cacheUsable guarantees the
		// stale branch); it exists only to satisfy rejectRefresh's signature.
		// The reason string is FIXED (class-queryable in Loki); the live
		// counts ride as structured fields on the error instead
		// (stale_returned/stale_previous), set post-construction the same way
		// rejectRefresh carries the rejection streak.
		next, err := rejectRefresh(prev, "refresh shrank below half of previous",
			nil, errors.New("mapping: refresh shrank unexpectedly and no cache available"))
		if stale, ok := errors.AsType[*StaleMapError](err); ok {
			stale.shrunkReturned, stale.shrunkPrevious = len(records), prevCount
		}
		return next, err
	}
	attrs := []any{"records", len(records)}
	if prev.RejectedRefreshes > 0 {
		attrs = append(attrs, "ended_rejection_streak", prev.RejectedRefreshes)
	}
	l.log.Info("mapping: refreshed", attrs...)
	// The fresh Cache literal deliberately omits RejectedRefreshes: an
	// accepted refresh resets the streak to zero (see Cache.RejectedRefreshes),
	// mirroring reuseCachedRecords' explicit 304 reset.
	return Cache{
		FetchedAt:    time.Now(),
		Records:      records,
		ETag:         res.Validators.ETag,
		LastModified: res.Validators.LastModified,
	}, nil
}

// validateRefreshedRecords is acceptRefresh's acceptance invariant for a fresh
// 200 body: it rejects a zero-record refresh, one below the AniList-key,
// arr-identifier, or type coverage floors, and one whose individual
// populations (typed, season-scoped, special, movie-/series-routed) collapse
// below half of the previously accepted cache's (populationCollapsed - the
// mid-band the 1% floors cannot see). The tolerant per-record decoders in
// fribb.go deliberately
// zero individual odd fields, so a wholesale upstream loss of the arr-ID
// fields can decode as a full set of otherwise-valid records that no longer
// map to any Sonarr or Radarr item. Accepting that as a successful refresh
// would replace a usable stale map with useless records; require a
// conservative 1% coverage minimum (about 40% of the real Fribb file's
// anilist-keyed records carry one — 8279/20687 measured live 2026-07 — so the
// floor has ~40x headroom and only fires on genuine wholesale degradation),
// computed as a ceiling
// so e.g. 1/199 stays below the documented floor. maxMapBytes bounds the
// decoded body (and thus len(records)), so the +99 cannot overflow.
//
// sourceElements is the top-level element count of the downloaded body
// (parseFribbForRefresh: survivors + skipped-malformed + dropped-keyless,
// BEFORE deduplication). The AniList-key floor validates len(records) against
// it, so destructive filtering and deduplication cannot shrink both the
// numerator and denominator: a first-boot body of 200 rows with one keyed
// record (or 200 duplicates of one ID) is rejected as wholesale key loss
// instead of passing as a "healthy" 1/1 map — the case the previous-relative
// shrink guard cannot catch when there is no previous cache.
func validateRefreshedRecords(previous, records []Record, sourceElements int) error {
	if len(records) == 0 {
		return errors.New("refresh returned zero records")
	}
	keyMinimum := coverageFloor(sourceElements)
	if len(records) < keyMinimum {
		return fmt.Errorf("AniList-key coverage %d/%d is below minimum %d", len(records), sourceElements, keyMinimum)
	}
	minimum := coverageFloor(len(records))
	if covered := arrIdentifierCount(records); covered < minimum {
		return fmt.Errorf("arr identifier coverage %d/%d is below minimum %d", covered, len(records), minimum)
	}
	// An unusable previous cache must degrade like no cache here too: the
	// loader refuses to serve it (cacheUsable gates every other cache-state
	// gate, including acceptRefresh's whole-map shrink guard), so it must
	// not anchor the loss-relative guards either - a corrupted or pre-guard
	// state file could otherwise falsely reject a healthy smaller refresh
	// (populationCollapsed against a map consumers never received), leaving
	// the loader in a permanent no-cache rejection loop.
	if !cacheUsable(previous) {
		return nil
	}
	previous = deduplicateRecords(previous)
	if err := validateTypeCoverage(previous, records, minimum); err != nil {
		return err
	}
	if err := validateScopeCoverage(previous, records, minimum); err != nil {
		return err
	}
	return validateRoutingCoverage(previous, records, minimum)
}

// validateTypeCoverage rejects a candidate refresh that lost type coverage
// relative to the previously accepted cache. A wholesale upstream loss of the
// type field (flexString zeroes any non-string shape) re-routes every MOVIE
// record to Sonarr via its parent tvdb_id while still passing the
// arr-identifier floor and the shrink guard — but only a LOSS is a
// degradation. fribb.go's tolerant contract lets an absent/odd type survive
// as the safe non-movie (Sonarr) default, so the floor is relative to the
// previously accepted cache: it fires only when that cache was itself
// type-rich (met the same 1% floor) AND the candidate carries fewer typed
// records than the cache did — an additive refresh that merely grows the
// record count (raising the ceiling-derived minimum) without losing any typed
// record is the catalogue growing, not type data degrading. An established
// type-sparse cache or a first boot against a type-sparse catalogue is the
// catalogue's valid shape, not a regression to reject.
func validateTypeCoverage(previous, records []Record, minimum int) error {
	if len(previous) == 0 {
		return nil
	}
	previousTyped := typedRecordCount(previous)
	previousMinimum := coverageFloor(len(previous))
	typed := typedRecordCount(records)
	if coverageLost(previousTyped, typed, previousMinimum, minimum) {
		return fmt.Errorf("type coverage %d/%d is below minimum %d (previous cache carried %d typed records)", typed, len(records), minimum, previousTyped)
	}
	if populationCollapsed(previousTyped, typed, previousMinimum) {
		return fmt.Errorf("typed records collapsed below half of previous (%d of previous %d)", typed, previousTyped)
	}
	return nil
}

// validateScopeCoverage rejects a candidate refresh that wholesale lost the
// mapping metadata controlling comparison scope, relative to the previously
// accepted cache. The typed and routing floors cannot see it: a body whose
// season objects all decode to SeasonTvdb=0 (flex decoding zeroes odd shapes)
// or whose OVA/SPECIAL labels all changed to the still-valid TV keeps AniList
// ids, arr ids, types, and both routing populations healthy — yet align.Scope
// then compares ordinary cours whole-series instead of their mapped season,
// and exclude_specials/season-0 bucketing is silently bypassed. Same
// loss-relative shape as the type and routing floors: each semantic
// population (positive-season, special-type) is guarded only when the prior
// cache met the floor for it, and an additive refresh that merely grows the
// record count passes.
func validateScopeCoverage(previous, records []Record, minimum int) error {
	if len(previous) == 0 {
		return nil
	}
	previousMinimum := coverageFloor(len(previous))
	prevSeasons, seasons := positiveSeasonCount(previous), positiveSeasonCount(records)
	if coverageLost(prevSeasons, seasons, previousMinimum, minimum) {
		return fmt.Errorf("positive-season coverage %d/%d is below minimum %d (previous cache carried %d season-scoped records)", seasons, len(records), minimum, prevSeasons)
	}
	if populationCollapsed(prevSeasons, seasons, previousMinimum) {
		return fmt.Errorf("season-scoped records collapsed below half of previous (%d of previous %d)", seasons, prevSeasons)
	}
	prevSpecials, specials := specialRecordCount(previous), specialRecordCount(records)
	if coverageLost(prevSpecials, specials, previousMinimum, minimum) {
		return fmt.Errorf("special-type coverage %d/%d is below minimum %d (previous cache carried %d special records)", specials, len(records), minimum, prevSpecials)
	}
	if populationCollapsed(prevSpecials, specials, previousMinimum) {
		return fmt.Errorf("special records collapsed below half of previous (%d of previous %d)", specials, prevSpecials)
	}
	return nil
}

// positiveSeasonCount returns how many records carry a positive TVDB season.
// It backs validateScopeCoverage's season floor: align.Scope keys season-exact
// comparison on SeasonTvdb > 0, so a refresh that wholesale zeroed the season
// field silently degrades every mapped cour to whole-series scope.
func positiveSeasonCount(records []Record) int {
	n := 0
	for i := range records {
		if records[i].SeasonTvdb > 0 {
			n++
		}
	}
	return n
}

// specialRecordCount returns how many records carry a special type (IsSpecial:
// OVA/ONA/SPECIAL/MUSIC). It backs validateScopeCoverage's special floor:
// exclude_specials filtering and the report's season-0 bucketing key on
// IsSpecial, so a refresh that relabeled every special as TV silently routes
// them through whole-series scope while passing the typed and routing floors.
func specialRecordCount(records []Record) int {
	n := 0
	for i := range records {
		if records[i].IsSpecial() {
			n++
		}
	}
	return n
}

// validateRoutingCoverage rejects a candidate refresh that collapsed a
// routing population relative to the previously accepted cache. The typed
// floor validates syntactic presence of Type, but routing recognizes only
// MOVIE and sends every other value to Sonarr — so a wrong-but-string schema
// change (all movie types renamed to FILM, or every record stamped MOVIE)
// retains 100% typed coverage while silently routing an entire side of the
// catalogue to the wrong arr. Guard the operational invariant instead:
// preservation of both routing populations (MOVIE-routed and non-MOVIE),
// relative to the previously accepted cache. For each side that met the
// conservative 1% floor in that cache, reject a candidate whose side falls
// below the candidate floor AND below its prior count — an additive catalogue
// update that keeps both sides populated passes, and individual or future
// non-movie labels stay legal because every non-MOVIE type counts toward the
// same side.
func validateRoutingCoverage(previous, records []Record, minimum int) error {
	if len(previous) == 0 {
		return nil
	}
	previousMinimum := coverageFloor(len(previous))
	prevMovies, prevOthers := routingCounts(previous)
	movies, others := routingCounts(records)
	if coverageLost(prevMovies, movies, previousMinimum, minimum) {
		return fmt.Errorf("movie-routed coverage %d/%d is below minimum %d (previous cache carried %d movie-routed records)", movies, len(records), minimum, prevMovies)
	}
	if populationCollapsed(prevMovies, movies, previousMinimum) {
		return fmt.Errorf("movie-routed records collapsed below half of previous (%d of previous %d)", movies, prevMovies)
	}
	if coverageLost(prevOthers, others, previousMinimum, minimum) {
		return fmt.Errorf("series-routed coverage %d/%d is below minimum %d (previous cache carried %d series-routed records)", others, len(records), minimum, prevOthers)
	}
	if populationCollapsed(prevOthers, others, previousMinimum) {
		return fmt.Errorf("series-routed records collapsed below half of previous (%d of previous %d)", others, prevOthers)
	}
	return nil
}

// routingCounts returns how many records route to each arr side AND can
// actually resolve there: MOVIE records (Radarr) and everything else (Sonarr,
// per RoutedIDs' branch), counting only records that retain an identifier
// their routed arr consumes (HasArrIdentifier). It backs
// validateRefreshedRecords' routing-distribution floor: consumers rely on
// both resolvable populations surviving a refresh, not on type labels alone —
// a candidate that keeps every type but loses one side's usable ids must read
// as a collapse of that side, not as healthy routing.
func routingCounts(records []Record) (movies, others int) {
	for i := range records {
		if !records[i].HasArrIdentifier() {
			continue
		}
		if records[i].IsMovie() {
			movies++
		} else {
			others++
		}
	}
	return movies, others
}

// typedRecordCount returns how many records carry a non-empty normalized
// type. It backs the type-coverage acceptance floor: routing (IsMovie /
// RoutedIDs / IsSpecial) keys entirely on Type, so a refresh whose records
// wholesale lost the field cannot be trusted to route to the right arr even
// when its id fields survive.
func typedRecordCount(records []Record) int {
	n := 0
	for i := range records {
		if records[i].Type != "" {
			n++
		}
	}
	return n
}

// arrIdentifierCount returns how many records retain an arr identifier the
// lookup paths actually consume (TMDB-movie/IMDb for movies, TVDB otherwise,
// per HasArrIdentifier). It backs acceptRefresh's acceptance guard: the
// tolerant Fribb decoders never fail a record for a missing id, so a refresh
// can only be trusted to map to the arrs when enough records still carry the
// identifier their routed arr actually consumes.
func arrIdentifierCount(records []Record) int {
	n := 0
	for i := range records {
		if records[i].HasArrIdentifier() {
			n++
		}
	}
	return n
}

// conditionalGet issues a GET with the cached ETag / Last-Modified validators
// via httpx.DoConditional, retrying transient failures. A 304 reports
// NotModified; a 200 returns the bounded body and fresh validators. Validators
// are sent only when there is a usable cached record set (cacheUsable): a
// validator-only or effectively-empty cache must force a full 200 download
// rather than being eligible for a 304 that would reuse an unusable map.
func (l *Loader) conditionalGet(ctx context.Context, prev *Cache) (httpx.ConditionalResult, error) {
	validators := httpx.Validators{}
	if cacheUsable(prev.Records) {
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
		httpx.WithLogger(l.log))
}

// --- Overrides: the operator overlay file ---

// maxLoggedUnknownKeys bounds how many unknown override keys the diagnostic
// WARN names. A malformed but accepted-size overrides file can carry enough
// unique keys to render a multi-megabyte log record every cycle, which
// downstream Docker/Alloy/Loki limits may truncate or reject — hiding the
// diagnostic while amplifying log volume. unknown_key_count carries the
// retained count (itself bounded by maxRetainedUnknownKeys), with
// keys_truncated marking an elided tail and count_capped marking a count
// that is a lower bound.
const maxLoggedUnknownKeys = 20

// maxRetainedUnknownKeys bounds how many distinct unknown-key strings the
// parser RETAINS for the diagnostic, not just how many the WARN displays: a
// valid sub-cap overrides file can carry hundreds of thousands of tiny
// skipped rows with distinct unknown keys (skipped rows are exempt from the
// effective-record and per-record ID caps), and unbounded retention would
// fan them into map/slice/string entries plus an O(n log n) sort on every
// mapping load. One extra slot beyond the logged prefix keeps the existing
// keys_truncated arithmetic truthful; further keys only set
// overrideSet.unknownOverflow.
const maxRetainedUnknownKeys = maxLoggedUnknownKeys + 1

// maxLoggedKeyBytes bounds one displayed unknown-key name. unknown_key_count
// is exact unless count_capped is true, in which case it is a lower bound.
const maxLoggedKeyBytes = 64

// maxLoggedDuplicateIDs bounds how many distinct duplicated AniList IDs the
// duplicate-override WARN names; the full distinct count still rides in
// duplicate_count.
const maxLoggedDuplicateIDs = 20

// applyOverrides reads the operator overrides file (if present) and overlays
// each effective record onto the index, keyed by AniList ID. A missing file is
// not an error; a malformed file is logged and ignored so a bad override never
// blocks a cycle.
func (l *Loader) applyOverrides(ctx context.Context, idx *Index) {
	if l.overridesPath == "" {
		return
	}
	set, ok := l.readOverrides(ctx)
	if !ok {
		return
	}
	for _, record := range set.records {
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
			"skipped", set.oversized, "max_ids", maxOverrideIDsPerRecord, "path", l.overridesPath)
	}
	if set.applied > 0 {
		l.log.Info("mapping: applied overrides", "count", set.applied)
	}
}

// readOverrides reads and parses the overrides file, returning ok=false for
// every ignored outcome: a cancelled read, a missing file (silently), an
// unreadable or malformed file (logged). Unknown keys are diagnosed with a
// bounded WARN but never reject the file.
func (l *Loader) readOverrides(ctx context.Context) (overrideSet, bool) {
	data, err := atomicfile.ReadBounded(ctx, l.overridesPath, maxOverrideBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return overrideSet{}, false
		}
		if !errors.Is(err, fs.ErrNotExist) {
			l.log.Warn("mapping: overrides unreadable, ignoring", "path", l.overridesPath, "error", err)
		}
		return overrideSet{}, false
	}
	set, err := parseOverrides(data)
	if err != nil {
		l.log.Warn("mapping: overrides malformed, ignoring", "path", l.overridesPath, "error", err)
		return overrideSet{}, false
	}
	if len(set.unknown) > 0 {
		l.logUnknownKeys(set.unknown, set.unknownOverflow)
	}
	return set, true
}

// logUnknownKeys emits the bounded unknown-key diagnostic. Full log-bound
// text policy for an operator-controlled JSON key, not just a length bound:
// SanitizeSingleLine replaces unsafe C0/C1 controls, bidi controls, DEL, and
// line separators before the byte cap, so a key carrying such runes cannot
// smuggle terminal-control or direction-override text into the log stream
// (the same runesafe policy the indexer's logParam and sanitizeUpstreamText
// apply at their emit boundaries).
func (l *Loader) logUnknownKeys(unknown []string, capped bool) {
	shown := min(len(unknown), maxLoggedUnknownKeys)
	logged := make([]string, 0, shown)
	shortened := false
	for _, k := range unknown[:shown] {
		k = runesafe.SanitizeSingleLine(k)
		if len(k) > maxLoggedKeyBytes {
			k = runesafe.CapBytes(k, maxLoggedKeyBytes) + "..."
			shortened = true
		}
		logged = append(logged, k)
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
// (positive AniList ID, deduplicated last-record-wins), so its size is
// bounded by the distinct usable IDs in the file rather than the transport
// row count. applied counts the positive-ID, non-oversized transport rows
// (duplicate rows included, matching the pre-streaming overlay arithmetic);
// skipped counts the non-positive-ID rows discarded during the stream
// (oversized rows are counted separately in oversized); duplicates lists each
// distinct duplicated AniList ID once, on its first repeated occurrence, so
// one heavily repeated ID cannot fill the bounded log prefix and hide later
// duplicated IDs; unknown is the sorted, deduplicated, BOUNDED set of keys
// outside overrideKeys (at most maxRetainedUnknownKeys entries, retained
// during the stream so skipped rows cannot amplify diagnostic state), with
// unknownOverflow marking that further distinct unknown keys were seen but
// not retained.
type overrideSet struct {
	records         []Record
	unknown         []string
	duplicates      []int
	applied         int
	skipped         int
	oversized       int
	unknownOverflow bool
}

// maxOverrideIDsPerRecord caps one override record's tmdb_movies and imdb_ids
// array lengths after normalization. The 4 MiB wire bound caps the FILE, not
// the retained amplification: a compact, syntactically valid record can
// otherwise fan a few bytes per entry into hundreds of thousands of retained
// slice entries and reverse-catalogue index insertions (a local configuration
// denial of service). One record maps ONE anime - the largest real franchise
// overrides run a few dozen ids - so 64 is generous headroom; an over-cap
// record is skipped loudly (the oversized counter's WARN), never silently
// truncated.
const maxOverrideIDsPerRecord = 64

// maxOverrideRecords caps the effective records parseOverrides retains,
// mirroring the Fribb parser's maxFribbRecords ceiling: the 4 MiB wire bound
// caps the file, not the retained amplification of ~250k tiny distinct-ID
// records fanned into set.records, the position map, and the live index.
// Skipped rows (non-positive IDs, e.g. semantically empty objects) are
// discarded during the stream and never retained, so they stay uncapped.
// An over-cap file errors out and routes through readOverrides' existing
// malformed-file WARN (overlay ignored loudly).
const maxOverrideRecords = 1 << 16

// knownOverrideKey reports whether key names an overrideKeys entry under the
// same case-insensitive matching encoding/json applies when decoding into
// Record, so a case-variant canonical key (e.g. "TYPE") that the typed decode
// accepts is never misreported as unknown and ignored.
func knownOverrideKey(key string) bool {
	for canonical := range overrideKeys {
		if strings.EqualFold(key, canonical) {
			return true
		}
	}
	return false
}

// collectUnknownKeys scans one raw override record for keys outside
// overrideKeys, appending first occurrences to set.unknown (seen dedupes
// across records). Retention is bounded at maxRetainedUnknownKeys so a file
// of many skipped rows with distinct unknown keys cannot amplify diagnostic
// state; once full, further distinct keys only set set.unknownOverflow (and
// an already-overflowed set skips the scan entirely). A record's fresh keys
// are sorted before retention so the bounded set is deterministic regardless
// of map iteration order. A raw-unmarshal error is ignored: the typed decode
// in parseOverrides reports real errors.
func (set *overrideSet) collectUnknownKeys(raw json.RawMessage, seen map[string]struct{}) {
	if set.unknownOverflow {
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	fresh := make([]string, 0, len(m))
	for k := range m {
		if knownOverrideKey(k) {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		fresh = append(fresh, k)
	}
	slices.Sort(fresh)
	for _, k := range fresh {
		if len(set.unknown) >= maxRetainedUnknownKeys {
			set.unknownOverflow = true
			return
		}
		seen[k] = struct{}{}
		set.unknown = append(set.unknown, k)
	}
}

// applyRecord decodes one raw override record and folds it into the set:
// unknown keys are collected, Type is normalized, IMDb ids are trimmed and
// TMDB movie ids reduced to positives - the same canonical forms the Fribb
// decoder produces (so exact-key lookups agree with HasArrIdentifier's
// trimmed usability view), a zero-AniList-ID record is
// counted as skipped, and a duplicate ID replaces its earlier record
// (last-record-wins) while being reported once in set.duplicates.
func (set *overrideSet) applyRecord(raw json.RawMessage, seenKeys map[string]struct{}, position map[int]int, reported map[int]struct{}) error {
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		return err
	}
	set.collectUnknownKeys(raw, seenKeys)
	record.Type = NormalizeType(record.Type)
	record.IMDbIDs = trimmed(record.IMDbIDs)
	record.TmdbMovies = positiveInts(record.TmdbMovies)
	if record.AniListID <= 0 {
		// Zero (missing) and negative alike: encoding/json decodes a negative
		// anilist_id the tolerant Fribb decoders can never produce, and an
		// indexed negative key matches no SeaDex lookup while still leaking
		// into the reverse arr-ID catalogue (phantom recognized-anime rows).
		set.skipped++
		return nil
	}
	if len(record.TmdbMovies) > maxOverrideIDsPerRecord || len(record.IMDbIDs) > maxOverrideIDsPerRecord {
		set.oversized++
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
	position[record.AniListID] = len(set.records)
	set.records = append(set.records, record)
	return nil
}

// positiveInts returns in with non-positive entries dropped, matching the
// canonical TmdbMovies form the Fribb decoders guarantee (flexInt zeroes
// negatives and non-numerics, intSlice drops zeros), so an override record
// and a Fribb record agree on the exact TMDB keys downstream lookups and the
// report's reverse catalogue index.
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
// empty records (e.g. a million {} rows) fits under it while three
// whole-document materializations ([]Record, the unknown-key scan's
// []map[string]json.RawMessage, and the overlay's row-sized seen map) would
// multiply it well past the container's memory budget before every row is
// discarded as unusable. Each record is instead decoded from its own
// RawMessage, its unknown keys collected, its Type normalized (so an operator
// can write "movie" or "tv"), and then either discarded (zero AniList ID,
// counted in skipped) or folded into the deduplicated effective set with
// last-record-wins. The top-level value must be a JSON array with no trailing
// data: encoding/json would otherwise accept a literal null into a nil
// []Record without error, silently treating a clobbered overrides file as a
// valid empty overlay instead of routing it through readOverrides'
// malformed-file warning.
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
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return overrideSet{}, err
		}
		if err := set.applyRecord(raw, seenKeys, position, reported); err != nil {
			return overrideSet{}, err
		}
		if len(set.records) > maxOverrideRecords {
			return overrideSet{}, fmt.Errorf("mapping: overrides exceed cap %d records", maxOverrideRecords)
		}
	}
	if err := dec.Close(); err != nil { // the closing ']'
		return overrideSet{}, err
	}
	if err := dec.End(); err != nil {
		return overrideSet{}, errors.New("mapping: overrides carry data after the JSON array")
	}
	slices.Sort(set.unknown)
	return set, nil
}

// overrideKeys is the set of keys an overrides record may carry (Record's JSON tags).
var overrideKeys = map[string]struct{}{
	"anilist_id": {}, "type": {}, "tvdb_id": {}, "tmdb_movies": {}, "imdb_ids": {}, "season_tvdb": {},
}

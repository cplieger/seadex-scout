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
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/appinfo"
)

const (
	// maxMapBytes bounds the Fribb download before decode (~2.7x the real ~5.9MB body).
	maxMapBytes = 16 << 20
	// maxOverrideBytes bounds the local overrides file.
	maxOverrideBytes = 4 << 20
	maxAttempts      = 3
	baseDelay        = time.Second
)

// maxValidatorBytes bounds one persisted HTTP validator (ETag/Last-Modified).
// Real validators are well under 1 KiB; Go's transport admits response
// headers up to ~10 MB, and an unbounded hostile validator would be
// re-persisted into state.json every cycle (past the state Save cap it
// blocks the whole state from persisting). An over-long validator is
// dropped, so the next refresh is simply an unconditional 200.
const maxValidatorBytes = 1 << 10

// Loader fetches and caches the Fribb map and overlays the overrides file.
type Loader struct {
	http          *http.Client
	log           *slog.Logger
	url           string
	overridesPath string
	refresh       time.Duration
}

// boundedValidator returns v, or empty when it exceeds maxValidatorBytes,
// logging the drop so a persistently oversized upstream validator (which
// forces an unconditional full re-download every cycle) is observable.
func (l *Loader) boundedValidator(name, v string) string {
	if len(v) > maxValidatorBytes {
		l.log.Warn("mapping: dropping oversized HTTP validator, next refresh will be unconditional", "validator", name, "length", len(v))
		return ""
	}
	return v
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

// HasArrIdentifier reports whether the record carries an identifier consumed by
// the arr selected by its type: TMDB-movie/IMDb for movies, TVDB for series.
// It is the canonical arr-routing predicate shared by the refresh acceptance
// guard, the matcher, and the report's reverse catalogue, so all three agree
// on which identifier fields are meaningful for a record's routed arr.
func (r *Record) HasArrIdentifier() bool {
	tvdb, tmdbMovies, imdbIDs := r.RoutedIDs()
	return tvdb != 0 || len(tmdbMovies) > 0 || len(imdbIDs) > 0
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
	// can escalate its degraded-mapping log at RejectionEscalationThreshold.
	// Transient fetch or parse failures neither advance nor reset it — with
	// one exception: a record-cap breach (errRecordCapExceeded) surfaces as a
	// parse error but is a persistent guard refusal (an over-cap upstream
	// list never self-heals), so it advances the streak like any other guard
	// rejection.
	RejectedRefreshes int `json:"rejected_refreshes,omitempty"`
}

// RejectionEscalationThreshold is the consecutive-rejection streak
// (Cache.RejectedRefreshes) at which the scout escalates its degraded-mapping
// log from WARN to ERROR. It is the single home of the shared escalation
// policy - tolerate 8 consecutive degraded cycles, about a day at the default
// 3h cadence, before escalating - which the scout's shrunk-walk threshold
// (shrunkWalkEscalationThreshold in internal/scout) references rather than
// re-declaring: long enough to ride out a transient upstream oddity, short
// enough that a persistent guard rejection (which re-downloads the ~5.9MB
// body every cycle against an aging cache and never self-heals) alerts
// instead of degrading silently forever. The remedy is operator-driven:
// inspect upstream, and if the change is legitimate remove state.json to
// cold-start onto the new map.
const RejectionEscalationThreshold = 8

// ShrinkGuardFactor is the shrink guards' trigger fraction: a refreshed data
// set that would replace the prior one with fewer than 1/ShrinkGuardFactor of
// its entries - below half, at the default 2 - is treated as a suspicious
// truncation rather than a real change, keeping the prior data and never
// auto-accepting. It is the single home of the shared below-half policy,
// applied by acceptRefresh's mapping shrink guard and referenced by the
// scout's library shrink guard (libraryShrinkFactor in internal/scout) rather
// than re-declared there.
const ShrinkGuardFactor = 2

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
// acceptRefresh runs it before the acceptance invariants so row counts and
// identifier coverage measure the AniList-keyed dataset consumers actually
// receive, not the transport representation.
func deduplicateRecords(records []Record) []Record {
	last := make(map[int]int, len(records))
	for i := range records {
		last[records[i].AniListID] = i
	}
	out := make([]Record, 0, len(last))
	for i := range records {
		if last[records[i].AniListID] == i {
			out = append(out, records[i])
		}
	}
	return out
}

// buildIndex keys records by AniList ID. A later record with the same AniList
// ID overwrites an earlier one (overrides are applied on top afterwards).
func buildIndex(records []Record) *Index {
	byAniList := make(map[int]Record, len(records))
	for _, r := range records {
		if r.AniListID != 0 {
			byAniList[r.AniListID] = r
		}
	}
	return &Index{byAniList: byAniList}
}

// cacheUsable reports whether a cached record set is usable as an effective
// AniList-keyed mapping: after deduplication it must build a non-empty index
// (buildIndex drops zero AniList IDs, so a JSON-valid state cache such as
// records:[{}] is not a usable map) and meet the same conservative 1%
// arr-identifier coverage floor a newly accepted refresh must meet
// (validateRefreshedRecords), without the previous-relative type and shrink
// checks. Every cache-state gate (the fresh-cache fast path, staleOrFail,
// reuseCachedRecords, conditionalGet, and acceptRefresh's shrink guard) keys
// on this predicate so "has cached bytes" can never diverge from "has a
// mapping the consumers can use".
func cacheUsable(records []Record) bool {
	records = deduplicateRecords(records)
	if buildIndex(records).Len() == 0 {
		return false
	}
	return arrIdentifierCount(records) >= coverageFloor(len(records))
}

// coverageFloor returns the conservative 1% acceptance floor (ceiling
// division, minimum 1) shared by cacheUsable and validateRefreshedRecords.
func coverageFloor(n int) int { return max(1, (n+99)/100) }

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
// re-read and applied on top. When a refresh fails but prev holds records, it
// returns the stale index with a *StaleMapError (match with errors.As) so the
// caller can log a degraded cycle while still comparing against the last good
// map; any other non-nil error means no usable map was returned at all.
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
}

// Error renders the degradation facts as the single prose line the degraded
// cycle logs. The exact message shape is a pinned log contract
// (stale_map_error_test.go locks it), so edits here change log content.
func (e *StaleMapError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("mapping: %s, using stale map (%d records, fetched %s ago): %v", e.msg, e.records, e.age, e.cause)
	}
	return fmt.Sprintf("mapping: %s, using stale map (%d records, fetched %s ago)", e.msg, e.records, e.age)
}

// Unwrap exposes the underlying refresh failure for errors.Is/As chains.
func (e *StaleMapError) Unwrap() error { return e.cause }

// LogAttrs returns the degradation facts Error() flattens into prose as
// structured slog key/value pairs (stale_reason, stale_age_seconds,
// stale_records, stale_consecutive_rejections), so callers can emit a
// queryable degraded-cycle log line without parsing the message text.
func (e *StaleMapError) LogAttrs() []any {
	return []any{
		"stale_reason", e.msg,
		"stale_age_seconds", e.age.Seconds(),
		"stale_records", e.records,
		"stale_consecutive_rejections", e.rejections,
	}
}

// ConsecutiveRejections reports how many refresh cycles in a row the
// acceptance guards rejected a fresh 200 body, including this one; 0 when the
// degradation is a fetch or parse failure rather than a guard rejection. The
// scout reads it to escalate its existing degraded-mapping log line to ERROR
// at RejectionEscalationThreshold - carrying the streak here keeps that the
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
// RejectionEscalationThreshold consecutive rejections. Only guard rejections
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
// map and returns the cache to persist.
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
			// RejectionEscalationThreshold instead of degrading at WARN forever.
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
	// mappings; treat a below-half-size refresh (ShrinkGuardFactor, the shared
	// below-half policy home) as part of the cache-acceptance invariant and
	// keep the stale map (multiplication avoids integer-division rounding for
	// odd counts).
	if prevCount := buildIndex(prev.Records).Len(); cacheUsable(prev.Records) && len(records)*ShrinkGuardFactor < prevCount {
		// The noCache argument is unreachable here (cacheUsable guarantees the
		// stale branch); it exists only to satisfy rejectRefresh's signature.
		return rejectRefresh(prev,
			fmt.Sprintf("refresh returned %d records, less than half of previous %d", len(records), prevCount),
			nil, errors.New("mapping: refresh shrank unexpectedly and no cache available"))
	}
	l.log.Info("mapping: refreshed", "records", len(records))
	// The fresh Cache literal deliberately omits RejectedRefreshes: an
	// accepted refresh resets the streak to zero (see Cache.RejectedRefreshes),
	// mirroring reuseCachedRecords' explicit 304 reset.
	return Cache{
		FetchedAt:    time.Now(),
		Records:      records,
		ETag:         l.boundedValidator("etag", res.Validators.ETag),
		LastModified: l.boundedValidator("last_modified", res.Validators.LastModified),
	}, nil
}

// validateRefreshedRecords is acceptRefresh's acceptance invariant for a fresh
// 200 body: it rejects a zero-record refresh and one below the AniList-key,
// arr-identifier, or type coverage floors. The tolerant per-record decoders in
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
	previous = deduplicateRecords(previous)
	if err := validateTypeCoverage(previous, records, minimum); err != nil {
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
	if previousTyped >= previousMinimum && typed < minimum && typed < previousTyped {
		return fmt.Errorf("type coverage %d/%d is below minimum %d (previous cache carried %d typed records)", typed, len(records), minimum, previousTyped)
	}
	return nil
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
	if prevMovies >= previousMinimum && movies < minimum && movies < prevMovies {
		return fmt.Errorf("movie-routed coverage %d/%d is below minimum %d (previous cache carried %d movie-routed records)", movies, len(records), minimum, prevMovies)
	}
	if prevOthers >= previousMinimum && others < minimum && others < prevOthers {
		return fmt.Errorf("series-routed coverage %d/%d is below minimum %d (previous cache carried %d series-routed records)", others, len(records), minimum, prevOthers)
	}
	return nil
}

// routingCounts returns how many records route to each arr side: MOVIE
// records (Radarr) and everything else (Sonarr, per RoutedIDs' branch). It
// backs validateRefreshedRecords' routing-distribution floor: consumers rely
// on both populations surviving a refresh, not on per-record type syntax.
func routingCounts(records []Record) (movies, others int) {
	for i := range records {
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
// diagnostic while amplifying log volume. The full count still rides in
// unknown_key_count, with keys_truncated marking an elided tail.
const maxLoggedUnknownKeys = 20

// maxLoggedKeyBytes bounds one displayed unknown-key name; the full
// count still rides in unknown_key_count.
const maxLoggedKeyBytes = 64

// applyOverrides reads the operator overrides file (if present) and overlays
// each record onto the index, keyed by AniList ID. A missing file is not an
// error; a malformed file is logged and ignored so a bad override never blocks
// a cycle.
func (l *Loader) applyOverrides(ctx context.Context, idx *Index) {
	if l.overridesPath == "" {
		return
	}
	overrides, ok := l.readOverrides(ctx)
	if !ok {
		return
	}
	applied := overlayRecords(idx, overrides)
	if skipped := len(overrides) - applied; skipped > 0 {
		l.log.Warn("mapping: overrides missing anilist_id skipped", "skipped", skipped, "path", l.overridesPath)
	}
	if applied > 0 {
		l.log.Info("mapping: applied overrides", "count", applied)
	}
}

// readOverrides reads and parses the overrides file, returning ok=false for
// every ignored outcome: a cancelled read, a missing file (silently), an
// unreadable or malformed file (logged). Unknown keys are diagnosed with a
// bounded WARN but never reject the file.
func (l *Loader) readOverrides(ctx context.Context) ([]Record, bool) {
	data, err := atomicfile.ReadBounded(ctx, l.overridesPath, maxOverrideBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false
		}
		if !errors.Is(err, fs.ErrNotExist) {
			l.log.Warn("mapping: overrides unreadable, ignoring", "path", l.overridesPath, "error", err)
		}
		return nil, false
	}
	overrides, unknown, err := parseOverrides(data)
	if err != nil {
		l.log.Warn("mapping: overrides malformed, ignoring", "path", l.overridesPath, "error", err)
		return nil, false
	}
	if len(unknown) > 0 {
		shown := min(len(unknown), maxLoggedUnknownKeys)
		logged := make([]string, 0, shown)
		shortened := false
		for _, k := range unknown[:shown] {
			if len(k) > maxLoggedKeyBytes {
				k = runesafe.CapBytes(k, maxLoggedKeyBytes) + "..."
				shortened = true
			}
			logged = append(logged, k)
		}
		l.log.Warn("mapping: overrides contain unknown keys, ignored",
			"keys", logged,
			"unknown_key_count", len(unknown),
			"keys_truncated", len(unknown) > maxLoggedUnknownKeys || shortened,
			"path", l.overridesPath)
	}
	return overrides, true
}

// overlayRecords overlays each record with a non-zero AniList ID onto the
// index and returns how many were applied; zero-ID records are skipped (the
// caller reports the skip count).
func overlayRecords(idx *Index, records []Record) int {
	applied := 0
	for _, record := range records {
		if record.AniListID == 0 {
			continue
		}
		idx.byAniList[record.AniListID] = record
		applied++
	}
	return applied
}

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

// unknownOverrideKeys scans the raw overrides JSON for keys outside
// overrideKeys and returns them sorted and de-duplicated; a raw-unmarshal
// error yields nil (the typed decode in parseOverrides reports real errors).
func unknownOverrideKeys(data []byte) []string {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var unknown []string
	for _, m := range raw {
		for k := range m {
			if knownOverrideKey(k) {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	return unknown
}

// parseOverrides decodes the overrides file: a JSON array of Record objects,
// each keyed by its AniList ID. The Type is normalized to upper case so an
// operator can write "movie" or "tv". It also returns the sorted set of
// unknown keys found in any record (e.g. upstream Fribb spellings like
// "imdb_id"), so the caller can warn instead of silently dropping them.
// The top-level value must be a JSON array: encoding/json would otherwise
// accept a literal null into a nil []Record without error, silently treating
// a clobbered overrides file as a valid empty overlay instead of routing it
// through readOverrides' malformed-file warning.
func parseOverrides(data []byte) ([]Record, []string, error) {
	trimmedData := bytes.TrimSpace(data)
	if len(trimmedData) == 0 || trimmedData[0] != '[' {
		return nil, nil, errors.New("mapping: overrides must be a JSON array")
	}
	var records []Record
	if err := json.Unmarshal(trimmedData, &records); err != nil {
		return nil, nil, err
	}
	unknown := unknownOverrideKeys(trimmedData)
	for i := range records {
		records[i].Type = NormalizeType(records[i].Type)
	}
	return records, unknown, nil
}

// overrideKeys is the set of keys an overrides record may carry (Record's JSON tags).
var overrideKeys = map[string]struct{}{
	"anilist_id": {}, "type": {}, "tvdb_id": {}, "tmdb_movies": {}, "imdb_ids": {}, "season_tvdb": {},
}

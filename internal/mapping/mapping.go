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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
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

// DefaultURL is the Fribb anime-list-mini.json endpoint - the AniList<->arr
// ID bridge. The variant choice (mini, not the reduced files) is Fribb
// contract knowledge and lives here beside fribb.go's shape decoders; the
// wiring site (build.go) references it.
const DefaultURL = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"

const (
	// DefaultRefresh is the reuse-if-fresh window for the Fribb map. 0
	// revalidates every cycle: each cycle issues a conditional GET
	// (ETag/If-Modified-Since), so an unchanged map (the common case, since Fribb
	// updates ~weekly) is a cheap 304 with no re-download, while a change is picked
	// up within one cycle instead of lagging a fixed cadence. A failed
	// revalidation is harmless (the persisted cache is reused stale-on-error and
	// the next cycle retries), and the full ~5.9 MB download still happens only
	// when Fribb actually changes, so per-cycle revalidation stays cheap. It is
	// Fribb contract knowledge, so it lives here beside DefaultURL; the wiring
	// site (build.go) references it.
	DefaultRefresh = 0

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
// Every other type (TV, OVA, ONA, SPECIAL, ...) maps to a Sonarr series. The
// token classification itself lives in the shared mediatype leaf.
func (r *Record) IsMovie() bool { return mediatype.IsMovie(r.Type) }

// RoutedIDs returns the identifiers the record's routed arr consumes, per the
// HasArrIdentifier routing decision: a MOVIE record yields its TMDB-movie and
// IMDb ids (tvdb zero); every other type yields its TVDB id (movie ids empty).
// It is the single home of the field-to-arr routing branch, so consumers never
// re-implement which identifier fields belong to which arr.
//
// The TYPE LABEL is what routes here. A record's unambiguous movie TMDB ids are
// separately reachable through MovieTMDBIDs regardless of that label, which is
// what lets the ID bridge resolve the ~300 live Fribb records that carry a
// movie id under a non-MOVIE type (h-f9/l-f73); this function deliberately does
// not fold that in, so "which fields does my routed arr consume" keeps one
// answer.
func (r *Record) RoutedIDs() (tvdbID int, tmdbMovies []int, imdbIDs []string) {
	if r.IsMovie() {
		return 0, r.MovieTMDBIDs(),
			usableIDs(r.IMDbIDs, func(s string) bool { return strings.TrimSpace(s) != "" })
	}
	// Zero out a non-usable TVDB id here so the usability rule has ONE home:
	// callers do a presence check, never a policy check. An operator override
	// decodes through plain encoding/json, so a negative tvdb_id can reach a
	// hand-built Record even though both producers canonicalize.
	if r.TvdbID <= 0 {
		return 0, nil, nil
	}
	return r.TvdbID, nil, nil
}

// MovieTMDBIDs returns the record's usable themoviedb_id.movie ids REGARDLESS of
// its type label, for the ID-bridge sites that need cross-type movie evidence.
//
// A TMDB *movie* id is a Radarr id by construction: it names a row in TMDB's
// movie namespace, which is disjoint from the TV namespace RoutedIDs' series arm
// routes. Fribb's object-form themoviedb_id.movie list is decoded for every
// record type, and the live upstream body carries ~300 keyed records shaped
// non-MOVIE type + no tvdb_id + a positive movie id (a split AniList<->arr
// mapping, common for films/OVAs) - so keying only on the type label lost the
// operator's Radarr copy in BOTH directions of the bridge (h-f9/l-f73).
//
// It is deliberately NOT the IMDb list: TVDB reuses a film's IMDb id on the
// parent series, so a non-MOVIE record's IMDb id claims nothing - that is the
// arr-consistency rule RoutedIDs enforces and the collision this narrower
// evidence does not reopen.
//
// Both directions of the bridge read this one method - match's FindByID (the
// secondary movie lookup) and match's reverse Catalogue - so which ids count as
// cross-type movie evidence cannot drift between them.
func (r *Record) MovieTMDBIDs() []int {
	return usableIDs(r.TmdbMovies, func(id int) bool { return id > 0 })
}

// usableIDs drops the non-usable values from an id slice, returning the input
// unchanged when every value is usable (the overwhelmingly common case: both
// Fribb decoders already canonicalize, and only an operator override can carry
// a zero/blank entry). It keeps the usability POLICY in this one place, so
// RoutedIDs' documented "callers do a presence check, never a policy check"
// contract holds for the slices exactly as it already does for the scalar.
func usableIDs[T any](in []T, usable func(T) bool) []T {
	for i, v := range in {
		if usable(v) {
			continue
		}
		out := make([]T, 0, len(in)-1)
		out = append(out, in[:i]...)
		for _, rest := range in[i+1:] {
			if usable(rest) {
				out = append(out, rest)
			}
		}
		return out
	}
	return in
}

// HasArrIdentifier reports whether the record carries a USABLE identifier
// consumed by the arr selected by its type: TMDB-movie/IMDb for movies, TVDB
// for series. It is the canonical arr-routing predicate shared by the refresh
// acceptance guard, the matcher, and the report's reverse catalogue, so all
// three agree on which identifier fields are meaningful for a record's routed
// arr. Usability is checked per value by RoutedIDs, not per field shape: the
// Fribb decoders guarantee positive/non-blank ids, but operator overrides
// construct Record through plain encoding/json, so a negative tvdb_id, a zero
// tmdb_movies entry, or a blank imdb id must read as id-less — otherwise it
// would suppress the AniList title fallback while FindByID can never match it.
func (r *Record) HasArrIdentifier() bool {
	tvdb, tmdbMovies, imdbIDs := r.RoutedIDs()
	// RoutedIDs canonicalizes every id kind it returns, so this is a presence
	// check over usable ids, never a second copy of the usability policy.
	return tvdb > 0 || len(tmdbMovies) > 0 || len(imdbIDs) > 0
}

// IsSpecial reports whether the entry is an OVA/ONA/special/music video rather
// than a standard TV season or movie, so it can be excluded when the operator
// turns specials off. A match with no type (an entry that resolved to no arr
// item, or one whose AniList format was empty) is treated as non-special; the
// AniList title fallback now sets Type from the AniList format, so a
// title-matched OVA/ONA/special IS filtered when specials are off. The token
// classification lives in the shared mediatype leaf, so the anilist client
// cannot admit a token this predicate has never heard of.
func (r *Record) IsSpecial() bool { return mediatype.IsSpecial(r.Type) }

// HasMappedSeason reports whether the record carries a positive Fribb TVDB
// season - the predicate align's scope resolution keys season-exact comparison on, and
// validateScopeCoverage's season floor counts.
func (r *Record) HasMappedSeason() bool { return r.SeasonTvdb > 0 }

// canonicalize applies Record's canonical field forms - the single home of the
// rule both producers (the Fribb decoders and the overrides overlay) must
// agree on, so exact-key lookups and the reverse arr-ID catalogue cannot see
// two differently-shaped Records for the same anime. It is idempotent: on the
// Fribb path the tolerant decoders already emit these forms, so the call is a
// no-op that pins the agreement structurally.
func (r *Record) canonicalize() {
	r.Type = normalizeType(r.Type)
	r.IMDbIDs = trimmed(r.IMDbIDs)
	r.TmdbMovies = positiveInts(r.TmdbMovies)
	r.TvdbID = max(0, r.TvdbID)
	r.SeasonTvdb = max(0, r.SeasonTvdb)
}

// --- Cache + Index: persisted state and the AniList-ID lookup ---

// Cache is the persisted mapping state: the parsed Fribb records plus the HTTP
// validators and timestamp needed for the next conditional GET.
type Cache struct {
	FetchedAt    time.Time `json:"fetched_at"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Records      []Record  `json:"records,omitempty"`
	// RejectedRefreshes is the persisted streak of consecutive persistent
	// refresh refusals. A fresh 200 advances it when the acceptance guards
	// (the validation floor, the below-half-size shrink guard, the parse-time
	// record cap, the aggregate identifier budget) reject it in favour of the
	// stale map. It persists across cycles and restarts, resets
	// to 0 on any accepted refresh or on a 304 that revalidates a USABLE
	// cache, and rides on the *StaleMapError
	// (the rejections field, surfaced as stale_consecutive_rejections by
	// LogAttrs) so the scout can escalate its degraded-mapping
	// log at degradation.EscalationThreshold.
	// It advances even when no usable stale cache exists (a first boot whose
	// every refresh is refused), because the streak describes the upstream, not
	// the cache; the scout therefore reads it off this Cache rather than
	// depending on a *StaleMapError to carry it.
	//
	// A TRANSIENT failure - one that can succeed on the next attempt - neither
	// advances nor resets it. Four classes are persistent guard refusals that
	// surface as fetch or parse errors and so DO advance it, because each one
	// re-downloads the multi-MB body and refuses it every cycle without ever
	// self-healing: a record-cap breach (errRecordCapExceeded), an aggregate
	// identifier-budget breach (errIdentifierBudgetExceeded), a body over the
	// download size cap (httpx.ResponseTooLargeError), and a non-array
	// top-level document (errNotJSONArray - content-shape evidence, since
	// truncation cannot change a body's first token). Mid-stream truncation and
	// every other malformed-body class stays transient. A 304 answered to a
	// request that carried NO validators also advances it (conditionalGet
	// suppresses them whenever the cache is unusable, so such a 304 is a
	// protocol violation that repeats identically every cycle) - which is why
	// the reset above is scoped to a 304 over a usable cache.
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

// indexedRecordCount returns how many records survive into the served index
// (distinct positive AniList IDs, per buildIndex). It is the single spelling
// of the count every `records` / stale_records log attribute reports, so a
// persisted cache written before deduplication (duplicate or non-positive
// rows) can never over-report the size of the map consumers actually
// receive, and no caller has to build a whole Record-valued index to count.
func indexedRecordCount(records []Record) int {
	seen := make(map[int]struct{}, len(records))
	for i := range records {
		if records[i].AniListID > 0 {
			seen[records[i].AniListID] = struct{}{}
		}
	}
	return len(seen)
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
// division, minimum 1) shared by cacheUsable and validateRefreshedRecords -
// the latter deriving BOTH the candidate floor (from the refreshed record
// count) and the previous-cache significance gate every population guard is
// judged against (from the deduplicated previous cache) with it.
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
// candidate retains less than half of it (degradation.Shrunk, the shared
// below-half policy home). The 1%-of-body floors catch total loss; this catches the
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
	return prevCount >= previousMinimum && degradation.Shrunk(count, prevCount)
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
			records: indexedRecordCount(prev.Records),
		}
	}
	return *prev, noCache
}

// rejectRefresh degrades a persistent refresh refusal to the stale map via
// staleOrFail, additionally advancing the persisted consecutive-rejection
// streak (Cache.RejectedRefreshes) and carrying it on the *StaleMapError so
// the scout can escalate its degraded-mapping log after
// degradation.EscalationThreshold consecutive rejections. Fresh-200
// acceptance-guard rejections and a protocol-violating validator-less 304 over
// an unusable cache route here; a transient fetch or parse failure does not, so
// it neither advances the streak (plain staleOrFail) nor resets it. The streak
// resets only on an accepted refresh or a 304 that revalidates a usable cache.
//
// The streak advances even when there is NO usable stale cache to return. It is
// persisted state about the upstream, not about the cache: on a first boot whose
// every refresh is refused (a poisoned or restructured body), gating the
// increment on a usable cache would freeze the streak at 0 and leave the loader
// WARNing forever while the feed and comparison stay disabled - the exact
// never-self-heals condition the streak exists to escalate. The scout reads the
// streak off the returned Cache, so escalation no longer depends on a
// *StaleMapError being available to carry it.
func rejectRefresh(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	next, err := staleOrFail(prev, staleMsg, cause, noCache)
	next.RejectedRefreshes = prev.RejectedRefreshes + 1
	if stale, ok := errors.AsType[*StaleMapError](err); ok {
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
	// Normalize the optional previous cache: Load(ctx, nil) is the natural
	// representation of "no persisted cache" on first use, and the pointer is
	// read-only on every path below, so an empty Cache makes nil take the
	// ordinary initial-fetch route instead of panicking on prev.FetchedAt.
	if prev == nil {
		prev = &Cache{}
	}
	age := time.Since(prev.FetchedAt)
	// age >= 0 rejects a future FetchedAt (clock skew or a corrupt state file):
	// a negative age is never fresh, forcing a revalidating fetch rather than
	// trusting the bad timestamp until it drifts back into range.
	if l.refresh > 0 && age >= 0 && age < l.refresh && cacheUsable(prev.Records) {
		l.log.Debug("mapping: cache fresh, skipping fetch", "records", indexedRecordCount(prev.Records), "age", age.Round(time.Second))
		return *prev, nil
	}

	res, err := l.conditionalGet(ctx, prev)
	if err != nil {
		if _, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok {
			// A persistent guard refusal (Cache.RejectedRefreshes): the cap is
			// deterministic on upstream SIZE, so it never self-heals.
			return rejectRefresh(prev, "refresh exceeded size cap", err,
				fmt.Errorf("mapping: refresh exceeded size cap and no cache available: %w", err))
		}
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
		// A 304 answered to a request that carried NO validators (conditionalGet
		// suppresses them whenever the cache is unusable) is an upstream or
		// intermediary protocol violation, not a transient outage: it repeats
		// identically every cycle and never self-heals without the operator, so it
		// advances the streak like the size cap rather than leaving it frozen at 0.
		// The returned error is unchanged - staleOrFail passes noCache through
		// verbatim for an unusable cache and constructs no *StaleMapError, so no
		// new stale_reason class enters the log vocabulary.
		return rejectRefresh(prev, "not modified without a usable cache", nil,
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
			// A persistent guard refusal (Cache.RejectedRefreshes): a
			// permanently over-cap upstream list never self-heals.
			return rejectRefresh(prev, "refresh exceeded record cap", err,
				fmt.Errorf("%w and no cache available", err))
		}
		if errors.Is(err, errIdentifierBudgetExceeded) {
			// A persistent guard refusal (Cache.RejectedRefreshes): the
			// aggregate identifier budget truncates the tail of the list, so
			// accepting the prefix would persist a knowably incomplete map
			// that every count floor still passes.
			return rejectRefresh(prev, "refresh exceeded identifier budget", err,
				fmt.Errorf("%w and no cache available", err))
		}
		// The no-first-token case (an empty or whitespace-only body) never reaches here:
		// parseFribbForRefresh classifies it at the source as a transient empty-body parse
		// failure (fribb.go's io.EOF arm) rather than wrapping errNotJSONArray.
		if errors.Is(err, errNotJSONArray) {
			// A persistent guard refusal (Cache.RejectedRefreshes): a moved
			// top-level shape is content evidence, not transport damage, so it
			// never self-heals - unlike mid-stream truncation below.
			err = errors.New(runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes))
			return rejectRefresh(prev, "refresh not a JSON array", err,
				fmt.Errorf("mapping: %w and no cache available", err))
		}
		err = errors.New(runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes))
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
	// mappings; treat a below-half-size refresh (degradation.Shrunk, the
	// shared below-half policy home) as part of the cache-acceptance
	// invariant and keep the stale map.
	if prevCount := indexedRecordCount(prev.Records); cacheUsable(prev.Records) && degradation.Shrunk(len(records), prevCount) {
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
	// previous_records is the baseline the absolute count needs: degradation.Shrunk
	// rejects only BELOW half, so an accepted refresh may legitimately retain as
	// little as exactly half of the previous map and would otherwise read like any
	// other success. revalidatable reports whether the fresh response carried a
	// validator to persist: with none (or with a persisted validator httpx refused
	// at replay) every following cycle re-downloads the whole body instead of
	// taking a cheap 304, which is DefaultRefresh's documented cost assumption
	// failing silently.
	attrs := []any{
		"records", len(records),
		"previous_records", indexedRecordCount(prev.Records),
		"revalidatable", res.Validators.ETag != "" || res.Validators.LastModified != "",
	}
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
// conservative 1% coverage minimum (about 19% of the real Fribb file's
// source elements carry one — 8279/~42868 measured live 2026-07 — so the
// floor has ~19x headroom and only fires on genuine wholesale degradation),
// computed as a ceiling
// so e.g. 1/199 stays below the documented floor. maxMapBytes and
// maxFribbRecords bound sourceElements, so the +99 cannot overflow.
//
// records MUST already be deduplicated - acceptRefresh calls
// deduplicateRecords before this call. Every quantity derived from
// len(records) (the candidate 1% minimum and the AniList-key numerator)
// measures the effective AniList-keyed set consumers receive; against a raw
// row count a body repeating one ID would clear all of them. previous is
// deduplicated here, so only the candidate carries the precondition.
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
	// Anchor the arr floor on the SOURCE element count, like the AniList-key floor above: deriving
	// it from the already-key-filtered candidate would let the two floors compose
	// multiplicatively, admitting a body with 1% of its keys and 1% of THOSE carrying an
	// identifier (0.01% effective coverage) as a healthy refresh whenever there is no usable
	// previous cache to anchor the loss-relative guards on.
	if covered := arrIdentifierCount(records); covered < keyMinimum {
		return fmt.Errorf("arr identifier coverage %d/%d is below minimum %d", covered, sourceElements, keyMinimum)
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
	// One significance gate for every population: the previously accepted
	// cache's own 1% floor, derived once so the type, scope, and routing
	// guards cannot drift apart on which basis they judge "the prior cache
	// carried a meaningful population".
	previousMinimum := coverageFloor(len(previous))
	floors := acceptanceFloors{total: len(records), previousMinimum: previousMinimum, minimum: minimum}
	if err := validateTypeCoverage(previous, records, floors); err != nil {
		return err
	}
	if err := validateScopeCoverage(previous, records, floors); err != nil {
		return err
	}
	return validateRoutingCoverage(previous, records, floors)
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
func validateTypeCoverage(previous, records []Record, f acceptanceFloors) error {
	return validatePopulation("type", "typed", typedRecordCount(previous), typedRecordCount(records), f)
}

// acceptanceFloors carries the three per-refresh quantities every population
// guard shares: the candidate record total the rejection messages quote, the
// previous cache's 1% significance gate, and the candidate's own 1% floor.
// Threading them as one named value keeps the five validatePopulation call
// sites from restating an identical three-int tail in which a transposed
// member still compiles and silently inverts a guard.
type acceptanceFloors struct {
	total           int
	previousMinimum int
	minimum         int
}

// validatePopulation applies the shared pair of per-population guards every
// semantic population (typed, season-scoped, special, movie-routed,
// series-routed) is checked with: the loss-relative floor (coverageLost) and
// the below-half shrink guard (populationCollapsed). floorNoun and
// collapseNoun carry each population's existing error vocabulary so the
// rejection messages stay byte-identical to the pre-extraction text.
//
// The three per-refresh floor quantities travel as one acceptanceFloors value
// rather than a positional int tail every call site restates.
func validatePopulation(floorNoun, collapseNoun string, prevCount, count int, f acceptanceFloors) error {
	if coverageLost(prevCount, count, f.previousMinimum, f.minimum) {
		return fmt.Errorf("%s coverage %d/%d is below minimum %d (previous cache carried %d %s records)", floorNoun, count, f.total, f.minimum, prevCount, collapseNoun)
	}
	if populationCollapsed(prevCount, count, f.previousMinimum) {
		return fmt.Errorf("%s records collapsed below half of previous (%d of previous %d)", collapseNoun, count, prevCount)
	}
	return nil
}

// validateScopeCoverage rejects a candidate refresh that wholesale lost the
// mapping metadata controlling comparison scope, relative to the previously
// accepted cache. The typed and routing floors cannot see it: a body whose
// season objects all decode to SeasonTvdb=0 (flex decoding zeroes odd shapes)
// or whose OVA/SPECIAL labels all changed to the still-valid TV keeps AniList
// ids, arr ids, types, and both routing populations healthy — yet align
// then compares ordinary cours whole-series instead of their mapped season,
// and exclude_specials/season-0 bucketing is silently bypassed. Same
// loss-relative shape as the type and routing floors: each semantic
// population (positive-season, special-type) is guarded only when the prior
// cache met the floor for it, and an additive refresh that merely grows the
// record count passes.
func validateScopeCoverage(previous, records []Record, f acceptanceFloors) error {
	if err := validatePopulation("positive-season", "season-scoped", positiveSeasonCount(previous), positiveSeasonCount(records), f); err != nil {
		return err
	}
	return validatePopulation("special-type", "special", specialRecordCount(previous), specialRecordCount(records), f)
}

// positiveSeasonCount returns how many records carry a positive TVDB season.
// It backs validateScopeCoverage's season floor: align keys season-exact
// comparison on SeasonTvdb > 0, so a refresh that wholesale zeroed the season
// field silently degrades every mapped cour to whole-series scope.
func positiveSeasonCount(records []Record) int {
	n := 0
	for i := range records {
		if records[i].HasMappedSeason() {
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
func validateRoutingCoverage(previous, records []Record, f acceptanceFloors) error {
	prevMovies, prevOthers := routingCounts(previous)
	movies, others := routingCounts(records)
	if err := validatePopulation("movie-routed", "movie-routed", prevMovies, movies, f); err != nil {
		return err
	}
	return validatePopulation("series-routed", "series-routed", prevOthers, others, f)
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
// keys_truncated marking that the displayed list is not verbatim — either an
// elided tail or an individual key name byte-capped at maxLoggedKeyBytes
// (rendered with a trailing "...") — and count_capped marking a count
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

// maxLoggedErrorBytes bounds untrusted-input-derived parse-error text before
// it reaches a log emit boundary (the anilist sanitizeUpstreamMessage policy).
const maxLoggedErrorBytes = 200

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
			"skipped", set.oversized, "ids", set.oversizedIDs,
			"max_ids", maxOverrideIDsPerRecord, "path", l.overridesPath)
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
		l.log.Warn("mapping: overrides malformed, ignoring", "path", l.overridesPath,
			"error", errors.New(runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedErrorBytes)))
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
// outside the six canonical override keys (at most maxRetainedUnknownKeys
// entries, retained
// during the stream so skipped rows cannot amplify diagnostic state), with
// unknownOverflow marking that further distinct unknown keys were seen but
// not retained.
type overrideSet struct {
	records    []Record
	unknown    []string
	duplicates []int
	// oversizedIDs names the first maxLoggedDuplicateIDs AniList IDs whose
	// record was skipped for an over-cap id array, so the operator can find
	// the offending rows in a large overrides file; oversized still carries
	// the exact total.
	oversizedIDs    []int
	applied         int
	skipped         int
	oversized       int
	unknownOverflow bool
}

// maxOverrideIDsPerRecord caps one override record's tmdb_movies and imdb_ids
// array lengths, enforced during the token walk BEFORE the element past the
// cap is decoded (decodeCappedArray). The 4 MiB wire bound caps the FILE, not
// the decode amplification: a compact, syntactically valid record can
// otherwise fan a few bytes per entry into hundreds of thousands of transient
// and retained slice entries and reverse-catalogue index insertions (a local
// configuration denial of service). One record maps ONE anime - the largest
// real franchise overrides run a few dozen ids - so 64 is generous headroom;
// an over-cap record is skipped loudly (the oversized counter's WARN), never
// silently truncated.
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

// recordUnknownKey retains one unknown override key for the diagnostic (seen
// dedupes across records). Retention is bounded at maxRetainedUnknownKeys so
// a file of many skipped rows with distinct unknown keys cannot amplify
// diagnostic state; once full, further distinct keys only set
// set.unknownOverflow. Keys arrive in document order off the token stream,
// so retention is deterministic without a per-record sort.
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
// semantics through bounded.Array's prior argument (a JSON null yields nil,
// matching Unmarshal's null-into-slice). An over-cap array reports
// oversized=true after token-skipping its remaining elements (they are never
// decoded or allocated) and consuming the closing bracket, so the record walk
// stays aligned and the caller counts the record oversized instead of
// materializing hundreds of thousands of compact ids before a length check.
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
// single bounded pass: the six canonical keys (matched with strings.EqualFold
// for encoding/json's case-insensitive field fallback, duplicate keys merging
// last-wins like Unmarshal) decode directly into the Record, the id arrays
// are capped BEFORE their 65th element allocates (decodeCappedArray), and an
// unknown key retains only its name (recordUnknownKey) while its value is
// token-skipped - never materialized into a map[string]json.RawMessage. This
// replaces the former whole-record json.Unmarshal plus independent raw
// unknown-key decode, whose pre-check allocations a valid compact near-cap
// record could amplify into severe memory pressure (CWE-770).
//
// The over-cap state of each id array follows the SAME last-wins rule as its
// value: duplicate keys are decoded in document order, so a later valid
// occurrence REPLACES an earlier over-cap one (an override spelling
// tmdb_movies twice, the second time with one id, has an effective value of
// that one id and must not be skipped as oversized). The two arrays are
// tracked independently so a valid duplicate of one field can never clear the
// other's over-cap state.
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

// applyRecord decodes the next override record from the token stream
// (decodeOverrideRecord: one bounded walk collecting unknown keys and capping
// the id arrays before allocation) and folds it into the set: Type is
// normalized, IMDb ids are trimmed and TMDB movie ids reduced to positives -
// the same canonical forms the Fribb decoder produces (so exact-key lookups
// agree with HasArrIdentifier's trimmed usability view), a zero-AniList-ID
// record is counted as skipped, an over-cap record is counted as oversized,
// and a duplicate ID replaces its earlier record (last-record-wins) while
// being reported once in set.duplicates.
func (set *overrideSet) applyRecord(dec *bounded.Decoder, seenKeys map[string]struct{}, position map[int]int, reported map[int]struct{}) error {
	record, oversized, err := decodeOverrideRecord(dec, set, seenKeys)
	if err != nil {
		return err
	}
	// Canonical form is Record's own rule (canonicalize), shared with the
	// Fribb producer, so a negative tvdb_id/season_tvdb or a non-positive
	// tmdb id cannot diverge between the two paths.
	record.canonicalize()
	if record.AniListID <= 0 {
		// Zero (missing) and negative alike: encoding/json decodes a negative
		// anilist_id the tolerant Fribb decoders can never produce, and an
		// indexed negative key matches no SeaDex lookup while still leaking
		// into the reverse arr-ID catalogue (phantom recognized-anime rows).
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
	// Reject BEFORE retaining: a new distinct record allocates both a
	// set.records slot and a position map entry, so the cardinality cap has to
	// fire here rather than after the append (which would let the first
	// over-cap record cross the documented ceiling). A duplicate at the cap
	// still replaces its earlier record above - it retains nothing new.
	if len(set.records) >= maxOverrideRecords {
		return fmt.Errorf("mapping: overrides exceed cap %d records", maxOverrideRecords)
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
// empty records (e.g. a million {} rows) or a single near-cap record carrying
// hundreds of thousands of compact ids or distinct unknown keys fits under it
// while whole-value materializations would multiply it well past the
// container's memory budget before the row is discarded. Each record is
// instead walked token by token (applyRecord/decodeOverrideRecord: id arrays
// capped before allocation, unknown-key values skipped), its Type normalized
// (so an operator can write "movie" or "tv"), and then either discarded (zero
// AniList ID, counted in skipped; over-cap arrays, counted in oversized) or
// folded into the deduplicated effective set with last-record-wins. The
// top-level value must be a JSON array with no trailing data: encoding/json
// would otherwise accept a literal null into a nil []Record without error,
// silently treating a clobbered overrides file as a valid empty overlay
// instead of routing it through readOverrides' malformed-file warning.
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

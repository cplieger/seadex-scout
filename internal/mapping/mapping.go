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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/httpx/v2"
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

// RejectionEscalationThreshold is the consecutive-rejection streak
// (Cache.RejectedRefreshes) at which the scout escalates its degraded-mapping
// log from WARN to ERROR: 8 cycles is about a day at the default 3h cadence -
// long enough to ride out a transient upstream oddity, short enough that a
// persistent guard rejection (which re-downloads the ~5.9MB body every cycle
// against an aging cache and never self-heals) alerts instead of degrading
// silently forever. The remedy is operator-driven: inspect upstream, and if
// the change is legitimate remove state.json to cold-start onto the new map.
const RejectionEscalationThreshold = 8

// Cache is the persisted mapping state: the parsed Fribb records plus the HTTP
// validators and timestamp needed for the next conditional GET.
type Cache struct {
	FetchedAt    time.Time `json:"fetched_at"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Records      []Record  `json:"records,omitempty"`
	// RejectedRefreshes counts consecutive fresh-200 refreshes the acceptance
	// guards (the validation floor, the below-half-size shrink guard)
	// rejected in favour of the stale map. It persists across cycles and
	// restarts, resets to 0 on any accepted refresh or 304, and rides on the
	// *StaleMapError (ConsecutiveRejections) so the scout can escalate its
	// degraded-mapping log at RejectionEscalationThreshold. Transient fetch
	// or parse failures neither advance nor reset it.
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

// Loader fetches and caches the Fribb map and overlays the overrides file.
type Loader struct {
	http          *http.Client
	log           *slog.Logger
	url           string
	overridesPath string
	refresh       time.Duration
}

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

// Error reproduces the pre-typed-error message text so log content is
// unchanged.
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
// holds records (carrying cause when non-nil), otherwise the no-cache error.
// It collapses refreshCache's repeated degrade-to-stale-or-fail branches into
// one call so each failure site stays flat.
func staleOrFail(prev *Cache, staleMsg string, cause, noCache error) (Cache, error) {
	if len(prev.Records) > 0 {
		return *prev, &StaleMapError{
			cause:   cause,
			msg:     staleMsg,
			age:     time.Since(prev.FetchedAt).Round(time.Second),
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
	if l.refresh > 0 && age >= 0 && age < l.refresh && len(prev.Records) > 0 {
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
// records are reused with a bumped timestamp. A validator-only cache with no
// records is unusable and errors instead.
func (l *Loader) reuseCachedRecords(prev *Cache) (Cache, error) {
	if len(prev.Records) == 0 {
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
// invariants (deduplication, the validation floor, and the shrink guard),
// degrading to the stale map when any step rejects the refresh.
func (l *Loader) acceptRefresh(prev *Cache, res httpx.ConditionalResult) (Cache, error) {
	records, err := parseFribb(res.Body, l.log)
	if err != nil {
		return staleOrFail(prev, "parse failed", err,
			fmt.Errorf("mapping: parse failed and no cache available: %w", err))
	}
	// Collapse duplicate AniList IDs BEFORE any acceptance invariant runs:
	// buildIndex later keeps only the last record per ID, so validating or
	// size-comparing the raw row count would let a body that repeats one ID
	// thousands of times pass every guard and then index to almost nothing.
	records = deduplicateRecords(records)
	if validationErr := validateRefreshedRecords(records); validationErr != nil {
		return rejectRefresh(prev, "refresh validation failed", validationErr,
			fmt.Errorf("mapping: %w and no cache available", validationErr))
	}
	// A syntactically valid but sharply truncated refresh (e.g. one record
	// replacing ~40k) can pass the coverage floor above yet silently erase most
	// mappings; treat a below-half-size refresh as part of the cache-acceptance
	// invariant and keep the stale map (multiplication avoids integer-division
	// rounding for odd counts).
	if prevCount := buildIndex(prev.Records).Len(); prevCount > 0 && len(records)*2 < prevCount {
		// The noCache argument is unreachable here (prevCount > 0 guarantees the
		// stale branch); it exists only to satisfy rejectRefresh's signature.
		return rejectRefresh(prev,
			fmt.Sprintf("refresh returned %d records, less than half of previous %d", len(records), prevCount),
			nil, errors.New("mapping: refresh shrank unexpectedly and no cache available"))
	}
	l.log.Info("mapping: refreshed", "records", len(records))
	return Cache{
		FetchedAt:    time.Now(),
		Records:      records,
		ETag:         res.Validators.ETag,
		LastModified: res.Validators.LastModified,
	}, nil
}

// validateRefreshedRecords is acceptRefresh's acceptance invariant for a fresh
// 200 body: it rejects a zero-record refresh and one below the arr-identifier
// coverage floor. The tolerant per-record decoders in fribb.go deliberately
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
func validateRefreshedRecords(records []Record) error {
	if len(records) == 0 {
		return errors.New("refresh returned zero records")
	}
	minimum := max(1, (len(records)+99)/100)
	if covered := arrIdentifierCount(records); covered < minimum {
		return fmt.Errorf("arr identifier coverage %d/%d is below minimum %d", covered, len(records), minimum)
	}
	return nil
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
// are sent only when there is a usable cached record set: a validator-only
// empty cache must force a full 200 download rather than being eligible for a
// 304 that would reuse zero records.
func (l *Loader) conditionalGet(ctx context.Context, prev *Cache) (httpx.ConditionalResult, error) {
	validators := httpx.Validators{}
	if len(prev.Records) > 0 {
		validators = httpx.Validators{ETag: prev.ETag, LastModified: prev.LastModified}
	}
	return httpx.RetryWithBackoff(ctx, maxAttempts, baseDelay, "mapping",
		func(ctx context.Context) (httpx.ConditionalResult, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, http.NoBody)
			if err != nil {
				return httpx.ConditionalResult{}, err
			}
			req.Header.Set("User-Agent", appinfo.UserAgent)
			return httpx.DoConditional(l.http, req, validators, maxMapBytes)
		})
}

// applyOverrides reads the operator overrides file (if present) and overlays
// each record onto the index, keyed by AniList ID. A missing file is not an
// error; a malformed file is logged and ignored so a bad override never blocks
// a cycle.
func (l *Loader) applyOverrides(ctx context.Context, idx *Index) {
	if l.overridesPath == "" {
		return
	}
	data, err := atomicfile.ReadBounded(ctx, l.overridesPath, maxOverrideBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			l.log.Warn("mapping: overrides unreadable, ignoring", "path", l.overridesPath, "error", err)
		}
		return
	}
	overrides, err := parseOverrides(data)
	if err != nil {
		l.log.Warn("mapping: overrides malformed, ignoring", "path", l.overridesPath, "error", err)
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

// parseOverrides decodes the overrides file: a JSON array of Record objects,
// each keyed by its AniList ID. The Type is normalized to upper case so an
// operator can write "movie" or "tv".
func parseOverrides(data []byte) ([]Record, error) {
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	for i := range records {
		records[i].Type = NormalizeType(records[i].Type)
	}
	return records, nil
}

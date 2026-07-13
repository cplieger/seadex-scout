// Package mapping bridges AniList IDs (what SeaDex keys on) to the arr IDs
// Sonarr and Radarr key on (TVDB, TMDB, IMDb), using the Fribb anime-lists
// dataset plus a local overrides file the operator can pin misses in.
//
// The Fribb file is fetched with a conditional GET (ETag / If-Modified-Since)
// on a slow cadence and cached, so an unchanged multi-MB file is not
// re-downloaded. Overrides are read every load and applied ahead of Fribb, so
// an operator entry always wins over the upstream mapping.
package mapping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/seadex-scout/internal/appinfo"
)

const (
	// maxMapBytes bounds the Fribb download before decode.
	maxMapBytes = 64 << 20
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
	TmdbTV     int      `json:"tmdb_tv,omitempty"`
	SeasonTvdb int      `json:"season_tvdb,omitempty"`
	OffsetTvdb int      `json:"offset_tvdb,omitempty"`
}

// IsMovie reports whether the entry maps to a Radarr movie (Fribb type MOVIE).
// Every other type (TV, OVA, ONA, SPECIAL, ...) maps to a Sonarr series.
func (r *Record) IsMovie() bool { return r.Type == typeMovie }

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

// Cache is the persisted mapping state: the parsed Fribb records plus the HTTP
// validators and timestamp needed for the next conditional GET.
type Cache struct {
	FetchedAt    time.Time `json:"fetched_at"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Records      []Record  `json:"records,omitempty"`
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

// NewLoader returns a mapping loader. url is the Fribb JSON source,
// overridesPath is the local override file (may be absent), refresh is the
// conditional re-download cadence, and logger may be nil.
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
// returns the stale index with a non-nil error so the caller can log a degraded
// cycle while still comparing against the last good map.
func (l *Loader) Load(ctx context.Context, prev *Cache) (Cache, *Index, error) {
	next, err := l.refreshCache(ctx, prev)
	// Build from whatever records survived (fresh, refreshed, or stale prev).
	idx := buildIndex(next.Records)
	l.applyOverrides(ctx, idx)
	return next, idx, err
}

// refreshCache decides whether to reuse, re-validate, or re-download the Fribb
// map and returns the cache to persist.
func (l *Loader) refreshCache(ctx context.Context, prev *Cache) (Cache, error) {
	if len(prev.Records) > 0 && time.Since(prev.FetchedAt) < l.refresh {
		l.log.Debug("mapping: cache fresh, skipping fetch", "records", len(prev.Records), "age", time.Since(prev.FetchedAt).Round(time.Second))
		return *prev, nil
	}

	res, err := l.conditionalGet(ctx, prev)
	if err != nil {
		if len(prev.Records) > 0 {
			return *prev, fmt.Errorf("mapping: refresh failed, using stale map (%d records): %w", len(prev.Records), err)
		}
		return *prev, fmt.Errorf("mapping: initial fetch failed and no cache available: %w", err)
	}
	if res.notModified {
		if len(prev.Records) == 0 {
			return *prev, errors.New("mapping: not modified but no cache available")
		}
		l.log.Debug("mapping: not modified, reusing cache", "records", len(prev.Records))
		refreshed := *prev
		refreshed.FetchedAt = time.Now()
		return refreshed, nil
	}

	records, err := parseFribb(res.body, l.log)
	if err != nil {
		if len(prev.Records) > 0 {
			return *prev, fmt.Errorf("mapping: parse failed, using stale map (%d records): %w", len(prev.Records), err)
		}
		return *prev, fmt.Errorf("mapping: parse failed and no cache available: %w", err)
	}
	if len(records) == 0 {
		if len(prev.Records) > 0 {
			return *prev, fmt.Errorf("mapping: refresh returned zero records, using stale map (%d records)", len(prev.Records))
		}
		return *prev, errors.New("mapping: refresh returned zero records and no cache available")
	}
	l.log.Info("mapping: refreshed", "records", len(records))
	return Cache{
		FetchedAt:    time.Now(),
		Records:      records,
		ETag:         res.etag,
		LastModified: res.lastModified,
	}, nil
}

// fetchResult is one conditional-GET outcome.
type fetchResult struct {
	etag         string
	lastModified string
	body         []byte
	notModified  bool
}

// conditionalGet issues a GET with the cached ETag / Last-Modified validators,
// retrying transient failures. A 304 returns notModified; a 200 returns the
// bounded body and fresh validators.
func (l *Loader) conditionalGet(ctx context.Context, prev *Cache) (fetchResult, error) {
	return httpx.RetryWithBackoff(ctx, maxAttempts, baseDelay, "mapping",
		func(ctx context.Context) (fetchResult, error) {
			req, err := l.buildRequest(ctx, prev)
			if err != nil {
				return fetchResult{}, err
			}
			resp, err := l.http.Do(req) //nolint:bodyclose // closed in readResponse (ReadLimitedBody on 200, DrainClose otherwise)
			if err != nil {
				return fetchResult{}, err
			}
			return readResponse(resp)
		})
}

// buildRequest builds the conditional GET with the cached validators set.
func (l *Loader) buildRequest(ctx context.Context, prev *Cache) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", appinfo.UserAgent)
	// Only send conditional-GET validators when there is a usable cached record
	// set; a validator-only empty cache must force a full 200 download rather
	// than being eligible for a 304 that would reuse zero records.
	if len(prev.Records) > 0 {
		if prev.ETag != "" {
			req.Header.Set("If-None-Match", prev.ETag)
		}
		if prev.LastModified != "" {
			req.Header.Set("If-Modified-Since", prev.LastModified)
		}
	}
	return req, nil
}

// readResponse classifies a conditional-GET response, draining and closing the
// body. A 304 is notModified; a 200 returns the bounded body and validators;
// anything else is an error (transient 5xx retried by the caller).
func readResponse(resp *http.Response) (fetchResult, error) {
	switch resp.StatusCode {
	case http.StatusNotModified:
		httpx.DrainClose(resp.Body)
		return fetchResult{notModified: true}, nil
	case http.StatusOK:
		// ReadLimitedBody fails closed on an over-cap body (ResponseTooLargeError)
		// instead of silently truncating it, and always closes resp.Body itself.
		body, err := httpx.ReadLimitedBody(resp.Body, maxMapBytes)
		if err != nil {
			return fetchResult{}, err
		}
		return fetchResult{
			body:         body,
			etag:         resp.Header.Get("ETag"),
			lastModified: resp.Header.Get("Last-Modified"),
		}, nil
	default:
		httpx.DrainClose(resp.Body)
		if statusErr := httpx.CheckHTTPStatus(resp); statusErr != nil {
			return fetchResult{}, statusErr
		}
		return fetchResult{}, fmt.Errorf("mapping: unexpected status %d", resp.StatusCode)
	}
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
	for _, r := range overrides {
		if r.AniListID != 0 {
			idx.byAniList[r.AniListID] = r
		}
	}
	if len(overrides) > 0 {
		l.log.Info("mapping: applied overrides", "count", len(overrides))
	}
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
		records[i].Type = strings.ToUpper(strings.TrimSpace(records[i].Type))
	}
	return records, nil
}

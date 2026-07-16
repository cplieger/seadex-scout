// Package seadex is a read client for the SeaDex (releases.moe) PocketBase API.
//
// SeaDex curates the best available release per anime, keyed by AniList ID. The
// client pages through the entries collection with the torrents relation
// expanded, is polite to the Cloudflare-fronted community service (a
// descriptive User-Agent and a configurable inter-page delay), and bounds every
// response before decoding. It is read-only and never authenticates.
package seadex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/seadex-scout/internal/appinfo"
)

const (
	// entriesPath is the PocketBase collection endpoint for SeaDex entries.
	entriesPath = "/api/collections/entries/records"
	// perPage is the page size. The full set is a few thousand entries, so
	// 500/page keeps it to a handful of requests.
	perPage = 500
	// maxPages caps pagination so a misbehaving API cannot loop forever
	// (~6 pages expected at perPage=500).
	maxPages = 200
	// maxEntries caps total accumulated entries so a compromised or misbehaving
	// upstream cannot accumulate unbounded memory across maxPages pages
	// (~a few thousand entries expected).
	maxEntries = 200_000
	// maxPageBytes bounds one page (500 entries with expanded torrents) before
	// decode, guarding against an oversized or malicious payload.
	maxPageBytes = 48 << 20
	// maxTotalBytes caps cumulative page bytes across the whole fetch so a
	// compromised upstream serving few-but-huge items per page (under the
	// entry-count cap) cannot accumulate maxPages*maxPageBytes of memory.
	// The honest catalogue is a few tens of MB; 512 MB is generous headroom.
	maxTotalBytes = 512 << 20
	// maxAttempts / baseDelay bound the per-page retry.
	maxAttempts = 3
	baseDelay   = time.Second
)

// File is one file inside a SeaDex torrent (its name and byte length).
type File struct {
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

// Torrent is a single release SeaDex tracks for an entry.
type Torrent struct {
	ReleaseGroup string   `json:"releaseGroup"`
	Tracker      string   `json:"tracker"`
	InfoHash     string   `json:"infoHash"`
	URL          string   `json:"url"`
	Files        []File   `json:"files"`
	Tags         []string `json:"tags"`
	IsBest       bool     `json:"isBest"`
	DualAudio    bool     `json:"dualAudio"`
}

// Entry is a SeaDex entry: one anime (by AniList ID) and its tracked releases.
type Entry struct {
	Updated         time.Time
	Notes           string
	TheoreticalBest string
	Torrents        []Torrent
	AniListID       int
	Incomplete      bool
}

// HasTheoreticalBest reports whether the entry names a theoretical-best release
// that is not yet muxed (nothing concrete to grab).
func (e *Entry) HasTheoreticalBest() bool { return e.TheoreticalBest != "" }

// Client fetches entries from a SeaDex PocketBase instance.
type Client struct {
	http      *http.Client
	log       *slog.Logger
	baseURL   string
	pageDelay time.Duration
}

// NewClient returns a SeaDex client for baseURL (e.g. "https://releases.moe")
// using the given HTTP client. pageDelay is slept between pages for politeness;
// logger may be nil (slog.Default is used).
func NewClient(httpClient *http.Client, baseURL string, pageDelay time.Duration, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		http:      httpClient,
		log:       logger,
		baseURL:   baseURL,
		pageDelay: pageDelay,
	}
}

// pbList is the PocketBase list-response envelope for the entries collection.
type pbList struct {
	Items      []pbEntry `json:"items"`
	TotalItems int       `json:"totalItems"`
	TotalPages int       `json:"totalPages"`
}

// pbEntry mirrors an entries record with the torrents relation expanded.
type pbEntry struct {
	Notes           string   `json:"notes"`
	TheoreticalBest string   `json:"theoreticalBest"`
	Updated         string   `json:"updated"`
	Expand          pbExpand `json:"expand"`
	AlID            int      `json:"alID"`
	Incomplete      bool     `json:"incomplete"`
}

// pbExpand holds the expanded torrents relation (?expand=trs).
type pbExpand struct {
	Trs []Torrent `json:"trs"`
}

// toEntry converts a decoded PocketBase record into a public Entry.
func (r *pbEntry) toEntry() Entry {
	return Entry{
		Torrents:        r.Expand.Trs,
		Notes:           r.Notes,
		TheoreticalBest: r.TheoreticalBest,
		Updated:         parsePBTime(r.Updated),
		AniListID:       r.AlID,
		Incomplete:      r.Incomplete,
	}
}

// pbTimeLayouts are the PocketBase datetime formats seen on the `updated`
// field (space-separated with optional fractional seconds, or RFC3339).
var pbTimeLayouts = []string{"2006-01-02 15:04:05.000Z", "2006-01-02 15:04:05Z", time.RFC3339}

// parsePBTime parses a PocketBase timestamp, returning the zero time on failure
// (which sorts oldest, so an unparseable record just falls to the feed's tail).
func parsePBTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range pbTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// FetchEntries pages through the entire entries collection with torrents
// expanded and returns every entry. It sleeps pageDelay between pages. A page
// fetch failure aborts and returns the error; partial results are discarded so
// a caller never compares against a truncated SeaDex view. A catalogue that
// completes with ZERO entries is an error, never a success: SeaDex is never
// legitimately empty for this app's use, and accepting one would make every
// library item read as having no SeaDex coverage. A completed catalogue whose
// entry count disagrees with the API's reported totalItems is logged (WARN)
// but still returned - pagination raciness over a live collection can
// legitimately shift counts mid-fetch.
func (c *Client) FetchEntries(ctx context.Context) ([]Entry, error) {
	var all []Entry
	var totalBytes, reportedTotal, unparsedTimes int
	for page := 1; page <= maxPages; page++ {
		if page > 1 {
			if err := httpx.SleepCtx(ctx, c.pageDelay); err != nil {
				return nil, fmt.Errorf("seadex: interrupted between pages: %w", err)
			}
		}
		var done bool
		var err error
		all, done, err = c.fetchAndAppend(ctx, page, all, &totalBytes, &reportedTotal, &unparsedTimes)
		if err != nil {
			return nil, err
		}
		if done {
			return c.finishFetch(all, reportedTotal, unparsedTimes)
		}
	}
	return nil, fmt.Errorf("seadex: pagination exceeded max %d pages (upstream reported more); "+
		"refusing to compare against a truncated view", maxPages)
}

// finishFetch validates a completed catalogue before returning it: zero
// collected entries is an error (SeaDex is never legitimately empty for this
// app's use, whether the API reported zero totals or served empty pages), and
// a collected count disagreeing with the API's reported totalItems logs the
// alert-stable count-mismatch WARN but still returns the entries. Entries
// whose non-empty updated timestamp failed to parse (zeroed, sorting to the
// feed's tail) are surfaced as one aggregate WARN so an upstream format drift
// that zeroes the whole catalogue is alertable without per-record noise.
func (c *Client) finishFetch(all []Entry, reportedTotal, unparsedTimes int) ([]Entry, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("seadex: returned an empty catalogue (totalItems=%d); "+
			"SeaDex is never legitimately empty, refusing to compare against it", reportedTotal)
	}
	if len(all) != reportedTotal {
		c.log.Warn("seadex catalogue count mismatch", "got", len(all), "want", reportedTotal)
	}
	if unparsedTimes > 0 {
		c.log.Warn("seadex updated timestamps unparseable; feed newest-first ordering degraded",
			"count", unparsedTimes, "entries", len(all))
	}
	c.log.Debug("seadex entries fetched", "entries", len(all))
	return all, nil
}

// fetchAndAppend fetches one page, appends its entries, updates the running
// byte total, the API's reported item total, and the unparseable-updated
// counter, enforces the cumulative-byte and entry-count caps, and reports
// whether pagination is complete.
func (c *Client) fetchAndAppend(ctx context.Context, page int, all []Entry, totalBytes, reportedTotal, unparsedTimes *int) (out []Entry, done bool, err error) {
	list, n, err := c.fetchPage(ctx, page)
	if err != nil {
		return all, false, fmt.Errorf("seadex: fetch page %d: %w", page, err)
	}
	*totalBytes += n
	*reportedTotal = list.TotalItems
	if *totalBytes > maxTotalBytes {
		return all, false, fmt.Errorf("seadex: cumulative page bytes exceeded cap %d (upstream misbehaving); "+
			"refusing to compare against a truncated view", maxTotalBytes)
	}
	for i := range list.Items {
		e := list.Items[i].toEntry()
		if e.Updated.IsZero() && strings.TrimSpace(list.Items[i].Updated) != "" {
			*unparsedTimes++
		}
		all = append(all, e)
	}
	if len(all) > maxEntries {
		return all, false, fmt.Errorf("seadex: entry count exceeded cap %d (upstream misbehaving)", maxEntries)
	}
	done, err = pageComplete(page, len(list.Items), list.TotalPages)
	return all, done, err
}

// pageComplete reports whether pagination is done after a page, or an error
// when the pagination metadata is invalid (totalPages < 1, or a page past the
// reported total — empty or not) or the page is empty before the reported
// total (a truncated view). totalPages is an unvalidated upstream field (a
// missing value decodes to zero), so the only invalid-metadata exception is an
// empty FIRST response with zeroed metadata (a degenerate `{}` body), which
// finishFetch's empty-catalogue guard converts into an error. Every LATER page
// was only requested because an earlier page promised it existed, so an empty
// page 3 reporting totalPages=2 signals records shifted across already-read
// pages (a deletion mid-pagination) and must not complete the catalogue —
// FetchEntries' contract is to never return a possibly-truncated view.
func pageComplete(page, itemCount, totalPages int) (done bool, err error) {
	if page == 1 && itemCount == 0 && totalPages <= 0 {
		return true, nil
	}
	if totalPages < 1 || page > totalPages {
		return false, fmt.Errorf("seadex: page %d with invalid pagination metadata (totalPages %d); "+
			"refusing to compare against a truncated view", page, totalPages)
	}
	if itemCount == 0 && page < totalPages {
		return false, fmt.Errorf("seadex: page %d empty before reported total %d pages; "+
			"refusing to compare against a truncated view", page, totalPages)
	}
	return page >= totalPages, nil
}

// fetchPage fetches and decodes a single page of entries, also returning the
// raw body size so the caller can bound cumulative bytes across pages.
func (c *Client) fetchPage(ctx context.Context, page int) (pbList, int, error) {
	q := url.Values{
		"expand":  {"trs"},
		"page":    {strconv.Itoa(page)},
		"perPage": {strconv.Itoa(perPage)},
		// Sort on immutable fields: with offset pagination over a live
		// collection, sorting on `updated` lets a mid-pagination entry update
		// shift records across already-fetched pages (one entry missed, another
		// duplicated, for this cycle). created,id is stable under updates.
		"sort": {"created,id"},
	}
	reqURL := c.baseURL + entriesPath + "?" + q.Encode()

	body, err := httpx.Retry(ctx, c.http, reqURL,
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithMaxBodyBytes(maxPageBytes),
		httpx.WithHeaders(setHeaders),
		httpx.WithLogger(c.log),
	)
	if err != nil {
		return pbList{}, 0, err
	}

	var list pbList
	if err := json.Unmarshal(body, &list); err != nil {
		return pbList{}, 0, fmt.Errorf("decode page: %w", err)
	}
	return list, len(body), nil
}

// setHeaders sets the descriptive User-Agent and JSON Accept header on each
// SeaDex request.
func setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/json")
}

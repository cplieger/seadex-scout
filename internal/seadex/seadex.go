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
	"time"

	"github.com/cplieger/httpx/v2"
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
	// maxPageBytes bounds one page (500 entries with expanded torrents) before
	// decode, guarding against an oversized or malicious payload.
	maxPageBytes = 48 << 20
	// userAgent identifies seadex-scout to the community service.
	userAgent = "seadex-scout (+https://github.com/cplieger/seadex-scout)"
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
	Notes           string
	Comparison      string
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
	TotalPages int       `json:"totalPages"`
}

// pbEntry mirrors an entries record with the torrents relation expanded.
type pbEntry struct {
	Notes           string   `json:"notes"`
	Comparison      string   `json:"comparison"`
	TheoreticalBest string   `json:"theoreticalBest"`
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
		Comparison:      r.Comparison,
		TheoreticalBest: r.TheoreticalBest,
		AniListID:       r.AlID,
		Incomplete:      r.Incomplete,
	}
}

// FetchEntries pages through the entire entries collection with torrents
// expanded and returns every entry. It sleeps pageDelay between pages. A page
// fetch failure aborts and returns the error; partial results are discarded so
// a caller never compares against a truncated SeaDex view.
func (c *Client) FetchEntries(ctx context.Context) ([]Entry, error) {
	var all []Entry
	for page := 1; page <= maxPages; page++ {
		list, err := c.fetchPage(ctx, page)
		if err != nil {
			return nil, fmt.Errorf("seadex: fetch page %d: %w", page, err)
		}
		for i := range list.Items {
			all = append(all, list.Items[i].toEntry())
		}
		if page >= list.TotalPages || len(list.Items) == 0 {
			break
		}
		if err := httpx.SleepCtx(ctx, c.pageDelay); err != nil {
			return nil, fmt.Errorf("seadex: interrupted between pages: %w", err)
		}
	}
	c.log.Debug("seadex entries fetched", "entries", len(all))
	return all, nil
}

// fetchPage fetches and decodes a single page of entries.
func (c *Client) fetchPage(ctx context.Context, page int) (pbList, error) {
	q := url.Values{
		"expand":  {"trs"},
		"page":    {strconv.Itoa(page)},
		"perPage": {strconv.Itoa(perPage)},
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
		return pbList{}, err
	}

	var list pbList
	if err := json.Unmarshal(body, &list); err != nil {
		return pbList{}, fmt.Errorf("decode page: %w", err)
	}
	return list, nil
}

// setHeaders sets the descriptive User-Agent and JSON Accept header on each
// SeaDex request.
func setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
}

// Package seadex is a read client for the SeaDex (releases.moe) PocketBase API.
//
// SeaDex curates the best available release per anime, keyed by AniList ID. The
// client pages through the entries collection with the torrents relation
// expanded, is polite to the Cloudflare-fronted community service (a
// descriptive User-Agent and a configurable inter-page delay), and bounds every
// response before decoding. It is read-only and never authenticates.
package seadex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/httpx/v3"
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
	// The honest catalogue is a few tens of MB (3x+ headroom at 128 MB), and
	// retained decoded entries grow roughly with cumulative body bytes, so
	// the cap must sit below the 256 MiB deployment container limit for the
	// guard to fire (clean degradation) before the kernel OOM-kills the
	// process.
	maxTotalBytes = 128 << 20
	// maxAttempts / baseDelay bound the per-page retry.
	maxAttempts = 3
	baseDelay   = time.Second
)

// Cardinality caps on one decoded page, enforced by decodePage DURING the
// token-level decode. json.Unmarshal materializes the whole decoded value
// before any caller-side count check can run, so compact serialized elements
// (a page of minimal `{}` objects) could otherwise amplify a bounded body into
// decoded structs and slice backing arrays far beyond maxPageBytes. The values
// are generous headroom over the honest catalogue (a handful of torrents per
// entry, packs of ~1200 files, a few short tags), not tuning knobs; a page
// crossing one is upstream misbehavior and aborts the fetch.
const (
	// maxTorrentsPerEntry bounds one entry's expanded trs relation (honest
	// data: tens at most, one torrent per episode on unpacked seasons).
	maxTorrentsPerEntry = 512
	// maxFilesPerTorrent bounds one torrent's file list (honest data: a
	// full-series pack tops out around ~1200 files).
	maxFilesPerTorrent = 8192
	// maxTagsPerTorrent bounds one torrent's tag list (honest data: a few
	// short labels like "best" / "dual").
	maxTagsPerTorrent = 64
	// maxPageElements bounds the TOTAL decoded array elements (items +
	// torrents + files + tags) of one page. The per-parent caps alone compose
	// multiplicatively (perPage x maxTorrentsPerEntry x maxFilesPerTorrent),
	// so a body of minimal elements could still decode into hundreds of MB;
	// this cap bounds the aggregate allocation (honest pages run ~tens of
	// thousands of elements).
	maxPageElements = 500_000
	// maxTotalElements bounds the cumulative decoded array elements across
	// the WHOLE fetch. fetchAndAppend retains every decoded entry until the
	// fetch completes, so a per-page element cap alone still lets dozens of
	// compact pages (each individually under maxPageElements, together under
	// maxTotalBytes) amplify into decoded structs and slice backing arrays
	// that OOM-kill the 256 MiB deployment container. Like the byte budget,
	// the remaining allowance caps each page's decode, so the guard fires
	// (clean degradation) before allocation scales with the hostile input.
	// Sized jointly with maxTotalBytes: worst-case element struct overhead
	// (~112 B/torrent x this cap) must fit under the 256 MiB container
	// TOGETHER with maxTotalBytes of decoded string content.
	maxTotalElements = 500_000
)

// errCumulativeBytes reports the cumulative-byte budget (maxTotalBytes) being
// exceeded. It is raised at the wire layer - fetchPage caps each download at
// the REMAINING budget, so an over-budget page is rejected before decode -
// which preserves the pre-budget error contract for the same condition.
var errCumulativeBytes = fmt.Errorf("seadex: cumulative page bytes exceeded cap %d (upstream misbehaving); "+
	"refusing to compare against a truncated view", maxTotalBytes)

// errCumulativeElements reports the fetch-wide decoded-element budget
// (maxTotalElements) being exceeded. Like errCumulativeBytes it is enforced
// at the decode layer - fetchPage bounds each page's decode at the REMAINING
// element budget, so an over-budget page is rejected mid-decode, before the
// excess elements are materialized or retained.
var errCumulativeElements = fmt.Errorf("seadex: cumulative decoded elements exceeded cap %d (upstream misbehaving); "+
	"refusing to compare against a truncated view", maxTotalElements)

// errElementCap marks the page decoder's element budget being exceeded, so
// fetchPage can classify which budget fired: the full per-page bound is a
// per-page violation, while a budget-reduced limit is the fetch-wide
// cumulative cap.
var errElementCap = errors.New("elements exceeded cap")

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

// RedactedInfoHash is the placeholder releases.moe publishes in place of a
// private-tracker (AnimeBytes) torrent's info hash.
const RedactedInfoHash = "<redacted>"

// InfoHashRedacted reports whether h is the RedactedInfoHash placeholder.
func InfoHashRedacted(h string) bool {
	return strings.EqualFold(strings.TrimSpace(h), RedactedInfoHash)
}

// ValidInfoHash returns h lowercased when it is a 40-char SHA-1 hex info hash,
// else "" (covers RedactedInfoHash and any other junk value).
func ValidInfoHash(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) != 40 {
		return ""
	}
	for i := range len(h) {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return h
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
// that is not yet muxed (nothing concrete to grab). Like the package's other
// predicates over untrusted PocketBase text, surrounding whitespace is not a
// name: a whitespace-only value reports false.
func (e *Entry) HasTheoreticalBest() bool { return strings.TrimSpace(e.TheoreticalBest) != "" }

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

// ---- PocketBase wire model and paging pipeline ----

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

// fetchTotals accumulates the cross-page counters of one FetchEntries run.
// reportedTotal and reportedPages retain the HIGHEST value any page promised
// (never overwritten downward): a later page whose metadata regresses — an
// empty page omitting totalItems decodes it as zero, or a lowered totalPages —
// must not erase an earlier page's promise of more records, or pageComplete
// would accept a truncated view.
type fetchTotals struct {
	bytes         int
	elements      int
	reportedTotal int
	reportedPages int
	unparsedTimes int
	unusableURLs  int
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
// legitimately shift counts mid-fetch. That leniency requires the final page
// to have carried entries: an EMPTY page while the collected count is still
// below the reported totalItems aborts with an error (pageComplete), since
// the API itself says entries remain and completing would falsely resolve
// findings against a truncated view.
func (c *Client) FetchEntries(ctx context.Context) ([]Entry, error) {
	var all []Entry
	var tot fetchTotals
	for page := 1; page <= maxPages; page++ {
		if page > 1 {
			if err := httpx.SleepCtx(ctx, c.pageDelay); err != nil {
				return nil, fmt.Errorf("seadex: interrupted between pages: %w", err)
			}
		}
		var done bool
		var err error
		all, done, err = c.fetchAndAppend(ctx, page, all, &tot)
		if err != nil {
			return nil, err
		}
		if done {
			return c.finishFetch(all, tot)
		}
	}
	return nil, fmt.Errorf("seadex: pagination exceeded max %d pages after %d entries fetched (upstream reported more); "+
		"refusing to compare against a truncated view", maxPages, len(all))
}

// finishFetch validates a completed catalogue before returning it: zero
// collected entries is an error (SeaDex is never legitimately empty for this
// app's use, whether the API reported zero totals or served empty pages), and
// a collected count disagreeing with the API's reported totalItems logs the
// alert-stable count-mismatch WARN but still returns the entries. Entries
// whose non-empty updated timestamp failed to parse (zeroed, sorting to the
// feed's tail) are surfaced as one aggregate WARN so an upstream format drift
// that zeroes the whole catalogue is alertable without per-record noise.
// Torrents whose URL is unusable (omitted/empty, or a non-empty value
// dropped to "" by UsableURL: a foreign host under a trusted label, an
// unknown tracker, a malformed URL) are likewise surfaced as one aggregate
// WARN — filter.Obtainable treats both cases as unobtainable — so a schema
// drift that strips every release link is alertable instead of silent.
func (c *Client) finishFetch(all []Entry, tot fetchTotals) ([]Entry, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("seadex: returned an empty catalogue (totalItems=%d); "+
			"SeaDex is never legitimately empty, refusing to compare against it", tot.reportedTotal)
	}
	if len(all) != tot.reportedTotal {
		c.log.Warn("seadex catalogue count mismatch", "got", len(all), "want", tot.reportedTotal)
	}
	if tot.unparsedTimes > 0 {
		c.log.Warn("seadex updated timestamps unparseable; feed newest-first ordering degraded",
			"count", tot.unparsedTimes, "entries", len(all))
	}
	if tot.unusableURLs > 0 {
		c.log.Warn("seadex torrent URLs unusable; affected findings and feed items carry no release link",
			"count", tot.unusableURLs, "entries", len(all))
	}
	c.log.Debug("seadex entries fetched", "entries", len(all))
	return all, nil
}

// fetchAndAppend fetches one page, appends its entries, updates the running
// totals (cumulative bytes and decoded elements, the API's reported item
// total, and the unparseable-updated and unusable-URL counters), enforces the
// cumulative-byte, cumulative-element, and entry-count caps, and reports
// whether pagination is complete. All caps run BEFORE allocation scales with
// the hostile input: the cumulative-byte budget caps the wire read itself
// (fetchPage downloads at most the remaining budget, so tot.bytes can never
// exceed maxTotalBytes), the cumulative-element budget caps the decode
// (fetchPage decodes at most the remaining element allowance, so tot.elements
// can never exceed maxTotalElements), and the entry-count cap rejects the
// page before any of its items are converted or appended.
func (c *Client) fetchAndAppend(ctx context.Context, page int, all []Entry, tot *fetchTotals) (out []Entry, done bool, err error) {
	remaining := int64(maxTotalBytes - tot.bytes)
	if remaining <= 0 {
		return all, false, fmt.Errorf("%w (page %d, %d entries fetched)", errCumulativeBytes, page, len(all))
	}
	remainingElems := maxTotalElements - tot.elements
	if remainingElems <= 0 {
		return all, false, fmt.Errorf("%w (page %d, %d entries fetched)", errCumulativeElements, page, len(all))
	}
	list, n, elems, err := c.fetchPage(ctx, page, min(int64(maxPageBytes), remaining), min(maxPageElements, remainingElems))
	if err != nil {
		if errors.Is(err, errCumulativeBytes) || errors.Is(err, errCumulativeElements) {
			return all, false, fmt.Errorf("%w (page %d, %d entries fetched)", err, page, len(all))
		}
		return all, false, fmt.Errorf("seadex: fetch page %d: %w", page, err)
	}
	tot.bytes += n
	tot.elements += elems
	tot.reportedTotal = max(tot.reportedTotal, list.TotalItems)
	tot.reportedPages = max(tot.reportedPages, list.TotalPages)
	if len(list.Items) > maxEntries-len(all) {
		return all, false, fmt.Errorf("seadex: entry count exceeded cap %d on page %d (%d already fetched, %d received; upstream misbehaving)",
			maxEntries, page, len(all), len(list.Items))
	}
	all = appendPageEntries(all, list.Items, tot)
	done, err = pageComplete(page, len(list.Items), list.TotalPages, len(all), tot.reportedTotal)
	if err != nil {
		return all, false, err
	}
	// pageComplete judges the CURRENT page's totalPages (so invalid current
	// metadata still errors); the retained highest promise prevents a
	// lower-but-currently-valid terminal value from ending the walk early
	// after an earlier page promised more pages.
	if done && page < tot.reportedPages {
		done = false
	}
	return all, done, nil
}

// appendPageEntries converts one page's decoded records into public entries,
// charging the unparseable-updated and unusable-URL counters as it appends.
func appendPageEntries(all []Entry, items []pbEntry, tot *fetchTotals) []Entry {
	for i := range items {
		entry := items[i].toEntry()
		if entry.Updated.IsZero() && strings.TrimSpace(items[i].Updated) != "" {
			tot.unparsedTimes++
		}
		for j := range entry.Torrents {
			if entry.Torrents[j].UsableURL() == "" {
				tot.unusableURLs++
			}
		}
		all = append(all, entry)
	}
	return all
}

// pageComplete reports whether pagination is done after a page, or an error
// when the pagination metadata is invalid (totalPages < 1, or a page past the
// reported total — empty or not), the page is empty before the reported total
// (a truncated view), or the page is empty while fetched (the entries
// collected so far) is still below the reported totalItems — the API itself
// says entries remain, so completing would hand downstream a truncated view
// that falsely resolves findings; failing instead degrades the cycle, the
// fail-safe direction that preserves existing findings. A NON-empty terminal
// page with a count mismatch stays finishFetch's WARN (offset pagination over
// a live collection can legitimately shift counts mid-fetch). totalPages is
// an unvalidated upstream field (a missing value decodes to zero), so the
// only invalid-metadata exception is an empty FIRST response with zeroed
// metadata (a degenerate `{}` body), which finishFetch's empty-catalogue
// guard converts into an error. Every LATER page was only requested because
// an earlier page promised it existed, so an empty page 3 reporting
// totalPages=2 signals records shifted across already-read pages (a deletion
// mid-pagination) and must not complete the catalogue — FetchEntries'
// contract is to never return a possibly-truncated view.
func pageComplete(page, itemCount, totalPages, fetched, reportedTotal int) (done bool, err error) {
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
	if itemCount == 0 && fetched < reportedTotal {
		return false, fmt.Errorf("seadex: page %d empty with %d of %d reported entries fetched; "+
			"refusing to compare against a truncated view", page, fetched, reportedTotal)
	}
	return page >= totalPages, nil
}

// fetchPage fetches and decodes a single page of entries, also returning the
// raw body size and the decoded array-element count so the caller can bound
// cumulative bytes and decoded elements across pages. wireLimit is the
// download cap for THIS page: the per-page bound
// (maxPageBytes) already reduced by the caller to the remaining cumulative
// budget, so an over-budget page is rejected at the wire layer, before any
// bytes beyond the budget are held or decoded. A too-large response that
// tripped a budget-reduced limit (below maxPageBytes) is reported as the
// cumulative-cap error; one that tripped the full per-page bound is a
// per-page violation and surfaces as the fetch error itself. elemLimit is
// the decode cap for THIS page, classified the same way: the per-page
// element bound (maxPageElements) already reduced by the caller to the
// remaining fetch-wide element budget, so tripping a reduced limit is the
// cumulative-element cap while tripping the full bound stays a per-page
// violation.
func (c *Client) fetchPage(ctx context.Context, page int, wireLimit int64, elemLimit int) (list pbList, bodyBytes, elems int, err error) {
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

	body, err := httpx.GetBytes(ctx, c.http, reqURL,
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithMaxBodyBytes(wireLimit),
		httpx.WithHeaders(setHeaders),
		httpx.WithLogger(c.log),
	)
	if err != nil {
		if tooLarge, ok := errors.AsType[*httpx.ResponseTooLargeError](err); ok && tooLarge.Limit < maxPageBytes {
			return pbList{}, 0, 0, errCumulativeBytes
		}
		return pbList{}, 0, 0, err
	}

	list, elems, err = decodePage(body, elemLimit)
	if err != nil {
		if errors.Is(err, errElementCap) && elemLimit < maxPageElements {
			return pbList{}, 0, 0, errCumulativeElements
		}
		return pbList{}, 0, 0, fmt.Errorf("decode page: %w", err)
	}
	return list, len(body), elems, nil
}

// ---- Bounded token-level page decoder ----

// pageDecoder is a schema-aware bounded decoder for one pbList page. Unlike
// json.Unmarshal - which materializes the entire decoded value before any
// caller-side count check can run, letting compact serialized elements amplify
// a wire-capped body into decoded structs and slice backing arrays far beyond
// maxPageBytes - it walks the token stream and enforces every cardinality cap
// (perPage items, maxTorrentsPerEntry, maxFilesPerTorrent, maxTagsPerTorrent,
// and the aggregate maxPageElements) BEFORE appending each element, so
// allocation never scales with hostile array cardinality. Scalar values decode
// via json.Decoder.Decode for stdlib-identical type handling; known keys
// match case-insensitively (json.Unmarshal's fallback rule for field names);
// unknown fields are token-skipped without materializing; a JSON null anywhere
// a container is expected yields the zero value, matching json.Unmarshal.
type pageDecoder struct {
	dec      *json.Decoder
	elements int
	limit    int
}

// decodePage decodes one page body under the pageDecoder bounds, rejecting
// trailing data after the top-level value (matching json.Unmarshal
// strictness). elemLimit is the aggregate element budget for this page (the
// per-page bound, possibly reduced to the fetch-wide remaining allowance by
// fetchAndAppend); the decoded element count is returned so the caller can
// charge the fetch-wide budget.
func decodePage(body []byte, elemLimit int) (pbList, int, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	// UseNumber keeps Token from converting unknown-field numbers to float64
	// (which rejects syntactically valid values like 1e1000 that
	// json.Unmarshal's field skipping accepts); Decode into the known
	// int/string/bool fields is unaffected.
	dec.UseNumber()
	d := &pageDecoder{dec: dec, limit: elemLimit}
	list, err := d.decodeList()
	if err != nil {
		return pbList{}, 0, err
	}
	if _, err := d.dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return pbList{}, 0, fmt.Errorf("trailing data after page object: %w", err)
		}
		return pbList{}, 0, errors.New("trailing data after page object")
	}
	return list, d.elements, nil
}

// count charges one decoded array element against the page's aggregate
// element budget.
func (d *pageDecoder) count() error {
	d.elements++
	if d.elements > d.limit {
		return fmt.Errorf("page %w %d (upstream misbehaving)", errElementCap, d.limit)
	}
	return nil
}

// open consumes the opening delimiter of a container. It reports ok=false
// (without error) for a JSON null, matching json.Unmarshal's null-into-value
// no-op, and errors on any other token.
func (d *pageDecoder) open(delim json.Delim) (ok bool, err error) {
	t, err := d.dec.Token()
	if err != nil {
		return false, err
	}
	if t == nil {
		return false, nil
	}
	if got, isDelim := t.(json.Delim); !isDelim || got != delim {
		return false, fmt.Errorf("expected %q, got %v", delim, t)
	}
	return true, nil
}

// close consumes a container's closing delimiter (the token json.Decoder
// guarantees once More reports false).
func (d *pageDecoder) close() error {
	_, err := d.dec.Token()
	return err
}

// key reads the next object key.
func (d *pageDecoder) key() (string, error) {
	t, err := d.dec.Token()
	if err != nil {
		return "", err
	}
	s, isString := t.(string)
	if !isString {
		return "", fmt.Errorf("expected object key, got %v", t)
	}
	return s, nil
}

// skip consumes and discards one whole value (scalar or container) without
// materializing it.
func (d *pageDecoder) skip() error {
	depth := 0
	for {
		t, err := d.dec.Token()
		if err != nil {
			return err
		}
		if delim, isDelim := t.(json.Delim); isDelim {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// decodeList decodes the pbList envelope.
func (d *pageDecoder) decodeList() (pbList, error) {
	var list pbList
	ok, err := d.open('{')
	if err != nil || !ok {
		return list, err
	}
	for d.dec.More() {
		k, err := d.key()
		if err != nil {
			return list, err
		}
		switch {
		case strings.EqualFold(k, "items"):
			list.Items, err = d.decodeItems(list.Items)
		case strings.EqualFold(k, "totalItems"):
			err = d.dec.Decode(&list.TotalItems)
		case strings.EqualFold(k, "totalPages"):
			err = d.dec.Decode(&list.TotalPages)
		default:
			err = d.skip()
		}
		if err != nil {
			return list, err
		}
	}
	return list, d.close()
}

// decodeBoundedArray decodes one JSON array with the package's shared
// bounded-decode lifecycle: per-parent cap check BEFORE the element is
// counted, count() budget charge BEFORE growForIndex, decode INTO the
// regrown element (json.Unmarshal's duplicate-key merge), and truncateArray
// finalization (SetLen/replace parity). prior is the already-decoded value
// of a previous occurrence of the key; what names the array in the cap
// error message.
func decodeBoundedArray[T any](d *pageDecoder, prior []T, maxElems int, what string, decodeElem func(*T) error) ([]T, error) {
	ok, err := d.open('[')
	if err != nil || !ok {
		return nil, err
	}
	s := prior
	n := 0
	for d.dec.More() {
		if n >= maxElems {
			return nil, fmt.Errorf("%s exceeded cap %d (upstream misbehaving)", what, maxElems)
		}
		if err := d.count(); err != nil {
			return nil, err
		}
		s = growForIndex(s, n)
		if err := decodeElem(&s[n]); err != nil {
			return nil, err
		}
		n++
	}
	return truncateArray(s, n), d.close()
}

// decodeItems decodes the items array, capped at perPage: the request asks
// for perPage records, so a page stuffing more is upstream misbehavior and is
// rejected before the excess is decoded. prior is the already-decoded value
// of a previous occurrence of the key: elements decode INTO the existing
// slice (regrown via growForIndex, so a within-capacity regrow re-exposes the
// retained backing element like stdlib) and the result is finalized by
// truncateArray, matching json.Unmarshal's duplicate-key slice semantics
// (element-wise field merge, SetLen truncation, backing replaced on an empty
// array).
func (d *pageDecoder) decodeItems(prior []pbEntry) ([]pbEntry, error) {
	return decodeBoundedArray(d, prior, perPage, "page items", d.decodeEntry)
}

// decodeEntry decodes one entries record field-wise into e, matching
// json.Unmarshal's duplicate-key semantics: a JSON null element is a no-op
// that preserves the existing value, and an object only overwrites the
// fields it actually carries. Field-specific decode behavior lives in
// decodeEntryField; this function owns only the open/key/dispatch/close
// token-stream lifecycle.
func (d *pageDecoder) decodeEntry(e *pbEntry) error {
	ok, err := d.open('{')
	if err != nil || !ok {
		return err
	}
	for d.dec.More() {
		k, err := d.key()
		if err != nil {
			return err
		}
		if err := d.decodeEntryField(e, k); err != nil {
			return err
		}
	}
	return d.close()
}

// decodeEntryField decodes one entries-record field (or skips an unknown
// key), preserving decodeEntry's duplicate-key semantics for each field.
func (d *pageDecoder) decodeEntryField(e *pbEntry, key string) error {
	switch {
	case strings.EqualFold(key, "notes"):
		return d.dec.Decode(&e.Notes)
	case strings.EqualFold(key, "theoreticalBest"):
		return d.dec.Decode(&e.TheoreticalBest)
	case strings.EqualFold(key, "updated"):
		return d.dec.Decode(&e.Updated)
	case strings.EqualFold(key, "alID"):
		return d.dec.Decode(&e.AlID)
	case strings.EqualFold(key, "incomplete"):
		return d.dec.Decode(&e.Incomplete)
	case strings.EqualFold(key, "expand"):
		// json.Unmarshal merges duplicate object keys field-wise and
		// treats null into a non-pointer struct as a no-op; decode into
		// the existing value so neither a duplicate "expand":null nor a
		// duplicate/partial "expand":{} can wipe an already-decoded trs.
		return d.decodeExpand(&e.Expand)
	default:
		return d.skip()
	}
}

// decodeExpand decodes the expand relation envelope field-wise into ex,
// matching json.Unmarshal's struct semantics for duplicate keys: a JSON null
// is a no-op (any previously decoded value stays), and an object only
// overwrites the fields it actually carries, so a repeated "expand" that
// omits "trs" leaves already-decoded torrents in place.
func (d *pageDecoder) decodeExpand(ex *pbExpand) error {
	ok, err := d.open('{')
	if err != nil || !ok {
		return err
	}
	for d.dec.More() {
		k, err := d.key()
		if err != nil {
			return err
		}
		if strings.EqualFold(k, "trs") {
			ex.Trs, err = d.decodeTorrents(ex.Trs)
		} else {
			err = d.skip()
		}
		if err != nil {
			return err
		}
	}
	return d.close()
}

// decodeTorrents decodes one entry's expanded trs relation, capped at
// maxTorrentsPerEntry. prior is the already-decoded value of a previous
// occurrence of the key: elements decode INTO the existing slice (regrown
// via growForIndex) and the result is finalized by truncateArray, matching
// json.Unmarshal's duplicate-key slice semantics.
func (d *pageDecoder) decodeTorrents(prior []Torrent) ([]Torrent, error) {
	return decodeBoundedArray(d, prior, maxTorrentsPerEntry, "torrents per entry", d.decodeTorrent)
}

// decodeTorrent decodes one torrent record field-wise into t, matching
// json.Unmarshal's duplicate-key semantics: a JSON null element is a no-op
// that preserves the existing value, and an object only overwrites the
// fields it actually carries. Field-specific decode behavior lives in
// decodeTorrentField; this function owns only the open/key/dispatch/close
// token-stream lifecycle.
func (d *pageDecoder) decodeTorrent(t *Torrent) error {
	ok, err := d.open('{')
	if err != nil || !ok {
		return err
	}
	for d.dec.More() {
		k, err := d.key()
		if err != nil {
			return err
		}
		if err := d.decodeTorrentField(t, k); err != nil {
			return err
		}
	}
	return d.close()
}

// decodeTorrentField decodes one torrent-record field (or skips an unknown
// key), preserving decodeTorrent's duplicate-key semantics for each field:
// the nested-array fields decode INTO the existing slice (element-wise merge
// plus truncation, matching json.Unmarshal's duplicate-key slice semantics).
func (d *pageDecoder) decodeTorrentField(t *Torrent, key string) error {
	switch {
	case strings.EqualFold(key, "releaseGroup"):
		return d.dec.Decode(&t.ReleaseGroup)
	case strings.EqualFold(key, "tracker"):
		return d.dec.Decode(&t.Tracker)
	case strings.EqualFold(key, "infoHash"):
		return d.dec.Decode(&t.InfoHash)
	case strings.EqualFold(key, "url"):
		return d.dec.Decode(&t.URL)
	case strings.EqualFold(key, "isBest"):
		return d.dec.Decode(&t.IsBest)
	case strings.EqualFold(key, "dualAudio"):
		return d.dec.Decode(&t.DualAudio)
	case strings.EqualFold(key, "files"):
		var err error
		t.Files, err = d.decodeFiles(t.Files)
		return err
	case strings.EqualFold(key, "tags"):
		var err error
		t.Tags, err = d.decodeTags(t.Tags)
		return err
	default:
		return d.skip()
	}
}

// decodeFiles decodes one torrent's file list, capped at maxFilesPerTorrent.
// A File is flat (two scalar fields), so per-element json.Decoder.Decode
// cannot amplify beyond the already-capped raw bytes. prior is the
// already-decoded value of a previous occurrence of the key: Decode into an
// existing element (regrown via growForIndex) gives json.Unmarshal's
// duplicate-key merge and null-no-op semantics, and truncateArray finalizes
// the result to the new array's length.
func (d *pageDecoder) decodeFiles(prior []File) ([]File, error) {
	return decodeBoundedArray(d, prior, maxFilesPerTorrent, "files per torrent", func(f *File) error { return d.dec.Decode(f) })
}

// decodeTags decodes one torrent's tag list, capped at maxTagsPerTorrent.
// prior is the already-decoded value of a previous occurrence of the key
// (see decodeFiles).
func (d *pageDecoder) decodeTags(prior []string) ([]string, error) {
	return decodeBoundedArray(d, prior, maxTagsPerTorrent, "tags per torrent", func(s *string) error { return d.dec.Decode(s) })
}

// growForIndex ensures the slice covers index n, matching json.Unmarshal's
// slice-regrow semantics for duplicate keys: within retained capacity the
// existing backing element is re-exposed (stdlib SetLen), beyond capacity a
// zero element is appended (stdlib Grow reallocates; the new tail is zero).
func growForIndex[T any](s []T, n int) []T {
	if n < len(s) {
		return s
	}
	if n < cap(s) {
		return s[:n+1]
	}
	var zero T
	return append(s, zero)
}

// truncateArray finalizes a decoded array at n elements, matching
// json.Unmarshal's end-of-array semantics: an empty array REPLACES the
// slice (stdlib MakeSlice(0,0) - no retained backing a later duplicate
// occurrence could re-expose), a non-empty one truncates in place
// (stdlib SetLen). Returning nil for the empty case stays inside the
// documented nil-vs-empty divergence no consumer observes.
func truncateArray[T any](s []T, n int) []T {
	if n == 0 {
		return nil
	}
	return s[:n]
}

// setHeaders sets the descriptive User-Agent and JSON Accept header on each
// SeaDex request.
func setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/json")
}

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
	maxPageElements = 1_000_000
	// maxTotalElements bounds the cumulative decoded array elements across
	// the WHOLE fetch. fetchAndAppend retains every decoded entry until the
	// fetch completes, so a per-page element cap alone still lets dozens of
	// compact pages (each individually under maxPageElements, together under
	// maxTotalBytes) amplify into decoded structs and slice backing arrays
	// that OOM-kill the 256 MiB deployment container. Like the byte budget,
	// the remaining allowance caps each page's decode, so the guard fires
	// (clean degradation) before allocation scales with the hostile input.
	maxTotalElements = 1_000_000
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

// fetchTotals accumulates the cross-page counters of one FetchEntries run.
type fetchTotals struct {
	bytes         int
	elements      int
	reportedTotal int
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
// Torrents whose non-empty URL is unusable (dropped to "" by UsableURL: a
// foreign host under a trusted label, an unknown tracker, a malformed URL)
// are likewise surfaced as one aggregate WARN, so a tracker host migration
// that strips every release link is alertable instead of silent.
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
		return all, false, errCumulativeBytes
	}
	remainingElems := maxTotalElements - tot.elements
	if remainingElems <= 0 {
		return all, false, errCumulativeElements
	}
	list, n, elems, err := c.fetchPage(ctx, page, min(int64(maxPageBytes), remaining), min(maxPageElements, remainingElems))
	if err != nil {
		if errors.Is(err, errCumulativeBytes) || errors.Is(err, errCumulativeElements) {
			return all, false, err
		}
		return all, false, fmt.Errorf("seadex: fetch page %d: %w", page, err)
	}
	tot.bytes += n
	tot.elements += elems
	tot.reportedTotal = list.TotalItems
	if len(list.Items) > maxEntries-len(all) {
		return all, false, fmt.Errorf("seadex: entry count exceeded cap %d (upstream misbehaving)", maxEntries)
	}
	all = appendPageEntries(all, list.Items, tot)
	done, err = pageComplete(page, len(list.Items), list.TotalPages, len(all), tot.reportedTotal)
	return all, done, err
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
			if strings.TrimSpace(entry.Torrents[j].URL) != "" && entry.Torrents[j].UsableURL() == "" {
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
	d := &pageDecoder{dec: json.NewDecoder(bytes.NewReader(body)), limit: elemLimit}
	list, err := d.decodeList()
	if err != nil {
		return pbList{}, 0, err
	}
	if _, err := d.dec.Token(); !errors.Is(err, io.EOF) {
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
			list.Items, err = d.decodeItems()
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

// decodeItems decodes the items array, capped at perPage: the request asks
// for perPage records, so a page stuffing more is upstream misbehavior and is
// rejected before the excess is decoded.
func (d *pageDecoder) decodeItems() ([]pbEntry, error) {
	ok, err := d.open('[')
	if err != nil || !ok {
		return nil, err
	}
	var items []pbEntry
	for d.dec.More() {
		if len(items) >= perPage {
			return nil, fmt.Errorf("page items exceeded cap %d (upstream misbehaving)", perPage)
		}
		if err := d.count(); err != nil {
			return nil, err
		}
		e, err := d.decodeEntry()
		if err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, d.close()
}

// decodeEntry decodes one entries record.
func (d *pageDecoder) decodeEntry() (pbEntry, error) {
	var e pbEntry
	ok, err := d.open('{')
	if err != nil || !ok {
		return e, err
	}
	for d.dec.More() {
		k, err := d.key()
		if err != nil {
			return e, err
		}
		switch {
		case strings.EqualFold(k, "notes"):
			err = d.dec.Decode(&e.Notes)
		case strings.EqualFold(k, "theoreticalBest"):
			err = d.dec.Decode(&e.TheoreticalBest)
		case strings.EqualFold(k, "updated"):
			err = d.dec.Decode(&e.Updated)
		case strings.EqualFold(k, "alID"):
			err = d.dec.Decode(&e.AlID)
		case strings.EqualFold(k, "incomplete"):
			err = d.dec.Decode(&e.Incomplete)
		case strings.EqualFold(k, "expand"):
			// json.Unmarshal merges duplicate object keys field-wise and
			// treats null into a non-pointer struct as a no-op; decode into
			// the existing value so neither a duplicate "expand":null nor a
			// duplicate/partial "expand":{} can wipe an already-decoded trs.
			err = d.decodeExpand(&e.Expand)
		default:
			err = d.skip()
		}
		if err != nil {
			return e, err
		}
	}
	return e, d.close()
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
			ex.Trs, err = d.decodeTorrents()
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
// maxTorrentsPerEntry.
func (d *pageDecoder) decodeTorrents() ([]Torrent, error) {
	ok, err := d.open('[')
	if err != nil || !ok {
		return nil, err
	}
	var trs []Torrent
	for d.dec.More() {
		if len(trs) >= maxTorrentsPerEntry {
			return nil, fmt.Errorf("torrents per entry exceeded cap %d (upstream misbehaving)", maxTorrentsPerEntry)
		}
		if err := d.count(); err != nil {
			return nil, err
		}
		t, err := d.decodeTorrent()
		if err != nil {
			return nil, err
		}
		trs = append(trs, t)
	}
	return trs, d.close()
}

// decodeTorrent decodes one torrent record.
func (d *pageDecoder) decodeTorrent() (Torrent, error) {
	var t Torrent
	ok, err := d.open('{')
	if err != nil || !ok {
		return t, err
	}
	for d.dec.More() {
		k, err := d.key()
		if err != nil {
			return t, err
		}
		switch {
		case strings.EqualFold(k, "releaseGroup"):
			err = d.dec.Decode(&t.ReleaseGroup)
		case strings.EqualFold(k, "tracker"):
			err = d.dec.Decode(&t.Tracker)
		case strings.EqualFold(k, "infoHash"):
			err = d.dec.Decode(&t.InfoHash)
		case strings.EqualFold(k, "url"):
			err = d.dec.Decode(&t.URL)
		case strings.EqualFold(k, "isBest"):
			err = d.dec.Decode(&t.IsBest)
		case strings.EqualFold(k, "dualAudio"):
			err = d.dec.Decode(&t.DualAudio)
		case strings.EqualFold(k, "files"):
			t.Files, err = d.decodeFiles()
		case strings.EqualFold(k, "tags"):
			t.Tags, err = d.decodeTags()
		default:
			err = d.skip()
		}
		if err != nil {
			return t, err
		}
	}
	return t, d.close()
}

// decodeFiles decodes one torrent's file list, capped at maxFilesPerTorrent.
// A File is flat (two scalar fields), so per-element json.Decoder.Decode
// cannot amplify beyond the already-capped raw bytes.
func (d *pageDecoder) decodeFiles() ([]File, error) {
	ok, err := d.open('[')
	if err != nil || !ok {
		return nil, err
	}
	var files []File
	for d.dec.More() {
		if len(files) >= maxFilesPerTorrent {
			return nil, fmt.Errorf("files per torrent exceeded cap %d (upstream misbehaving)", maxFilesPerTorrent)
		}
		if err := d.count(); err != nil {
			return nil, err
		}
		var f File
		if err := d.dec.Decode(&f); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, d.close()
}

// decodeTags decodes one torrent's tag list, capped at maxTagsPerTorrent.
func (d *pageDecoder) decodeTags() ([]string, error) {
	ok, err := d.open('[')
	if err != nil || !ok {
		return nil, err
	}
	var tags []string
	for d.dec.More() {
		if len(tags) >= maxTagsPerTorrent {
			return nil, fmt.Errorf("tags per torrent exceeded cap %d (upstream misbehaving)", maxTagsPerTorrent)
		}
		if err := d.count(); err != nil {
			return nil, err
		}
		var s string
		if err := d.dec.Decode(&s); err != nil {
			return nil, err
		}
		tags = append(tags, s)
	}
	return tags, d.close()
}

// setHeaders sets the descriptive User-Agent and JSON Accept header on each
// SeaDex request.
func setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/json")
}

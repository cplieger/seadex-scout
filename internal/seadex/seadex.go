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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
)

const (
	// DefaultPageDelay is the politeness delay between SeaDex pages. It is
	// releases.moe contract knowledge (a Cloudflare-fronted community service),
	// so it lives here beside the client that paces itself with it rather than
	// in the config leaf; the wiring site (build.go) references it.
	DefaultPageDelay = 2 * time.Second

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
	// (~a few thousand entries expected). It is deliberately slack: the
	// per-page items cap (decodeList rejects a page carrying more than perPage
	// records, merged duplicates included) already bounds a whole fetch at
	// maxPages*perPage = 100_000 entries, so this guard is belt-and-braces and
	// unreachable through FetchEntries today - fetchAndAppend's own test
	// reaches it only by pre-filling the accumulator. Raising maxPages or
	// perPage past that product is what would make it load-bearing; resize it
	// together with them.
	maxEntries = 200_000
	// maxPageBytes bounds one page (500 entries with expanded torrents) before
	// decode, guarding against an oversized or malicious payload.
	maxPageBytes = 48 << 20
	// maxTotalBytes caps cumulative page bytes across the whole fetch so a
	// compromised upstream serving few-but-huge items per page (under the
	// entry-count cap) cannot accumulate maxPages*maxPageBytes of memory.
	// The honest catalogue is a few tens of MB (still ample headroom at
	// 64 MB), and retained decoded entries grow roughly with cumulative body
	// bytes. Sized jointly with maxPageBytes and maxTotalElements so the
	// conservative SeaDex working set (decoded strings + the raw page still
	// held by fetchPage + element structs) stays under the 192 MiB budget
	// asserted by TestSeadexWorkingSetBudget, leaving over 64 MiB of the
	// 256 MiB deployment container for slice spare capacity, decoder
	// buffers, the loaded state/mapping/library snapshots, and the runtime
	// — so the guard fires (clean degradation) before the kernel OOM-kills
	// the process.
	maxTotalBytes = 64 << 20
	// maxCursorValueBytes bounds an upstream keyset cursor value before it is
	// placed in an outbound filter: a real PocketBase id is 15 alphanumerics and
	// a created value a ~24-byte ASCII timestamp, so anything longer is upstream
	// misbehavior and must not be echoed back into a request URL.
	maxCursorValueBytes = 64
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
	// thousands of elements; the live catalogue's largest page decodes ~35k).
	// Kept at or below maxTotalElements so a first-page violation still
	// classifies as per-page (fetchPage's budget-reduced check) rather than
	// fetch-wide.
	maxPageElements = 250_000
	// maxTotalElements bounds the cumulative decoded array elements across
	// the WHOLE fetch. fetchAndAppend retains every decoded entry until the
	// fetch completes, so a per-page element cap alone still lets dozens of
	// compact pages (each individually under maxPageElements, together under
	// maxTotalBytes) amplify into decoded structs and slice backing arrays
	// that OOM-kill the 256 MiB deployment container. Like the byte budget,
	// the remaining allowance caps each page's decode, so the guard fires
	// (clean degradation) before allocation scales with the hostile input.
	// Sized jointly with maxTotalBytes: worst-case element struct overhead
	// (~120 B/torrent on supported 64-bit targets x this cap, ~57 MiB) must
	// fit under the 192 MiB working-set ceiling asserted by
	// TestSeadexWorkingSetBudget TOGETHER with maxTotalBytes of decoded
	// string content and the raw page fetchPage still holds (~169 MiB
	// together). The value keeps ~3x headroom over the MEASURED live
	// catalogue - 2797 entries / 9182 torrents / 138088 files / 1258 tags =
	// 151325 elements - so ordinary SeaDex growth cannot turn this guard into
	// a permanently degraded cycle; that headroom ratio matches
	// maxTotalBytes' (18 MB observed against 64 MB).
	maxTotalElements = 500_000
)

// budgetWarnNumerator/budgetWarnDenominator express the fraction of a
// cumulative budget whose consumption is worth one WARN per fetch: the caps
// exist for hostile input, so an honest catalogue approaching one means the
// cap needs raising at the next release, not that the fetch is in trouble.
const (
	budgetWarnNumerator   = 3
	budgetWarnDenominator = 4
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
var errCumulativeElements = fmt.Errorf("seadex: cumulative decoded elements exceeded cap %d "+
	"(upstream misbehaving, or the catalogue outgrew the cap - raise maxTotalElements); "+
	"refusing to compare against a truncated view", maxTotalElements)

// fetchPage's classification of the aggregate element budget rides
// jsonx/bounded's ErrElementBudget sentinel: the full per-page bound is a
// per-page violation, while a budget-reduced limit is the fetch-wide
// cumulative cap (errCumulativeElements).

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

// ValidInfoHash returns h lowercased when it is a 40-char SHA-1 hex info hash,
// else "" (covers the releases.moe "<redacted>" placeholder and any other
// junk value).
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
// ID and Created are the immutable PocketBase fields the keyset walk pages on
// (see cursor): they are never surfaced on the public Entry, only used to
// build the next chunk's filter.
type pbEntry struct {
	ID              string   `json:"id"`
	Created         string   `json:"created"`
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
// field (space-separated or RFC3339). time.Parse accepts a fractional
// second after the seconds field even when the layout omits it, so the
// space-separated layout covers both the whole-second and the ".000"
// fractional wire forms (any fraction length).
var pbTimeLayouts = []string{"2006-01-02 15:04:05Z", time.RFC3339}

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
// reportedTotal and reportedPages retain the HIGHEST value any chunk promised
// (never overwritten downward): a later chunk whose metadata regresses — an
// empty chunk omitting totalItems decodes it as zero — must not erase an
// earlier chunk's promise of more records, or chunkComplete's outstanding-items
// guard would accept a truncated view. reportedPages no longer steers the walk
// (the keyset cursor does); it survives only for finishFetch's totalItems-fits-
// totalPages self-consistency check.
type fetchTotals struct {
	// seenAniListIDs is the identity set of every entry accepted so far, so
	// the walk can prove count completeness is also KEY completeness (see
	// validatePageIdentities).
	seenAniListIDs map[int]struct{}
	bytes          int
	elements       int
	reportedTotal  int
	reportedPages  int
	unparsedTimes  int
	unusableURLs   int
}

// cursor is the keyset position of the catalogue walk: the (created, id) pair
// of the LAST record already consumed. Offset pagination is not stable under
// deletion even with an immutable sort - deleting a record from an
// already-read prefix shifts every later record one slot forward, so the next
// numbered page silently skips the record that moved into the consumed offset
// range while the aggregate counts still agree - so each chunk instead asks
// for the records strictly AFTER this position, which no concurrent insert or
// delete can shift.
type cursor struct {
	created string
	id      string
}

// set reports whether the walk has a position yet (the first chunk has none
// and is requested unfiltered).
func (c cursor) set() bool { return c.created != "" || c.id != "" }

// filter renders the PocketBase filter selecting the records strictly after
// the cursor under sort=created,id: a later created, or the same created with
// a greater id (the composite tie-break that keeps equal-timestamp records
// from stalling or skipping the walk).
func (c cursor) filter() string {
	created, id := quoteFilterValue(c.created), quoteFilterValue(c.id)
	return "(created>" + created + "||(created=" + created + "&&id>" + id + "))"
}

// filterQuoteEscaper escapes the two characters that could break out of a
// double-quoted PocketBase filter literal. Cursor values are upstream data;
// filterSafe already refuses the shapes that have no business in a PocketBase
// id or timestamp, so this is the belt to that braces.
var filterQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// quoteFilterValue renders v as a double-quoted PocketBase filter literal.
func quoteFilterValue(v string) string {
	return `"` + filterQuoteEscaper.Replace(v) + `"`
}

// filterSafe reports whether an upstream cursor value is safe to place in a
// filter expression: no quote, backslash, or control character, and no more
// than maxCursorValueBytes long (a real PocketBase id is 15 alphanumerics and
// a created value an ASCII timestamp).
// The walk fails closed on anything else rather than sending a filter it
// cannot reason about.
func filterSafe(v string) bool {
	if len(v) > maxCursorValueBytes {
		return false
	}
	for i := range len(v) {
		if c := v[i]; c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// advanceCursor returns the keyset position after a full chunk: the (created,
// id) pair of its last record. It fails the fetch when the pair is unusable -
// missing (an upstream that stopped returning the fields the walk pages on),
// unsafe to place in a filter, or identical to the previous position (no
// progress, which would re-request the same chunk forever) - since continuing
// blind would either loop or skip records, and this client never returns a
// possibly-truncated view.
func advanceCursor(items []pbEntry, prev cursor) (cursor, error) {
	last := &items[len(items)-1]
	next := cursor{created: strings.TrimSpace(last.Created), id: strings.TrimSpace(last.ID)}
	if next.created == "" || next.id == "" {
		return prev, fmt.Errorf("seadex: page of %d records carries no usable keyset cursor "+
			"(created %q, id %q); refusing to compare against a truncated view",
			len(items), last.Created, last.ID)
	}
	if !filterSafe(next.created) || !filterSafe(next.id) {
		return prev, fmt.Errorf("seadex: keyset cursor rejected (created %q, id %q); "+
			"refusing to compare against a truncated view", next.created, next.id)
	}
	if next == prev {
		return prev, fmt.Errorf("seadex: keyset cursor did not advance past (created %q, id %q) "+
			"(upstream ignoring the pagination filter); refusing to compare against a truncated view",
			prev.created, prev.id)
	}
	return next, nil
}

// FetchEntries walks the entire entries collection with torrents expanded and
// returns every entry. The walk is KEYSET-paged on the immutable (created, id)
// pair (see cursor), so a record deleted from an already-read prefix cannot
// shift a still-existing record into a consumed offset range and out of the
// catalogue. It sleeps pageDelay between chunks. A chunk fetch failure aborts
// and returns the error; partial results are discarded so a caller never
// compares against a truncated SeaDex view. A catalogue that completes with
// ZERO entries is an error, never a success: SeaDex is never legitimately
// empty for this app's use, and accepting one would make every library item
// read as having no SeaDex coverage. A completed catalogue that retained less
// than HALF the API's reported totalItems is likewise an error. A SMALLER
// disagreement is logged (WARN) but still
// returned - pagination over a live collection can legitimately shift counts
// mid-fetch. That leniency requires the walk to have ended on a SHORT chunk:
// an EMPTY chunk after a full one while the collected count is still below the
// reported totalItems aborts with an error (chunkComplete), since the API
// itself says entries remain and completing would falsely resolve findings
// against a truncated view.
func (c *Client) FetchEntries(ctx context.Context) ([]Entry, error) {
	var all []Entry
	var tot fetchTotals
	var cur cursor
	for page := 1; page <= maxPages; page++ {
		if page > 1 {
			if err := httpx.SleepCtx(ctx, c.pageDelay); err != nil {
				return nil, fmt.Errorf("seadex: interrupted between pages: %w", err)
			}
		}
		var done bool
		var err error
		all, done, err = c.fetchAndAppend(ctx, page, all, &tot, &cur)
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
// app's use, whether the API reported zero totals or served empty pages), a
// collected count below HALF the API's reported totalItems is an error (the
// app-wide shrink policy - no credible mid-fetch delete loses half a
// catalogue, and this was the last path accepting a truncated view), and a
// smaller disagreement logs the alert-stable count-mismatch WARN but still
// returns the entries. Entries
// whose non-empty updated timestamp failed to parse (zeroed, sorting to the
// feed's tail) are surfaced as one aggregate WARN so an upstream format drift
// that zeroes the whole catalogue is alertable without per-record noise.
// Torrents whose URL is unusable (omitted/empty, or a non-empty value the
// publisher dropped to "": a foreign host under a trusted label, an
// unknown tracker, a malformed URL) are likewise surfaced as one aggregate
// WARN — filter.Obtainable treats both cases as unobtainable — so a schema
// drift that strips every release link is alertable instead of silent.
//
// That counter is the ONE reason this client reads a publish policy at all
// (trackerlink.Publish, itself a pure leaf over the canonical tracker table):
// the diagnostic is about UPSTREAM DATA QUALITY across the whole catalogue in
// one pass, which only the client sees, and it is deliberately defined against
// the same rule filter.Obtainable applies rather than a weaker
// is-the-field-blank test that would miss a wholesale host drift. The client
// carries no other link knowledge - the publish policy itself lives in
// internal/trackerlink beside its hide half (l-f86).
func (c *Client) finishFetch(all []Entry, tot fetchTotals) ([]Entry, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("seadex: returned an empty catalogue (totalItems=%d); "+
			"SeaDex is never legitimately empty, refusing to compare against it", tot.reportedTotal)
	}
	if tot.reportedTotal > tot.reportedPages*perPage {
		return nil, fmt.Errorf("seadex: reported totalItems %d cannot fit the reported %d pages of %d (upstream misbehaving); "+
			"refusing to compare against a truncated view", tot.reportedTotal, tot.reportedPages, perPage)
	}
	if degradation.Shrunk(len(all), tot.reportedTotal) {
		// The keyset cursor makes a SKIPPED record structurally impossible (see
		// cursor), so a shortfall against the API's own reported total can only
		// be a mid-fetch delete - or an upstream that ended the walk early with
		// a short chunk while records remained. A handful of deletions during a
		// ~6-chunk walk is ordinary and stays the WARN below; losing more than
		// HALF the catalogue mid-fetch is not credible, and this was the last
		// path on which a truncated view was accepted at all (the empty-chunk
		// and metadata-inconsistency arms already fail). Erroring degrades the
		// cycle, which PRESERVES existing findings - the fail-safe direction -
		// where completing would resolve every finding whose entry vanished.
		// The below-half trigger is the app-wide shrink policy
		// (degradation.ShrinkGuardFactor), the same one the mapping refresh and
		// library-walk guards apply, rather than a second threshold of its own.
		return nil, fmt.Errorf("seadex: collected %d of %d reported entries (below half); "+
			"refusing to compare against a truncated view", len(all), tot.reportedTotal)
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
	if tot.bytes*budgetWarnDenominator >= maxTotalBytes*budgetWarnNumerator ||
		tot.elements*budgetWarnDenominator >= maxTotalElements*budgetWarnNumerator {
		c.log.Warn("seadex fetch budget mostly spent; raise the caps before the catalogue outgrows them",
			"bytes", tot.bytes, "max_bytes", maxTotalBytes,
			"elements", tot.elements, "max_elements", maxTotalElements)
	}
	c.log.Debug("seadex entries fetched", "entries", len(all),
		"bytes", tot.bytes, "elements", tot.elements)
	return all, nil
}

// fetchAndAppend fetches one chunk at the walk's cursor, appends its entries,
// updates the running totals (cumulative bytes and decoded elements, the API's
// reported item total, and the unparseable-updated and unusable-URL counters),
// enforces the cumulative-byte, cumulative-element, and entry-count caps,
// validates the chunk's entry identities (validatePageIdentities),
// advances the cursor past the chunk when the walk continues, and reports
// whether pagination is complete. All caps run BEFORE allocation scales with
// the hostile input: the cumulative-byte budget caps the wire read itself
// (fetchPage downloads at most the remaining budget, so tot.bytes can never
// exceed maxTotalBytes), the cumulative-element budget caps the decode
// (fetchPage decodes at most the remaining element allowance, so tot.elements
// can never exceed maxTotalElements), and the entry-count cap rejects the
// chunk before any of its items are converted or appended.
func (c *Client) fetchAndAppend(ctx context.Context, page int, all []Entry, tot *fetchTotals, cur *cursor) (out []Entry, done bool, err error) {
	remaining := int64(maxTotalBytes - tot.bytes)
	if remaining <= 0 {
		return all, false, fmt.Errorf("%w (page %d, %d entries fetched)", errCumulativeBytes, page, len(all))
	}
	remainingElems := maxTotalElements - tot.elements
	if remainingElems <= 0 {
		return all, false, fmt.Errorf("%w (page %d, %d entries fetched)", errCumulativeElements, page, len(all))
	}
	list, n, elems, err := c.fetchPage(ctx, *cur, min(int64(maxPageBytes), remaining), min(maxPageElements, remainingElems))
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
	if verr := validatePageIdentities(list.Items, page, tot); verr != nil {
		return all, false, verr
	}
	all = appendPageEntries(all, list.Items, tot)
	done, err = chunkComplete(page, len(list.Items), len(all), tot.reportedTotal)
	if err != nil {
		return all, false, err
	}
	if !done {
		// The walk continues, so the next chunk is requested strictly after
		// this one's last record; an unusable cursor fails the fetch rather
		// than looping or skipping (advanceCursor).
		next, cerr := advanceCursor(list.Items, *cur)
		if cerr != nil {
			return all, false, cerr
		}
		*cur = next
	}
	return all, done, nil
}

// validatePageIdentities enforces the catalogue's primary-key invariant across
// the whole walk: every entry is keyed by ONE positive, unique AniList ID. The
// byte/element/count budgets prove a chunk is well-shaped and the pagination
// arithmetic proves the counts add up, but neither notices key loss - an entry
// that omits alID decodes it as 0 (which the matcher would silently treat as
// unmapped) and a repeated alID can stand in for a record that was dropped,
// both while the aggregate counts still agree. Failing the whole fetch is the
// fail-safe direction: the caller preserves the last known findings and feed
// instead of resolving them against a catalogue that lost an anime.
func validatePageIdentities(items []pbEntry, page int, tot *fetchTotals) error {
	if tot.seenAniListIDs == nil {
		tot.seenAniListIDs = make(map[int]struct{}, len(items))
	}
	for i := range items {
		id := items[i].AlID
		if id <= 0 {
			return fmt.Errorf("seadex: page %d item %d has invalid AniList ID %d; "+
				"refusing to compare against an incomplete catalogue", page, i+1, id)
		}
		if _, exists := tot.seenAniListIDs[id]; exists {
			return fmt.Errorf("seadex: page %d repeats AniList ID %d; "+
				"refusing to compare against a possibly truncated catalogue", page, id)
		}
		tot.seenAniListIDs[id] = struct{}{}
	}
	return nil
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
			if trackerlink.Publish(entry.Torrents[j].Tracker, entry.Torrents[j].URL) == "" {
				tot.unusableURLs++
			}
		}
		all = append(all, entry)
	}
	return all
}

// chunkComplete reports whether the keyset walk is done after a chunk: a chunk
// short of perPage is the last one (the filter asked for everything after the
// cursor, so a partial chunk means the collection is exhausted), while a FULL
// chunk always continues. Under keyset pagination completeness is a property
// of the chunk itself, not of the response's page metadata (a numbered-page
// count cannot skip or duplicate what a cursor walk reads), so totalPages no
// longer steers the walk; it survives only as finishFetch's metadata
// self-consistency check.
//
// One arm stays an error: an EMPTY chunk after a full one, while the entries
// collected so far are still below the reported totalItems, or the response
// carries no reported total at all (an omitted totalItems decodes to zero, so
// there is nothing left to vouch for the walk's completeness). The API itself
// says entries remain — or declines to say anything — so completing would hand
// downstream a truncated view that falsely resolves findings; failing instead
// degrades the cycle, the
// fail-safe direction that preserves existing findings. A SHORT (non-empty)
// terminal chunk with a count mismatch stays finishFetch's WARN (pagination
// over a live collection can legitimately shift counts mid-fetch), and an
// empty FIRST chunk completes the walk so finishFetch's empty-catalogue guard
// converts it into an error.
func chunkComplete(page, itemCount, fetched, reportedTotal int) (done bool, err error) {
	if itemCount >= perPage {
		return false, nil
	}
	if itemCount == 0 && page > 1 {
		// An empty follow-up chunk is only a legitimate terminal state when the
		// API's own reported total vouches for it. With no reported total there is
		// nothing to check the walk against, so completing would hand downstream a
		// possibly-truncated catalogue — the one thing FetchEntries promises never
		// to do (the pre-keyset walk rejected metadata-less responses outright).
		if reportedTotal <= 0 {
			return false, fmt.Errorf("seadex: page %d empty with %d entries fetched and no reported total to "+
				"vouch for completeness; refusing to compare against a truncated view", page, fetched)
		}
		if fetched < reportedTotal {
			return false, fmt.Errorf("seadex: page %d empty with %d of %d reported entries fetched; "+
				"refusing to compare against a truncated view", page, fetched, reportedTotal)
		}
	}
	return true, nil
}

// fetchPage fetches and decodes a single chunk of entries at the walk's cursor,
// also returning the raw body size and the decoded array-element count so the
// caller can bound cumulative bytes and decoded elements across chunks. Every
// request asks for page 1 of the sorted remainder: the cursor's filter (absent
// on the first chunk) is what advances the walk, so no numbered offset can go
// stale under a concurrent delete. wireLimit is the download cap for THIS
// chunk: the per-page bound
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
func (c *Client) fetchPage(ctx context.Context, cur cursor, wireLimit int64, elemLimit int) (list pbList, bodyBytes, elems int, err error) {
	q := url.Values{
		"expand":  {"trs"},
		"page":    {"1"},
		"perPage": {strconv.Itoa(perPage)},
		// Sort on immutable fields: created,id is stable under updates (an
		// entry updated mid-walk cannot move across chunks) and is the key the
		// cursor filter below pages on.
		"sort": {"created,id"},
	}
	if cur.set() {
		q.Set("filter", cur.filter())
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
		if errors.Is(err, bounded.ErrElementBudget) && elemLimit < maxPageElements {
			return pbList{}, 0, 0, errCumulativeElements
		}
		return pbList{}, 0, 0, fmt.Errorf("decode page: %w", err)
	}
	return list, len(body), elems, nil
}

// ---- Bounded token-level page decoder ----
//
// decodePage and the decode* functions below form a schema-aware bounded
// decoder for one pbList page, built on jsonx/bounded. Unlike
// json.Unmarshal - which materializes the entire decoded value before any
// caller-side count check can run, letting compact serialized elements
// amplify a wire-capped body into decoded structs and slice backing arrays
// far beyond maxPageBytes - the token walk enforces every cardinality cap
// (perPage items, maxTorrentsPerEntry, maxFilesPerTorrent,
// maxTagsPerTorrent, and the aggregate maxPageElements budget) BEFORE
// appending each element, so allocation never scales with hostile array
// cardinality. The library owns the json.Unmarshal-parity building blocks
// (null-into-container no-ops, duplicate-key merge via each Array call's
// prior argument, unknown-field token skipping, UseNumber so a skipped
// 1e1000 stays valid); the dispatch functions below own only which keys
// exist, their scalar targets, and their caps. Keys match with
// strings.EqualFold, json.Unmarshal's case-insensitive field fallback.

// decodePage decodes one page body under the bounded-decoder caps, rejecting
// trailing data after the top-level value (matching json.Unmarshal
// strictness). elemLimit is the aggregate element budget for this page (the
// per-page bound, possibly reduced to the fetch-wide remaining allowance by
// fetchAndAppend); the decoded element count is returned so the caller can
// charge the fetch-wide budget.
func decodePage(body []byte, elemLimit int) (pbList, int, error) {
	d := bounded.NewDecoder(bytes.NewReader(body), elemLimit)
	list, err := decodeList(d)
	if err != nil {
		return pbList{}, 0, err
	}
	if err := d.End(); err != nil {
		return pbList{}, 0, err
	}
	return list, d.Elements(), nil
}

// decodeList decodes the pbList envelope. The items array is capped at
// perPage: the request asks for perPage records, so a page stuffing more is
// upstream misbehavior and is rejected before the excess is decoded.
func decodeList(d *bounded.Decoder) (pbList, error) {
	var list pbList
	err := d.Object(func(k string) error {
		switch {
		case strings.EqualFold(k, "items"):
			var err error
			list.Items, err = bounded.Array(d, list.Items, perPage, "page items",
				func(e *pbEntry) error { return decodeEntry(d, e) })
			return err
		case strings.EqualFold(k, "totalItems"):
			return d.Decode(&list.TotalItems)
		case strings.EqualFold(k, "totalPages"):
			return d.Decode(&list.TotalPages)
		default:
			return d.Skip()
		}
	})
	return list, err
}

// decodeEntry decodes one entries record field-wise into e; the Object walk
// gives json.Unmarshal's duplicate-key semantics (a JSON null element is a
// no-op that preserves the existing value, and an object only overwrites
// the fields it actually carries).
func decodeEntry(d *bounded.Decoder, e *pbEntry) error {
	return d.Object(func(k string) error { return decodeEntryField(d, e, k) })
}

// decodeEntryField decodes one entries-record field (or skips an unknown
// key).
func decodeEntryField(d *bounded.Decoder, e *pbEntry, key string) error {
	switch {
	case strings.EqualFold(key, "notes"):
		return d.Decode(&e.Notes)
	case strings.EqualFold(key, "theoreticalBest"):
		return d.Decode(&e.TheoreticalBest)
	case strings.EqualFold(key, "updated"):
		return d.Decode(&e.Updated)
	case strings.EqualFold(key, "id"):
		return d.Decode(&e.ID)
	case strings.EqualFold(key, "created"):
		return d.Decode(&e.Created)
	case strings.EqualFold(key, "alID"):
		return d.Decode(&e.AlID)
	case strings.EqualFold(key, "incomplete"):
		return d.Decode(&e.Incomplete)
	case strings.EqualFold(key, "expand"):
		// Decode into the existing value so neither a duplicate
		// "expand":null nor a duplicate/partial "expand":{} can wipe an
		// already-decoded trs (Object's null no-op + field-wise merge).
		return decodeExpand(d, &e.Expand)
	default:
		return d.Skip()
	}
}

// decodeExpand decodes the expand relation envelope field-wise into ex. The
// trs relation is capped at maxTorrentsPerEntry; a repeated "trs" decodes
// INTO the existing slice (bounded.Array's prior), matching
// json.Unmarshal's duplicate-key slice semantics.
func decodeExpand(d *bounded.Decoder, ex *pbExpand) error {
	return d.Object(func(k string) error {
		if strings.EqualFold(k, "trs") {
			var err error
			ex.Trs, err = bounded.Array(d, ex.Trs, maxTorrentsPerEntry, "torrents per entry",
				func(t *Torrent) error { return decodeTorrent(d, t) })
			return err
		}
		return d.Skip()
	})
}

// decodeTorrent decodes one torrent record field-wise into t (see
// decodeEntry for the duplicate-key semantics the Object walk provides).
func decodeTorrent(d *bounded.Decoder, t *Torrent) error {
	return d.Object(func(k string) error { return decodeTorrentField(d, t, k) })
}

// decodeTorrentField decodes one torrent-record field (or skips an unknown
// key). The files and tags arrays are capped per torrent; a File is flat
// (two scalar fields), so per-element json.Decoder.Decode cannot amplify
// beyond the already-capped raw bytes.
func decodeTorrentField(d *bounded.Decoder, t *Torrent, key string) error {
	switch {
	case strings.EqualFold(key, "releaseGroup"):
		return d.Decode(&t.ReleaseGroup)
	case strings.EqualFold(key, "tracker"):
		return d.Decode(&t.Tracker)
	case strings.EqualFold(key, "infoHash"):
		return d.Decode(&t.InfoHash)
	case strings.EqualFold(key, "url"):
		return d.Decode(&t.URL)
	case strings.EqualFold(key, "isBest"):
		return d.Decode(&t.IsBest)
	case strings.EqualFold(key, "dualAudio"):
		return d.Decode(&t.DualAudio)
	case strings.EqualFold(key, "files"):
		var err error
		t.Files, err = bounded.Array(d, t.Files, maxFilesPerTorrent, "files per torrent",
			func(f *File) error { return d.Decode(f) })
		return err
	case strings.EqualFold(key, "tags"):
		var err error
		t.Tags, err = bounded.Array(d, t.Tags, maxTagsPerTorrent, "tags per torrent",
			func(s *string) error { return d.Decode(s) })
		return err
	default:
		return d.Skip()
	}
}

// setHeaders sets the descriptive User-Agent and JSON Accept header on each
// SeaDex request.
func setHeaders(req *http.Request) {
	req.Header.Set("User-Agent", appinfo.UserAgent)
	req.Header.Set("Accept", "application/json")
}

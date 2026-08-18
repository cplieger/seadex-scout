// Package seadexapi is the read client for the SeaDex (releases.moe) PocketBase
// API.
//
// It pages through the entries collection with the torrents relation expanded,
// is polite to the Cloudflare-fronted community service (a descriptive
// User-Agent and a configurable inter-page delay), and bounds every response
// before decoding. It is read-only and never authenticates.
//
// The wire shape, the paging pipeline and the decode budgets change with the
// releases.moe API; the MODEL they produce (internal/seadex) changes with this app's
// comparison rules.
package seadexapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/jsonx/bounded"
	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/seadex-scout/internal/appinfo"
	"github.com/cplieger/seadex-scout/internal/degradation"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

const (
	// DefaultPageDelay is the politeness delay between SeaDex pages. It is releases.moe
	// contract knowledge, so it lives beside the client that paces itself with it.
	DefaultPageDelay = 2 * time.Second

	// entriesPath is the PocketBase collection endpoint for SeaDex entries.
	entriesPath = "/api/collections/entries/records"
	// perPage is the page size. The full set is a few thousand entries, so
	// 500/page keeps it to a handful of requests.
	perPage = 500
	// maxPages caps pagination so a misbehaving API cannot loop forever
	// (~6 pages expected at perPage=500).
	maxPages = 200
	// maxEntries is the ceiling a whole fetch's accumulated entries must stay under. It is
	// not enforced at runtime because it cannot be crossed: the per-page items cap and
	// the maxPages bound cap a walk at maxPages*perPage, and the guard below keeps it so.
	maxEntries = 200_000
	// maxPageBytes bounds one page (500 entries with expanded torrents) before
	// decode, guarding against an oversized or malicious payload.
	maxPageBytes = 48 << 20

	// MaxWindowEntries is the window size a caller must NOT reach: the bound is exclusive,
	// so a window strictly under it is always ONE request. At or over it a caller defers
	// to a full pass, since the walk sorts on `created` and page 1 holds the OLDEST.
	MaxWindowEntries = perPage

	// maxProbeBytes bounds CountWindow's response. It carries one id and the
	// list metadata - measured at 88 bytes - so 4 KiB is a generous ceiling
	// that still refuses a body that is not the shape asked for.
	maxProbeBytes = 4 << 10

	// maxTotalBytes caps cumulative page bytes across the whole fetch, so an upstream
	// serving few-but-huge items cannot accumulate maxPages*maxPageBytes. Sized so the
	// working set stays under the budget TestSeadexWorkingSetBudget asserts.
	maxTotalBytes = 64 << 20
	// maxCursorValueBytes bounds an upstream keyset cursor value before it is placed in
	// an outbound filter: a real PocketBase id is 15 alphanumerics and a created value a
	// ~24-byte ASCII timestamp, so anything longer must not be echoed into a request.
	maxCursorValueBytes = 64
	// maxLoggedCursorBytes bounds a REJECTED cursor value before it is quoted into an
	// error internal/scout logs as a slog attribute, so a hostile page cannot balloon a
	// Loki record. Sized just over maxCursorValueBytes so an honest value stays readable.
	maxLoggedCursorBytes = 128
	// maxLoggedDecodeBytes bounds a page-DECODE failure's rendered text: stdlib json
	// renders a rejected number literal verbatim, so the message is otherwise bounded
	// only by maxPageBytes. An amplifying literal keeps only its head.
	maxLoggedDecodeBytes = 512
	// maxAttempts / baseDelay bound the per-page retry.
	maxAttempts = 3
	baseDelay   = time.Second
	// maxFetchDuration bounds the WHOLE walk: the per-attempt bound is the client's own
	// timeout, so maxPages chunks x maxAttempts retries is a multi-hour worst case. One
	// hour is above the honest ceiling and below the deployed poll_interval.
	maxFetchDuration = time.Hour
)

// Cardinality caps on one decoded page, enforced by decodePage DURING the token-level
// decode. json.Unmarshal materializes the whole decoded value before any caller-side
// count check can run, so compact serialized elements could otherwise amplify a bounded
// body far beyond maxPageBytes. The values are generous headroom over the honest
// catalogue, not tuning knobs; a page crossing one aborts the fetch.
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
	// maxPageElements bounds the TOTAL decoded array elements of one page: the per-parent
	// caps compose multiplicatively. Kept at or below maxTotalElements so a first-page
	// violation still classifies as per-page.
	maxPageElements = 250_000
	// maxTotalElements bounds the cumulative decoded array elements across the WHOLE
	// fetch: a per-page cap alone still lets dozens of compact pages amplify into structs
	// that OOM-kill the container. It keeps ~3x headroom over the live catalogue.
	maxTotalElements = 500_000
)

// Compile-time guard on the relation fetchPage's cumulative-vs-per-page classification
// infers from: each per-page bound must stay at or below its fetch-wide budget, so a
// limit below the full bound can only mean the CUMULATIVE cap reduced it. Raising a
// per-page bound past its budget would misreport the first oversized page as fetch-wide
// exhaustion; the negative difference fails the build instead.
const (
	_ = uint(maxTotalBytes - maxPageBytes)
	_ = uint(maxTotalElements - maxPageElements)
	// The walk's structural entry ceiling (maxPages pages of at most perPage items) must
	// stay under maxEntries, which is why no runtime entry-count guard is needed.
	_ = uint(maxEntries - maxPages*perPage)
)

// budgetWarnNumerator/budgetWarnDenominator express the fraction of a cumulative
// budget whose consumption is worth one WARN per fetch: the caps exist for hostile
// input, so an honest catalogue approaching one means the cap needs raising.
const (
	budgetWarnNumerator   = 3
	budgetWarnDenominator = 4
)

// errCumulativeBytes reports the cumulative-byte budget (maxTotalBytes) being
// exceeded. It is raised at the wire layer - fetchPage caps each download at the
// REMAINING budget - so an over-budget page is rejected before decode.
var errCumulativeBytes = fmt.Errorf("seadex: cumulative page bytes exceeded cap %d "+
	"(upstream misbehaving, or the catalogue outgrew the cap - raise maxTotalBytes); "+
	"refusing to compare against a truncated view", maxTotalBytes)

// errCumulativeElements reports the fetch-wide decoded-element budget
// (maxTotalElements) being exceeded. Like errCumulativeBytes it is enforced during the
// decode, so an over-budget page is rejected before the excess is materialized.
var errCumulativeElements = fmt.Errorf("seadex: decoded elements exceeded the remaining fetch-wide budget "+
	"(cap %d; upstream misbehaving, or the catalogue outgrew the cap - raise maxTotalElements, "+
	"and maxPageElements too if one page alone carries more than %d elements); "+
	"refusing to compare against a truncated view", maxTotalElements, maxPageElements)

// fetchPage's classification of the aggregate element budget rides jsonx/bounded's
// ErrElementBudget sentinel: the full per-page bound is a per-page violation, while a
// budget-reduced limit is the fetch-wide cumulative cap.

// Client fetches entries from a SeaDex PocketBase instance.
type Client struct {
	http      *http.Client
	log       *slog.Logger
	baseURL   string
	pageDelay time.Duration
	// mu guards lastAccepted, the in-process catalogue-size baseline
	// warnCatalogueShrink compares against. FetchEntries is serialized per process
	// today, so the mutex buys a future concurrent caller safety.
	mu           sync.Mutex
	lastAccepted int
}

// cfg holds the resolved tuning knobs for a Client.
type cfg struct {
	logger    *slog.Logger
	pageDelay time.Duration
}

// Option tunes a Client built by NewClient.
type Option func(*cfg)

// WithPageDelay sets the politeness delay slept between pages of a walk.
// Defaults to no delay; DefaultPageDelay is the value the app runs.
func WithPageDelay(d time.Duration) Option {
	return func(c *cfg) { c.pageDelay = d }
}

// WithLogger sets the logger the client's diagnostics go to. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *cfg) { c.logger = l }
}

// NewClient returns a SeaDex client for baseURL (e.g. "https://releases.moe")
// using the given HTTP client.
func NewClient(httpClient *http.Client, baseURL string, opts ...Option) *Client {
	c := &cfg{}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return &Client{
		http:      httpClient,
		log:       c.logger,
		baseURL:   baseURL,
		pageDelay: c.pageDelay,
	}
}

// ---- PocketBase wire model and paging pipeline ----

// pbList is the PocketBase list-response envelope for the entries collection.
type pbList struct {
	Items      []pbEntry `json:"items"`
	TotalItems int       `json:"totalItems"`
	TotalPages int       `json:"totalPages"`
}

// pbEntry mirrors an entries record with the torrents relation expanded. ID and Created
// are the immutable PocketBase fields the keyset walk pages on; they are never surfaced
// on the public Entry, only used to build the next chunk's filter.
type pbEntry struct {
	ID              string   `json:"id"`
	Created         string   `json:"created"`
	Notes           string   `json:"notes"`
	TheoreticalBest string   `json:"theoreticalBest"`
	Expand          pbExpand `json:"expand"`
	AlID            int      `json:"alID"`
	Incomplete      bool     `json:"incomplete"`
}

// pbExpand holds the expanded torrents relation (?expand=trs).
type pbExpand struct {
	Trs []seadex.Torrent `json:"trs"`
}

// toEntry converts a decoded PocketBase record into a public Entry.
func (r *pbEntry) toEntry() seadex.Entry {
	return seadex.Entry{
		Torrents:        r.Expand.Trs,
		Notes:           r.Notes,
		TheoreticalBest: r.TheoreticalBest,
		AniListID:       r.AlID,
		Incomplete:      r.Incomplete,
	}
}

// fetchTotals accumulates the cross-page counters of one FetchEntries run.
// reportedTotal and reportedPages retain the HIGHEST value any chunk promised, which is
// load-bearing on EVERY chunk: only the FIRST is requested unfiltered, so every later
// chunk reports the totals of the remaining suffix, which legitimately shrinks. A
// last-writer assignment would hand both completeness guards a shrinking denominator
// and let a truncated walk satisfy them.
type fetchTotals struct {
	// seenAniListIDs is the identity set of every entry accepted so far, so
	// the walk can prove count completeness is also KEY completeness (see
	// validatePageIdentities).
	seenAniListIDs map[int]struct{}
	bytes          int
	elements       int
	reportedTotal  int
	reportedPages  int
	// chunks counts the walk's delivered chunks. A ONE-chunk walk is the only shape
	// whose delivered count and the totalItems that counts them arrive in the SAME
	// response, which is what makes the window shortfall a sound signal.
	chunks int
}

// cursor is the keyset position of the catalogue walk: the (created, id) pair of the
// LAST record already consumed. Offset pagination is not stable under deletion even
// with an immutable sort - deleting a record from an already-read prefix shifts every
// later record one slot forward, so the next numbered page silently skips one while the
// aggregate counts still agree.
type cursor struct {
	created string
	id      string
}

// set reports whether the walk has a position yet (the first chunk has none
// and is requested unfiltered).
func (c cursor) set() bool { return c.created != "" || c.id != "" }

// filter renders the PocketBase filter selecting the records strictly after the cursor
// under sort=created,id: a later created, or the same created with a greater id (the
// tie-break that keeps equal-timestamp records from stalling or skipping the walk).
func (c cursor) filter() string {
	created, id := quoteFilterValue(c.created), quoteFilterValue(c.id)
	return "(created>" + created + "||(created=" + created + "&&id>" + id + "))"
}

// joinFilters renders the page request's filter: the keyset cursor's
// strictly-after clause, the window's changed-since clause, or both ANDed. An
// empty result means an unfiltered first page of a full walk.
func joinFilters(cur cursor, opts Options) string {
	parts := make([]string, 0, 2)
	if cur.set() {
		parts = append(parts, cur.filter())
	}
	if opts.Mode == FetchWindow {
		parts = append(parts, windowFilter(opts.Since))
	}
	return strings.Join(parts, "&&")
}

// filterQuoteEscaper escapes the two characters that could break out of a
// double-quoted PocketBase filter literal. Cursor values are upstream data, and
// filterSafe already refuses the shapes with no business in an id or timestamp.
var filterQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// quoteFilterValue renders v as a double-quoted PocketBase filter literal.
func quoteFilterValue(v string) string {
	return `"` + filterQuoteEscaper.Replace(v) + `"`
}

// filterSafe reports whether an upstream cursor value is safe to place in a filter
// expression: no quote, backslash or control character, and no longer than
// maxCursorValueBytes. The walk fails closed on anything else.
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

// logCursor bounds and cleans one untrusted cursor value before it is quoted into an
// error internal/scout logs as a slog attribute; the single application of
// maxLoggedCursorBytes.
func logCursor(v string) string {
	return runesafe.SanitizeSingleLineBounded(v, maxLoggedCursorBytes)
}

// recordCursor reads one record's (created, id) keyset pair, trimmed, and fails when
// the pair cannot be used: missing, or unsafe to place in a filter. index (1-based) and
// chunkLen only shape the diagnostic, naming WHICH record drifted when both fields are
// blank.
func recordCursor(item *pbEntry, index, chunkLen int) (cursor, error) {
	c := cursor{created: strings.TrimSpace(item.Created), id: strings.TrimSpace(item.ID)}
	if c.created == "" || c.id == "" {
		return cursor{}, fmt.Errorf("seadex: record %d of %d carries no usable keyset cursor "+
			"(created %q, id %q); refusing to compare against a truncated view",
			index, chunkLen, logCursor(item.Created), logCursor(item.ID))
	}
	if !filterSafe(c.created) || !filterSafe(c.id) {
		return cursor{}, fmt.Errorf("seadex: keyset cursor rejected at record %d of %d "+
			"(created %q, id %q); refusing to compare against a truncated view",
			index, chunkLen, logCursor(c.created), logCursor(c.id))
	}
	return c, nil
}

// cursorAdvances reports whether next sorts strictly after prev under the walk's
// sort=created,id ordering. Equality and any regression both read as no progress.
func cursorAdvances(next, prev cursor) bool {
	return next.created > prev.created ||
		(next.created == prev.created && next.id > prev.id)
}

// advanceCursor validates a non-empty chunk's whole keyset sequence and returns the
// position after it. EVERY record is checked, in order, from the previous position,
// because that ordering premise is what the walk's completeness argument rests on: a
// chunk shorter than perPage is read as exhaustion only because the filter asked for
// everything after the cursor. So it runs for a SHORT terminal chunk too.
//
// It fails the fetch when any record's pair is unusable - missing, unsafe, or not
// strictly after its predecessor (equality would re-request forever, a regression would
// re-read a consumed prefix while later records went unread).
func advanceCursor(items []pbEntry, prev cursor) (cursor, error) {
	pos := prev
	for i := range items {
		next, err := recordCursor(&items[i], i+1, len(items))
		if err != nil {
			return prev, err
		}
		if pos.set() && !cursorAdvances(next, pos) {
			return prev, fmt.Errorf("seadex: keyset cursor did not advance past (created %q, id %q); "+
				"got (created %q, id %q) at record %d of %d "+
				"(upstream ignoring the pagination filter or its sort order); "+
				"refusing to compare against a truncated view",
				logCursor(pos.created), logCursor(pos.id),
				logCursor(next.created), logCursor(next.id),
				i+1, len(items))
		}
		pos = next
	}
	return pos, nil
}

// FetchMode selects a fetch's COMPLETENESS POLICY. The wire request is the same either
// way - one filter conjunct apart - but what counts as a valid result is not, and
// conflating them is how a windowed fetch would inherit whole-catalogue guards.
type FetchMode uint8

const (
	// FetchFull walks the whole collection. Every completeness guard applies:
	// SeaDex is never legitimately empty, a walk no reported total vouches for
	// is refused, and a below-half shortfall is refused.
	FetchFull FetchMode = iota
	// FetchWindow walks only the records changed since Options.Since. It is legitimately
	// EMPTY (measured: 6 of 90 days upstream had no change), so the empty-catalogue and
	// no-reported-total guards must not apply; every STRUCTURAL guard still does.
	FetchWindow
)

// Options selects what a fetch retrieves and how its result is judged.
// The zero value is a full-catalogue walk.
type Options struct {
	// Since bounds a FetchWindow to records whose `updated` is strictly after it.
	// Ignored by FetchFull. A zero Since is an error rather than a silent full fetch: the
	// zero time is also what a failed timestamp parse yields.
	Since time.Time
	Mode  FetchMode
}

// windowFilter renders the PocketBase filter conjunct selecting records changed since
// t. It is ANDed with the keyset cursor's filter, so the walk pages on the IMMUTABLE
// (created, id) pair while selecting on the mutable `updated` - sorting on `updated`
// would let a record edited mid-walk move between chunks and be skipped.
func windowFilter(t time.Time) string {
	return "updated>" + quoteFilterValue(t.UTC().Format("2006-01-02 15:04:05.000Z"))
}

// CountWindow reports how many records changed since t, without downloading any of
// them: one request of ~88 bytes (perPage=1, fields=id), read off totalItems. It is the
// tick's cost bound - reading the count off a real page would download up to ~2.9 MiB
// per tick, and a one-page cap cannot substitute because page 1 of an oversized window
// holds the OLDEST records. A negative total is an error, not a zero: PocketBase
// answers totalItems -1 when asked to skip the count.
func (c *Client) CountWindow(ctx context.Context, since time.Time) (int, error) {
	if since.IsZero() {
		return 0, errors.New("seadex: CountWindow needs a non-zero since")
	}
	q := url.Values{
		"page":    {"1"},
		"perPage": {"1"},
		"fields":  {"id"},
		"filter":  {windowFilter(since)},
	}
	body, err := httpx.GetBytes(ctx, c.http, c.baseURL+entriesPath+"?"+q.Encode(),
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithMaxBodyBytes(maxProbeBytes),
		httpx.WithHeaders(setHeaders),
		httpx.WithLogger(c.log),
		httpx.WithExhaustedLevel(slog.LevelDebug),
	)
	if err != nil {
		return 0, fmt.Errorf("seadex: count window: %w", err)
	}
	var list pbList
	if err := json.Unmarshal(body, &list); err != nil {
		// Bounded like the page decoder's arm: stdlib json can render a rejected number
		// literal verbatim, so the message is otherwise bounded only by the body cap.
		return 0, fmt.Errorf("seadex: decode window count: %s",
			runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedDecodeBytes))
	}
	if list.TotalItems < 0 {
		return 0, fmt.Errorf("seadex: window count reported a negative total (%d); "+
			"upstream refused to count", list.TotalItems)
	}
	return list.TotalItems, nil
}

// FetchEntries walks the entire entries collection with torrents expanded and returns
// every entry. The walk is KEYSET-paged on the immutable (created, id) pair, so a
// record deleted from an already-read prefix cannot shift a still-existing record out
// of the catalogue. It sleeps pageDelay between chunks, and a chunk failure aborts:
// partial results are discarded so a caller never compares against a truncated view.
//
// A catalogue completing with ZERO entries is an error, as is one that retained less
// than HALF the reported totalItems; a smaller disagreement is logged and still
// returned, provided the walk ended on a SHORT chunk rather than an empty one.
func (c *Client) FetchEntries(ctx context.Context, opts Options) ([]seadex.Entry, error) {
	if opts.Mode == FetchWindow && opts.Since.IsZero() {
		return nil, errors.New("seadex: FetchWindow needs a non-zero Since")
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, maxFetchDuration)
	defer cancel()
	var all []seadex.Entry
	var tot fetchTotals
	var cur cursor
	for page := 1; page <= maxPages; page++ {
		if page > 1 {
			if err := httpx.SleepCtx(ctx, c.pageDelay); err != nil {
				return nil, walkBudgetError(parent, ctx,
					fmt.Errorf("seadex: interrupted between pages: %w", err), page, len(all))
			}
		}
		var done bool
		var err error
		all, done, err = c.fetchAndAppend(ctx, page, all, &tot, &cur, opts)
		if err != nil {
			return nil, walkBudgetError(parent, ctx, err, page, len(all))
		}
		c.log.Debug("seadex chunk fetched", "page", page, "entries", len(all),
			"reported_total", tot.reportedTotal, "done", done,
			"bytes", tot.bytes, "elements", tot.elements)
		if done {
			return c.finishFetch(all, tot, opts.Mode)
		}
	}
	return nil, fmt.Errorf("seadex: pagination exceeded max %d pages after %d entries fetched "+
		"(upstream still serving full pages; reported total %d); "+
		"refusing to compare against a truncated view", maxPages, len(all), tot.reportedTotal)
}

// walkBudgetError names maxFetchDuration when the WALK's own deadline is what ended the
// fetch: a bare "context deadline exceeded" is otherwise indistinguishable from the
// per-request client timeout and names no remedy. The CALLER's context is checked
// first, so a shutdown keeps its own error and stays classifiable as one.
func walkBudgetError(parent, walk context.Context, err error, page, fetched int) error {
	if parent.Err() != nil || !errors.Is(walk.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("seadex: catalogue walk exceeded its %s budget on page %d after %d entries "+
		"(upstream stalling, or the catalogue outgrew the budget - raise maxFetchDuration); "+
		"refusing to compare against a truncated view: %w", maxFetchDuration, page, fetched, err)
}

// finishFetch validates a completed catalogue before returning it: zero collected
// entries is an error (SeaDex is never legitimately empty for this app's use), so is a
// catalogue no response ever reported a totalItems for, so is a collected count below
// HALF the reported total; a smaller disagreement logs the alert-stable count-mismatch
// WARN and still returns the entries. The catalogue's TRACKER-LINK quality is
// deliberately NOT diagnosed here: that judgment needs the publish policy, a layer above
// this wire client, so internal/scout owns it. warnCatalogueShrink stands apart because
// no upstream number vouches for it.
func (c *Client) finishFetch(all []seadex.Entry, tot fetchTotals, mode FetchMode) ([]seadex.Entry, error) {
	if err := validateFinishedFetch(len(all), tot, mode); err != nil {
		return nil, err
	}
	if mode == FetchFull {
		// Both of these compare the result against a CATALOGUE-scale expectation, and a
		// window is a legitimately varying subset of that - so running either would emit
		// a shrink diagnostic every tick and poison the next full walk's comparison.
		c.logFinishedFetchWarnings(len(all), tot)
		c.warnCatalogueShrink(len(all))
	} else {
		c.warnWindowShortfall(len(all), tot)
	}
	c.log.Debug("seadex entries fetched", "entries", len(all),
		"bytes", tot.bytes, "elements", tot.elements)
	return all, nil
}

// warnWindowShortfall reports a ONE-CHUNK window that delivered fewer entries than the
// same response claimed to be selecting - the freshness half of the product going
// silently missing, since the tick would otherwise log a count indistinguishable from a
// complete pass. It is its OWN message rather than a reuse of the catalogue-count
// mismatch, which is a CATALOGUE-scale comparison a window must not run. Gated on ONE
// chunk, the sound case: delivered items and the totalItems counting them arrive in the
// same response. A WARN rather than a refusal, because a short window cannot falsely
// resolve anything and refusing would discard real freshness.
func (c *Client) warnWindowShortfall(count int, tot fetchTotals) {
	// A non-positive reported total needs no arm of its own: count is a slice length, so
	// count >= tot.reportedTotal already returns for it, the empty window included.
	// reportedTotal is never negative here (fetchAndAppend raises it from zero with max).
	if tot.chunks != 1 || count >= tot.reportedTotal {
		return
	}
	c.log.Warn("seadex change window delivered fewer entries than it reported selecting; "+
		"this tick's freshness is incomplete and the next reconcile is the backstop",
		"got", count, "want", tot.reportedTotal)
}

// reportedTotalFitsPages is the one catalogue-metadata guard BOTH fetch modes keep:
// totalItems cannot exceed what the reported pages can hold, whatever the filter,
// because it catches the upstream contradicting ITSELF. refusal names what this fetch
// is declining to do, so both modes keep their own wording over one predicate.
func reportedTotalFitsPages(tot fetchTotals, refusal string) error {
	if tot.reportedTotal <= tot.reportedPages*perPage {
		return nil
	}
	return fmt.Errorf("seadex: reported totalItems %d cannot fit the reported %d pages of %d (upstream misbehaving); %s",
		tot.reportedTotal, tot.reportedPages, perPage, refusal)
}

// validateFinishedFetch holds finishFetch's completeness guards, in order: an empty
// catalogue, a walk no reported total vouches for, a reported total that cannot fit the
// reported pages, and a below-half shortfall. Every one refuses the catalogue outright.
func validateFinishedFetch(count int, tot fetchTotals, mode FetchMode) error {
	if mode == FetchWindow {
		// A window legitimately holds nothing and its reported total counts MATCHING
		// records, so neither the empty-catalogue nor the below-half arm describes
		// anything real here. The metadata-consistency arm still applies.
		if err := reportedTotalFitsPages(tot, "refusing a window it cannot vouch for"); err != nil {
			return err
		}
		return nil
	}
	if count == 0 {
		return fmt.Errorf("seadex: returned an empty catalogue (totalItems=%d); "+
			"SeaDex is never legitimately empty, refusing to compare against it", tot.reportedTotal)
	}
	if tot.reportedTotal <= 0 {
		return fmt.Errorf("seadex: catalogue of %d entries completed with no reported total to vouch for "+
			"completeness (upstream misbehaving); refusing to compare against a truncated view", count)
	}
	if err := reportedTotalFitsPages(tot, "refusing to compare against a truncated view"); err != nil {
		return err
	}
	if degradation.Shrunk(count, tot.reportedTotal) {
		// The keyset cursor makes a SKIPPED record structurally impossible, so a shortfall
		// can only be a mid-fetch delete or a walk the upstream ended early. Losing more
		// than HALF is not credible, and erroring PRESERVES existing findings.
		return fmt.Errorf("seadex: collected %d of %d reported entries (below half); "+
			"refusing to compare against a truncated view", count, tot.reportedTotal)
	}
	return nil
}

// logFinishedFetchWarnings emits finishFetch's degradation diagnostics for a
// catalogue that PASSED validateFinishedFetch: the alert-stable count mismatch
// and the budget-mostly-spent capacity warning.
func (c *Client) logFinishedFetchWarnings(count int, tot fetchTotals) {
	// No reportedTotal > 0 conjunct: this runs only after validateFinishedFetch, which
	// already fails a FULL fetch whose reported total is not positive, so the mismatch
	// WARN can never fire with want=0.
	if count != tot.reportedTotal {
		c.log.Warn("seadex catalogue count mismatch", "got", count, "want", tot.reportedTotal)
	}
	if tot.bytes*budgetWarnDenominator >= maxTotalBytes*budgetWarnNumerator ||
		tot.elements*budgetWarnDenominator >= maxTotalElements*budgetWarnNumerator {
		c.log.Warn("seadex fetch budget mostly spent; raise the caps before the catalogue outgrows them",
			"bytes", tot.bytes, "max_bytes", maxTotalBytes,
			"elements", tot.elements, "max_elements", maxTotalElements)
	}
}

// warnCatalogueShrink warns when an ACCEPTED catalogue is a suspicious truncation of
// the previous one THIS PROCESS accepted, then adopts it as the new baseline.
//
// Every other completeness check here is SELF-ATTESTED: they compare the collected count
// against the totalItems the SAME responses reported, so an upstream that serves 200
// entries and reports 200 satisfies all of them. It adopts the new count whether or not
// it warned, so a legitimate shrink warns once and settles; persisting the baseline and
// REFUSING on a streak would newly degrade cycles that succeed today.
func (c *Client) warnCatalogueShrink(count int) {
	c.mu.Lock()
	prev := c.lastAccepted
	c.lastAccepted = count
	c.mu.Unlock()
	if prev > 0 && degradation.Shrunk(count, prev) {
		c.log.Warn("seadex catalogue shrank against this process's previous fetch; upstream may be serving a truncated catalogue",
			"got", count, "previous", prev)
	}
}

// fetchAndAppend fetches one chunk at the walk's cursor, appends its entries, updates
// the running totals, enforces the cumulative caps, validates the chunk's entry
// identities, advances the cursor, and reports whether pagination is complete. All caps
// run BEFORE allocation scales with the hostile input: the byte budget caps the wire
// read itself and the element budget caps the decode.
func (c *Client) fetchAndAppend(ctx context.Context, page int, all []seadex.Entry, tot *fetchTotals, cur *cursor, opts Options) (out []seadex.Entry, done bool, err error) {
	pageBytes, pageElems, err := remainingFetchBudgets(*tot)
	if err != nil {
		return all, false, pageFetchError(err, page, len(all))
	}
	list, n, elems, err := c.fetchPage(ctx, *cur, pageBytes, pageElems, opts)
	if err != nil {
		return all, false, pageFetchError(err, page, len(all))
	}
	tot.bytes += n
	tot.elements += elems
	tot.chunks++
	tot.reportedTotal = max(tot.reportedTotal, list.TotalItems)
	tot.reportedPages = max(tot.reportedPages, list.TotalPages)
	if verr := validatePageIdentities(list.Items, page, tot); verr != nil {
		return all, false, verr
	}
	// The chunk's keyset sequence is validated BEFORE it is accepted, not only when
	// another request will be issued: the short-chunk exhaustion decision below rests on
	// the response really being the sorted suffix after the cursor.
	var next cursor
	if len(list.Items) > 0 {
		var cerr error
		next, cerr = advanceCursor(list.Items, *cur)
		if cerr != nil {
			return all, false, cerr
		}
	}
	all = appendPageEntries(all, list.Items)
	done, err = chunkComplete(page, len(list.Items), len(all), tot.reportedTotal)
	if err != nil {
		return all, false, err
	}
	if !done {
		// The walk continues, so the next chunk is requested strictly after
		// this one's last record; an unusable cursor already failed the fetch
		// above rather than looping or skipping (advanceCursor).
		*cur = next
	}
	return all, done, nil
}

// remainingFetchBudgets derives the next chunk's per-request byte and element
// allowances from what the cumulative budgets have left. An exhausted budget is
// reported as its own sentinel and the caller adds the page context.
func remainingFetchBudgets(tot fetchTotals) (pageBytes int64, pageElements int, err error) {
	bytesLeft := int64(maxTotalBytes - tot.bytes)
	if bytesLeft <= 0 {
		return 0, 0, errCumulativeBytes
	}
	elemsLeft := maxTotalElements - tot.elements
	if elemsLeft <= 0 {
		return 0, 0, errCumulativeElements
	}
	return min(int64(maxPageBytes), bytesLeft), min(maxPageElements, elemsLeft), nil
}

// pageFetchError classifies one chunk's failure, whether raised BEFORE the request (an
// exhausted cumulative budget) or by it: a budget exhaustion keeps its sentinel and
// gains the page context, anything else becomes the ordinary per-page fetch error. It
// is the ONE home of that context, so the two paths cannot render the sentinel
// messages differently.
func pageFetchError(err error, page, fetched int) error {
	if errors.Is(err, errCumulativeBytes) || errors.Is(err, errCumulativeElements) {
		return fmt.Errorf("%w (page %d, %d entries fetched)", err, page, fetched)
	}
	return fmt.Errorf("seadex: fetch page %d: %w", page, err)
}

// validatePageIdentities enforces the catalogue's primary-key invariant across the
// whole walk: every entry is keyed by ONE positive, unique AniList ID. The budgets
// prove a chunk is well-shaped and the arithmetic proves the counts add up, but neither
// notices key loss - an omitted alID decodes as 0 and a repeated alID can stand in for
// a dropped record. Failing the whole fetch is the fail-safe direction.
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

// appendPageEntries converts one page's decoded records into public entries. The
// tracker-link counters moved to internal/scout with the diagnostic itself, which is
// what lets this client stay a pure releases.moe wire+contract leaf.
func appendPageEntries(all []seadex.Entry, items []pbEntry) []seadex.Entry {
	for i := range items {
		all = append(all, items[i].toEntry())
	}
	return all
}

// chunkComplete reports whether the keyset walk is done after a chunk: a chunk short of
// perPage is the last one (the filter asked for everything after the cursor), while a
// FULL chunk always continues. Under keyset pagination completeness is a property of
// the chunk itself, not of the response's page metadata.
//
// One arm stays an error: an EMPTY chunk after a full one while the collected entries
// are still below the reported totalItems, or with no reported total at all. The API
// itself says entries remain, so completing would hand downstream a truncated view;
// failing degrades the cycle, which preserves existing findings.
func chunkComplete(page, itemCount, fetched, reportedTotal int) (done bool, err error) {
	if itemCount >= perPage {
		return false, nil
	}
	if itemCount == 0 && page > 1 {
		// An empty follow-up chunk is only a legitimate terminal state when the API's own
		// reported total vouches for it. With none there is nothing to check the walk
		// against, so completing would hand downstream a possibly-truncated catalogue.
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

// fetchPage fetches and decodes a single chunk of entries at the walk's cursor, also
// returning the raw body size and the decoded array-element count so the caller can
// bound both across chunks. Every request asks for page 1 of the sorted remainder: the
// cursor's filter is what advances the walk, so no numbered offset can go stale under a
// concurrent delete. wireLimit and elemLimit are THIS chunk's caps, already reduced by
// the caller to the remaining cumulative budgets - so tripping a reduced limit is the
// cumulative cap while tripping the full bound stays a per-page violation.
func (c *Client) fetchPage(ctx context.Context, cur cursor, wireLimit int64, elemLimit int, opts Options) (list pbList, bodyBytes, elems int, err error) {
	q := url.Values{
		"expand":  {"trs"},
		"page":    {"1"},
		"perPage": {strconv.Itoa(perPage)},
		// Sort on immutable fields: created,id is stable under updates (an
		// entry updated mid-walk cannot move across chunks) and is the key the
		// cursor filter below pages on.
		"sort": {"created,id"},
	}
	// The window is one extra conjunct on the filter the keyset cursor already builds, so
	// a windowed walk and a full walk are the same request, paging, budgets and decode -
	// only the completeness policy differs.
	q.Set("filter", joinFilters(cur, opts))
	reqURL := c.baseURL + entriesPath + "?" + q.Encode()

	body, err := httpx.GetBytes(ctx, c.http, reqURL,
		httpx.WithMaxAttempts(maxAttempts),
		httpx.WithBaseDelay(baseDelay),
		httpx.WithMaxBodyBytes(wireLimit),
		httpx.WithHeaders(setHeaders),
		httpx.WithLogger(c.log),
		// Demote httpx's terminal "http retries exhausted" line to Debug: a page whose
		// retries ran out aborts the WHOLE walk, and the caller republishes that failure
		// with the streak, so leaving both at Warn reports one outage twice.
		httpx.WithExhaustedLevel(slog.LevelDebug),
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
		// The decoder's error can embed RAW upstream bytes: stdlib *json.UnmarshalTypeError
		// renders a rejected NUMBER literal verbatim, so a page whose totalItems is a
		// megabyte of digits yields a megabyte-long error. Bounded HERE for both paths.
		return pbList{}, 0, 0, fmt.Errorf("decode page: %s",
			runesafe.SanitizeSingleLineBounded(err.Error(), maxLoggedDecodeBytes))
	}
	return list, len(body), elems, nil
}

// ---- Bounded token-level page decoder ----
//
// decodePage and the decode* functions below form a schema-aware bounded decoder for one
// pbList page, built on jsonx/bounded: the token walk enforces every cardinality cap
// BEFORE appending each element, where json.Unmarshal materializes the whole value first.

// decodePage decodes one page body under the bounded-decoder caps, rejecting trailing
// data after the top-level value (matching json.Unmarshal strictness). elemLimit is
// this page's aggregate element budget; the decoded count is returned so the caller can
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

// decodeEntry decodes one entries record field-wise into e; the Object walk gives
// json.Unmarshal's duplicate-key semantics (a null element is a no-op, and an object
// only overwrites the fields it carries).
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

// decodeExpand decodes the expand relation envelope field-wise into ex. The trs
// relation is capped at maxTorrentsPerEntry; a repeated "trs" decodes INTO the existing
// slice, matching json.Unmarshal's duplicate-key slice semantics.
func decodeExpand(d *bounded.Decoder, ex *pbExpand) error {
	return d.Object(func(k string) error {
		if strings.EqualFold(k, "trs") {
			var err error
			ex.Trs, err = bounded.Array(d, ex.Trs, maxTorrentsPerEntry, "torrents per entry",
				func(t *seadex.Torrent) error { return decodeTorrent(d, t) })
			return err
		}
		return d.Skip()
	})
}

// decodeTorrent decodes one torrent record field-wise into t (see
// decodeEntry for the duplicate-key semantics the Object walk provides).
func decodeTorrent(d *bounded.Decoder, t *seadex.Torrent) error {
	return d.Object(func(k string) error { return decodeTorrentField(d, t, k) })
}

// decodeTorrentField decodes one torrent-record field (or skips an unknown key). The
// files and tags arrays are capped per torrent; a File is flat, so per-element decoding
// cannot amplify beyond the already-capped raw bytes.
func decodeTorrentField(d *bounded.Decoder, t *seadex.Torrent, key string) error {
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
			func(f *seadex.File) error { return d.Decode(f) })
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

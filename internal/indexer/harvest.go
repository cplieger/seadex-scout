package indexer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/httpx/v4"
	"github.com/cplieger/seadex-scout/internal/titlekey"
)

// --- Politeness rate, time slice, paging, and stats ---

// harvestQueryInterval is the pacing gap between consecutive harvest queries:
// one Prowlarr Torznab query per 2s. Nyaa publishes no API guidance, so the
// authoritative politeness number is Prowlarr's own nyaasi indexer definition,
// which declares `requestDelay: 2` (the delay Prowlarr enforces tracker-side);
// the harvest matches it app-side so a rebuild's query train is polite
// regardless of Prowlarr's own throttling. The gap is global across scopes
// (conservative: per-indexer pacing would allow interleaving).
const harvestQueryInterval = 2 * time.Second

// harvestTimeBudget is the wall-clock slice one rebuild may spend harvesting
// titles. The harvest runs inside the compare cycle (before the arr walk), so
// the slice keeps a backlogged harvest from stalling the cycle's findings
// indefinitely; whatever does not fit resumes NEXT rebuild at the rotation
// cursor (see harvestTitles), so there is no per-rebuild query count to
// starve - a deep backlog just takes more cycles. At the 2s pacing this
// admits ~300 queries per rebuild, an order of magnitude beyond any realistic
// pending set (the journal is a 14-day window of new curations).
const harvestTimeBudget = 10 * time.Minute

// harvestShowPageCap bounds one show's offset pages per rebuild, so a single
// never-matching show with a deep result set cannot monopolize a rebuild's
// time slice: at most 3 pages (300 results) per show per rebuild, then the
// next show runs; the capped show pages deeper across subsequent rebuilds.
const harvestShowPageCap = 3

// harvestPageSize is the per-query result window requested from Prowlarr and
// the paging stride: a page returning fewer results than this ends the show's
// offset paging (there is nothing older left to reach).
const harvestPageSize = 100

// harvestWait blocks between paced queries; a package var so the test suite
// can replace the real sleep (pacing gaps are wall-clock politeness, not
// logic under test) and the pacer tests can advance a fake clock instead.
// The default is httpx.SleepCtx: block for d or until ctx is done,
// returning ctx's error when cancelled first -- a pacing gap must never
// outlive a shutdown.
var harvestWait = httpx.SleepCtx

// harvestPacer enforces the politeness rate and the per-rebuild time slice:
// next gates every query, blocking for the pacing gap first (except before
// the rebuild's first query) and reporting false - permanently, for this
// rebuild - once the slice is spent or ctx is done.
type harvestPacer struct {
	now      func() time.Time
	deadline time.Time
	started  bool
}

// next reports whether another query may run, after enforcing the pacing gap.
// The deadline is re-checked after the gap so a wait that crosses it cannot
// admit one final over-budget query.
func (p *harvestPacer) next(ctx context.Context) bool {
	if p.spent(ctx) {
		return false
	}
	if p.started {
		if harvestWait(ctx, harvestQueryInterval) != nil {
			return false
		}
		if p.spent(ctx) {
			return false
		}
	}
	p.started = true
	return true
}

// spent reports whether the rebuild's harvest slice is over (deadline passed
// or ctx done) without consuming a pacing slot.
func (p *harvestPacer) spent(ctx context.Context) bool {
	return ctx.Err() != nil || p.now().After(p.deadline)
}

// harvestStats summarizes one rebuild's title harvest for the snapshot log
// line: queries spent, titles matched into the cache, and journal items still
// on a synthesized title afterwards (out of time, unmatched, no query source,
// or no upstream for their tracker).
type harvestStats struct {
	queries  int
	matched  int
	rejected int
	pending  int
}

// harvestGroup is one show's pending harvest work on one tracker: the journal
// keys still lacking a cached real title, queried with a single Torznab search
// (plus offset pages) built from the show's synthesis title source.
type harvestGroup struct {
	scope string
	keys  []string
	alID  int
}

// --- Harvest orchestration ---

// harvester owns the Prowlarr title harvest: the upstreams it queries plus the
// logger and clock its pacing and diagnostics need. It is the network half of
// the rebuild, held by FeedWriter (whose own job is the offline snapshot
// transform) as a single collaborator, so the persisted snapshot's shape and
// the Prowlarr query/pacing/matching policy have separate homes. An empty
// upstreams slice still constructs; harvestTitles' own early return is the off
// switch.
type harvester struct {
	log       *slog.Logger
	now       func() time.Time
	upstreams []*upstream
}

// newHarvester returns a harvester querying ups, using log for its
// diagnostics and now for its pacing clock.
func newHarvester(log *slog.Logger, now func() time.Time, ups []*upstream) *harvester {
	return &harvester{log: log, now: now, upstreams: ups}
}

// harvestTitles fetches real release titles for journal items still serving a
// synthesized title: ONE Prowlarr Torznab query per show and tracker (q = the
// show's synthesis title source), matching the returned items back to curated
// torrents by tracker id / info hash - the same identity extraction the search
// curation match uses - and caching each match in titles (torrents are
// immutable, so a title is harvested once, ever). AnimeBytes search is
// series-level (one query returns the show's whole torrent set, validated
// live); Nyaa uses the season form and pages by offset under the indexer's
// default created/desc ordering (see harvestParams), at most
// harvestShowPageCap pages per show per rebuild. Queries are paced at
// harvestQueryInterval inside a harvestTimeBudget slice (see the constants);
// work that does not fit resumes next rebuild: the groups are visited in
// their deterministic order ROTATED to start after the persisted checkpoint's
// last group - the last group that consumed a query last rebuild - and the
// returned cursor carries that fairness forward, so a never-matching deep
// show can only delay its successors within one rebuild, never starve them
// across rebuilds. The persisted cursor is a harvestCheckpoint (see
// decodeHarvestCheckpoint): beside the rotation position it records each
// group's next offset page, so a show cut off by harvestShowPageCap resumes
// paging DEEPER on its next visit (offset 300+ is reachable across rebuilds)
// instead of restarting at offset zero forever. Failures warn and never fail the rebuild; a show
// with no known title, no configured upstream, or no remaining slice stays
// synthetic and retries next cycle. A SCOPE-WIDE query failure
// (status/transport - see harvestShow) skips the scope's remaining shows
// this rebuild, while a show-local malformed response only skips that show,
// so one poison result set cannot freeze an otherwise healthy tracker's
// whole harvest on synthesized titles; a run of consecutiveMalformedLatch
// malformed shows — or of consecutiveRejectedLatch request-scoped rejections
// — on one scope latches it scope-wide anyway, since systematic 2xx garbage
// (e.g. a proxy answering HTML to everything) or an upstream deterministically
// rejecting every query shape is upstream-wide breakage that would otherwise
// burn the whole time slice with zero progress.
func (h *harvester) harvestTitles(ctx context.Context, feeds map[string][]journalItem, titles map[string]string, infoFor EntryInfoFunc, prevCursor string) (stats harvestStats, cursor string) {
	cp := decodeHarvestCheckpoint(prevCursor)
	defer func() { stats.pending = syntheticCount(feeds, titles) }()
	groups, index, showTitles := pendingHarvest(feeds, titles, infoFor)
	pruneHarvestPages(cp.Pages, groups)
	defer func() { cursor = encodeHarvestCheckpoint(cp) }()
	if len(groups) == 0 || len(h.upstreams) == 0 {
		// The value read here is discarded: the deferred encode above still runs
		// on this path and replaces it with the PRUNED checkpoint, which is what
		// keeps a no-work rebuild from carrying a stale deep page forward (a key
		// that stops pending must resume at page zero). Do not move the encode
		// onto the working path.
		return stats, cursor
	}
	// The pacer's deadline only gates ADMISSION of the next query; an
	// admitted u.search runs the whole Prowlarr retry tree (three 60s
	// attempts plus backoff or Retry-After waits) under the caller's
	// context, so a query admitted just before the deadline could hold the
	// compare cycle minutes past the promised slice. Derive the same
	// wall-clock budget as a context deadline so the slice also cancels
	// in-flight HTTP attempts and retry sleeps; the per-attempt client
	// timeout stays the inner bound. Budget expiry is normal exhaustion,
	// not a failure (harvestShow never warns on a done context, and the
	// loop ends via pacer.spent before any latched scope state matters);
	// outer-ctx cancellation still means shutdown and ends the loop the
	// same way.
	harvestCtx, cancelHarvest := context.WithTimeout(ctx, harvestTimeBudget)
	defer cancelHarvest()
	run := &harvestRun{
		infoFor:    infoFor,
		checkpoint: &cp,
		pacer:      &harvestPacer{now: h.now, deadline: h.now().Add(harvestTimeBudget)},
		stats:      &stats,
		index:      index,
		titles:     titles,
		showTitles: showTitles,
		latches:    newHarvestLatches(len(h.upstreams)),
	}
	start := rotationStart(groups, cp.Last)
	for i := range groups {
		if !h.processHarvestGroup(harvestCtx, groups[(start+i)%len(groups)], run) {
			break
		}
	}
	return stats, cursor
}

// harvestRun is one harvestTitles run's mutable accounting: the per-rebuild
// checkpoint, time slice, stats, the identity index, title cache and per-key
// show titles the matcher writes through and ranks with, and the per-scope latch
// state. It exists so the
// orchestration loop passes ONE value to the per-group step instead of nine,
// keeping harvestTitles about setup and ordered iteration.
type harvestRun struct {
	infoFor    EntryInfoFunc
	checkpoint *harvestCheckpoint
	pacer      *harvestPacer
	stats      *harvestStats
	index      map[string]string
	titles     map[string]string
	showTitles map[string]string
	latches    *harvestLatches
}

// harvestLatches tracks, per tracker scope, whether the scope is condemned for
// this rebuild plus the three consecutive-failure runs that condemn it. The
// cross-reset rules live in the single method that mutates it
// (updateHarvestScopeState), so an added outcome kind cannot miss one.
type harvestLatches struct {
	failed    map[string]bool
	malformed map[string]int
	rejected  map[string]int
	fruitless map[string]int
}

// newHarvestLatches returns empty latches sized for n scopes.
func newHarvestLatches(n int) *harvestLatches {
	return &harvestLatches{
		failed:    make(map[string]bool, n),
		malformed: make(map[string]int, n),
		rejected:  make(map[string]int, n),
		fruitless: make(map[string]int, n),
	}
}

// blocked reports whether scope is condemned for this rebuild, so no further
// show on it is queried.
func (l *harvestLatches) blocked(scope string) bool { return l.failed[scope] }

// processHarvestGroup runs one show-on-one-tracker group of the rotation:
// admission against the time slice, the already-satisfied skip, upstream
// selection, the query itself, and the resulting checkpoint / cursor /
// scope-latch updates. It reports whether the rotation should continue; false
// means the slice (or the caller's context) is spent and harvestTitles stops.
func (h *harvester) processHarvestGroup(ctx context.Context, g harvestGroup, r *harvestRun) bool {
	key := harvestCursorKey(g)
	if r.pacer.spent(ctx) {
		return false
	}
	if !groupPending(g, r.titles) {
		// An earlier page already titled this group's items
		// opportunistically (matchHarvest matches the global index);
		// spend no query on a satisfied group - and drop its resumed
		// page state: a satisfied group has nothing left to page for.
		delete(r.checkpoint.Pages, key)
		h.log.Debug("indexer title harvest group already satisfied; skipping query",
			"upstream", g.scope, "al_id", g.alID, "items", len(g.keys))
		return true
	}
	u := availableHarvestUpstream(h.upstreams, r.latches, g.scope)
	if u == nil {
		return true
	}
	before, beforeMatched := r.stats.queries, r.stats.matched
	outcome, nextPage, refused := h.harvestShow(ctx, u, g, r.infoFor(g.alID), r, r.checkpoint.Pages[key])
	// A show whose every candidate result was refused for contradictory
	// identity signals answered cleanly but resolved nothing: it is a
	// no-progress show for the fruitless backstop, even though its query
	// succeeded. A page that simply matched nothing is NOT contradicted, so
	// the ordinary miss still resets the run - and neither is a rejection of a
	// result that never named one of this show's pending releases: a tracker
	// answering the same broad series-level corpus to every query (AnimeBytes
	// does) would otherwise let ONE unrelated malformed item in that corpus
	// condemn the whole scope after consecutiveFruitlessLatch clean shows
	// (d-gpt-u8-1).
	contradicted := r.stats.matched == beforeMatched && refused
	if r.stats.matched > beforeMatched && (outcome == harvestShowMalformed || outcome == harvestShowFailed) {
		// The show harvested real titles before a LATER page failed
		// show-locally (a deep offset the upstream garbles or rejects). All
		// three latches are documented against one premise - the scope is
		// burning the slice with ZERO progress - so a show that produced
		// titles must not charge a consecutive-failure run: otherwise an
		// upstream whose deep pages fail while its first pages work latches
		// the scope after consecutiveRejectedLatch shows and cuts the
		// rebuild's remaining rotation off, and the fruitless WARN claims "no
		// show made progress" while titles were cached. The show's own
		// failure WARN already named the page, so nothing is hidden; only the
		// scope-wide inference is withheld. A scope-WIDE failure still
		// latches whatever progress preceded it - that upstream is down.
		outcome = harvestOK
	}
	if nextPage > 0 {
		// The show ended this rebuild with deeper pages still unseen
		// (page cap, slice expiry, or a failed page worth retrying):
		// persist where to resume so later rebuilds reach offsets the
		// per-rebuild cap alone never could.
		r.checkpoint.Pages[key] = nextPage
	} else {
		delete(r.checkpoint.Pages, key)
	}
	if r.stats.queries > before {
		// The cursor tracks the last group that CONSUMED a query - not
		// merely one dispatched after the slice ran out - so the next
		// rebuild resumes exactly where real work stopped.
		r.checkpoint.Last = key
	}
	h.updateHarvestScopeState(g.scope, outcome, contradicted, r.latches)
	return true
}

// harvestCheckpoint is the harvest's persisted resumption state, encoded into
// the snapshot's harvest_cursor string. Last is the rotation cursor (the
// "scope:alID" of the last group that consumed a query); Pages records, per
// group key, the next offset page that group's harvest should resume at, so
// a show whose curated torrent sits beyond one rebuild's harvestShowPageCap
// pages deeper across rebuilds instead of re-querying offsets 0..cap forever.
// The encoding is backward compatible both ways: a pages-less checkpoint
// encodes as the bare legacy "scope:alID" cursor an older binary reads, and
// decodeHarvestCheckpoint reads that legacy form back as Last-only.
type harvestCheckpoint struct {
	Pages map[string]int `json:"pages,omitempty"`
	Last  string         `json:"last,omitempty"`
}

// decodeHarvestCheckpoint reads a persisted harvest_cursor string: the legacy
// bare "scope:alID" rotation cursor (any non-JSON string) becomes a Last-only
// checkpoint, a JSON object decodes fully, and malformed JSON - a hand-edited
// or corrupted snapshot - degrades to an empty checkpoint (start at the head,
// page from zero: the safe baseline). The rotation cursor is validated in both
// arms (validRotationCursor): only the "<scope>:<alID>" shape harvestCursorKey
// produces survives, so a garbage value cannot be carried forward verbatim
// forever. Non-positive persisted pages are
// dropped: page 0 is the default and needs no entry, a negative value is
// meaningless, and a value that would overflow the offset computation resets
// to zero. A cursor over maxPersistedCursorBytes (writer.go, the one home of
// the persisted-snapshot size caps, enforced first at loadPrevious) is
// external corruption and takes the same empty-checkpoint baseline before any
// decoding.
func decodeHarvestCheckpoint(raw string) harvestCheckpoint {
	if len(raw) > maxPersistedCursorBytes {
		// An over-cap cursor cannot come from this writer (a cursor names
		// live groups only, and pruneHarvestPages keeps it that way), so it
		// is external corruption: degrade to the same safe baseline
		// malformed JSON takes (start at the head, page from zero) instead
		// of decoding it and re-persisting it forever.
		return harvestCheckpoint{Pages: make(map[string]int)}
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return harvestCheckpoint{Last: validRotationCursor(raw), Pages: make(map[string]int)}
	}
	var cp harvestCheckpoint
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		return harvestCheckpoint{Pages: make(map[string]int)}
	}
	cp.Last = validRotationCursor(cp.Last)
	if cp.Pages == nil {
		cp.Pages = make(map[string]int)
	}
	for key, page := range cp.Pages {
		// Drop pages that would overflow the offset multiplication in
		// harvestShow (page*harvestPageSize), including after the up-to
		// harvestShowPageCap-1 in-rebuild increments harvestShow applies
		// before multiplying: an overflowed negative offset
		// is silently omitted by harvestPage, so the show would re-query
		// page zero forever while persisting the poisoned value - unlike an
		// in-range absurd page, which self-heals via the short-page exit.
		if page <= 0 || page > math.MaxInt/harvestPageSize-(harvestShowPageCap-1) {
			delete(cp.Pages, key)
		}
	}
	return cp
}

// encodeHarvestCheckpoint renders the checkpoint back into the persisted
// harvest_cursor string: the bare legacy cursor while no page state exists
// (so an unchanged deployment round-trips byte-identical and an older binary
// keeps reading it), the JSON object once any group has a page to resume. A
// marshal failure - unreachable for this shape - degrades to the legacy form
// rather than persisting garbage.
func encodeHarvestCheckpoint(cp harvestCheckpoint) string {
	if len(cp.Pages) == 0 {
		return cp.Last
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return cp.Last
	}
	return string(b)
}

// pruneHarvestPages drops page state for groups no longer pending harvest
// (titled, aged out of the journal, gone from the catalogue, or left with no
// synthesis title source to query with), so the
// persisted checkpoint only ever names live groups and cannot grow without
// bound across rebuilds.
func pruneHarvestPages(pages map[string]int, groups []harvestGroup) {
	if len(pages) == 0 {
		return
	}
	pending := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		pending[harvestCursorKey(g)] = struct{}{}
	}
	for key := range pages {
		if _, ok := pending[key]; !ok {
			delete(pages, key)
		}
	}
}

// harvestCursorKey renders a group's rotation-cursor identity, the
// "scope:alID" form persisted in the snapshot's harvest_cursor field.
func harvestCursorKey(g harvestGroup) string {
	return g.scope + ":" + strconv.Itoa(g.alID)
}

// validRotationCursor returns cursor unchanged when it has the rotation-key
// shape harvestCursorKey produces ("<scope>:<alID>", with a POSITIVE AniList
// id - the only ids a pending group carries), else "". The cursor is
// carried into every future snapshot verbatim - a rebuild with no pending
// group never overwrites it - so a garbage or unbounded value from a
// hand-edited or corrupted snapshot would persist forever, the hazard the
// seen-ledger and title-cache caps (seenLedgerWithinLimits /
// titleCacheWithinLimits) already close for the other verbatim-carried
// fields. Dropping it changes no rotation behavior: rotationStart already
// treats an unparseable cursor as "start at the head", and a zero or negative
// id is outside harvestCursorKey's domain, so no honest cursor is rejected.
func validRotationCursor(cursor string) string {
	scope, idStr, ok := strings.Cut(cursor, ":")
	if !ok || (scope != upstreamNyaa && scope != upstreamAB) {
		return ""
	}
	alID, err := strconv.Atoi(idStr)
	if err != nil || alID <= 0 {
		return ""
	}
	return cursor
}

// rotationStart resolves where this rebuild's group iteration begins: the
// first group strictly AFTER the persisted cursor in the deterministic
// (scope, AniList ID) order, wrapping to the head past the end. An empty or
// unparseable cursor - a fresh install, a baseline, or a hand-edited
// snapshot - starts at the head; a cursor whose group is gone (titled or
// aged out) still lands on its order-successor.
func rotationStart(groups []harvestGroup, cursor string) int {
	scope, idStr, ok := strings.Cut(cursor, ":")
	if !ok {
		return 0
	}
	alID, err := strconv.Atoi(idStr)
	if err != nil {
		return 0
	}
	after := harvestGroup{scope: scope, alID: alID}
	for i := range groups {
		if compareHarvestGroups(groups[i], after) > 0 {
			return i
		}
	}
	return 0
}

// updateHarvestScopeState applies one queried show's outcome to the per-scope
// failure latch and the three consecutive-run counters: harvestScopeFailed
// latches the scope, harvestShowMalformed counts toward
// consecutiveMalformedLatch (latching the scope when the run trips it), a
// show-local request rejection (harvestShowFailed) resets the malformed run
// but counts toward its own consecutiveRejectedLatch (latching the scope on a
// run of systematic rejections), and any other outcome - a success - resets
// the two per-kind runs (and the fruitless run too, unless the show resolved
// nothing because every candidate was refused as contradictory).
//
// fruitless counts consecutive shows that produced NO progress of any kind and is
// reset ONLY by a show that actually RESOLVED something (a successful query whose
// every candidate result was refused as contradictory is progress-free and keeps
// the run charged), so it states the latches' actual purpose directly
// ("nothing is working") instead of inferring it per failure kind; why neither
// per-kind latch can see a mixed failure run is documented once, on
// consecutiveFruitlessLatch. The
// cross-resets STAY, so both existing latches keep their exact documented
// semantics and thresholds for homogeneous runs - this arm only fires on the
// mixed runs neither of them can see.
func (h *harvester) updateHarvestScopeState(scope string, outcome harvestOutcome, contradicted bool, l *harvestLatches) {
	switch outcome {
	case harvestScopeFailed:
		l.failed[scope] = true
	case harvestShowMalformed:
		l.rejected[scope] = 0
		l.malformed[scope]++
		if l.malformed[scope] >= consecutiveMalformedLatch {
			h.log.Warn("indexer title harvest: repeated malformed responses; skipping this upstream's remaining shows this rebuild",
				"upstream", scope, "consecutive", l.malformed[scope])
			l.failed[scope] = true
		}
	case harvestShowFailed:
		// A request-scoped rejection is a definitive upstream answer for
		// ONE show (reset the malformed run), but a consecutive RUN of
		// rejections is the signature of an upstream deterministically
		// rejecting this app's query shape - latch it like systematic
		// malformed bodies, or the whole budget burns with zero progress
		// on every rebuild.
		l.malformed[scope] = 0
		l.rejected[scope]++
		if l.rejected[scope] >= consecutiveRejectedLatch {
			h.log.Warn("indexer title harvest: repeated request rejections; skipping this upstream's remaining shows this rebuild",
				"upstream", scope, "consecutive", l.rejected[scope])
			l.failed[scope] = true
		}
	default:
		// The query itself succeeded, so both per-kind runs reset. The
		// fruitless run only resets when the show actually resolved
		// something: an upstream returning our releases with contradictory
		// identity signals answers cleanly forever while harvesting nothing,
		// which is the no-progress condition this backstop exists for.
		l.malformed[scope] = 0
		l.rejected[scope] = 0
		if !contradicted {
			l.fruitless[scope] = 0
			return
		}
	}
	// Reached by every failure arm, whichever kind, and by a SUCCESSFUL show that
	// resolved nothing because every candidate was refused as contradictory:
	// charge the no-progress run and latch when even a mixed sequence has produced
	// nothing. The latch is skipped when a per-kind latch already fired, so the two
	// never double-warn.
	l.fruitless[scope]++
	if !l.blocked(scope) && l.fruitless[scope] >= consecutiveFruitlessLatch {
		h.log.Warn("indexer title harvest: no show made progress; skipping this upstream's remaining shows this rebuild",
			"upstream", scope, "consecutive", l.fruitless[scope])
		l.failed[scope] = true
	}
}

// availableHarvestUpstream returns the upstream serving scope, or nil when
// the scope's upstream already failed this rebuild (keep synthesized titles,
// retry next cycle) or the tracker is not configured for searches (never
// queried).
func availableHarvestUpstream(upstreams []*upstream, l *harvestLatches, scope string) *upstream {
	if l.blocked(scope) {
		return nil
	}
	return upstreamForScope(upstreams, scope)
}

// --- Failure classification ---

// harvestOutcome classifies how one show's harvest ended, deciding what
// harvestTitles latches for the show's scope: harvestScopeFailed condemns the
// whole scope this rebuild, harvestShowMalformed counts toward the
// consecutive-malformed latch, harvestShowFailed ends only that show's
// harvest (a request-scoped Torznab rejection - the upstream answered, so it
// resets the malformed run but counts toward the consecutive-rejected
// latch), and harvestOK resets both runs.
type harvestOutcome int

const (
	harvestOK harvestOutcome = iota
	harvestScopeFailed
	harvestShowMalformed
	harvestShowFailed
)

// requestScopedHarvestError reports whether err names a failure the upstream
// scoped to THIS show's query, so the failure is show-local - terminal for
// the show (retrying the same invalid request cannot help, which is why
// terminalTorznabCode and fetchAndParse already fail it fast) but never
// evidence the upstream itself is down, so one rejection stays show-local (a
// consecutive run of them may still trip consecutiveRejectedLatch and latch
// the scope). Two shapes qualify: a Torznab <error> document naming a
// request/parameter failure (Newznab codes 200-299, read from the
// parse-time codeNum - never re-parsed from the code string, which API-key
// redaction may have rewritten), and an HTTP status that condemns only the
// request that carried it - 400 Bad Request, 414 URI Too Long, 422
// Unprocessable Entity, the statuses an upstream answers when ONE title's
// encoded query is itself unacceptable. Auth/account document codes
// (100-199) and auth/config/availability statuses (401/403/404/408/429/5xx)
// stay scope-wide: they fail every show's query identically.
func requestScopedHarvestError(err error) bool {
	if docErr, ok := errors.AsType[*upstreamDocError](err); ok {
		return docErr.codeNum >= 200 && docErr.codeNum < 300
	}
	if statusErr, ok := errors.AsType[*httpx.StatusError](err); ok {
		switch statusErr.Code {
		case http.StatusBadRequest, http.StatusRequestURITooLong, http.StatusUnprocessableEntity:
			return true
		}
	}
	return false
}

// consecutiveMalformedLatch is how many CONSECUTIVE shows on one scope may
// fail with a persistently malformed 2xx body before the scope is treated as
// upstream-wide broken (e.g. a reverse proxy answering an HTML error page to
// every request) and its remaining shows are skipped this rebuild. One poison
// result set stays show-local; a show whose harvest ends without a malformed
// page - a success (even an empty one) or a request-scoped rejection - resets
// the run. The reset is per show outcome, not per page: a show whose LATER
// offset page is malformed after a successful first page still counts toward
// the latch.
const consecutiveMalformedLatch = 3

// consecutiveRejectedLatch is how many CONSECUTIVE shows on one scope may
// fail with a request-scoped Torznab rejection (codes 200-299) before the
// scope is treated as systematically rejecting this app's query shape (e.g.
// an indexer definition without tvsearch caps answering 203 to every
// season-form query) and its remaining shows are skipped this rebuild. One
// rejected query stays show-local; a show whose harvest ends without a
// request-scoped rejection - a success (even an empty one) or a malformed
// show - resets the run.
const consecutiveRejectedLatch = 3

// consecutiveFruitlessLatch is how many CONSECUTIVE shows on one scope may fail
// in ANY mix before the scope is treated as broken and its remaining shows are
// skipped this rebuild. It is the backstop the two per-kind latches cannot be:
// each of those resets the other's counter (deliberately - see
// updateHarvestScopeState), so an upstream ALTERNATING between a garbled 2xx body
// and a request rejection trips neither of them however long it runs, and the
// full harvestTimeBudget burns with zero title progress every rebuild (l-f91).
//
// Twice the per-kind threshold: a homogeneous run always latches at 3 through its
// own counter, so this fires only on a genuinely mixed sequence - where the
// evidence for "this upstream is broken" accumulates more slowly and deserves
// more patience - and it can never preempt the more specific diagnostics.
const consecutiveFruitlessLatch = 2 * consecutiveMalformedLatch

// harvestShow runs one show's query (plus offset pages while its items remain
// unmatched and full pages keep coming, up to harvestShowPageCap pages this
// rebuild) against its tracker's upstream, starting at startPage - the
// checkpointed page a previous rebuild stopped at - so paging resumes deeper
// across rebuilds. The second return is the page the NEXT rebuild should
// resume at: 0 when the show's paging is complete (its items matched, or a
// short page proved nothing older is left), otherwise the first page this
// rebuild did not successfully consume (the cap/slice cutoff, or the failed
// page itself so it is retried). Every page passes through the pacer
// (politeness gap + time slice); a show cut off by the cap or the slice
// simply resumes on a later rebuild via the checkpoint. A query failure
// warns and ends the
// show's harvest for this rebuild (the next rebuild retries). Failures are
// classified before condemning the whole scope: a SCOPE-WIDE failure
// (429/5xx, an auth/config status, a transport error - the upstream is likely
// down or refusing service) reports harvestScopeFailed so the caller skips the
// scope's remaining groups this rebuild, while a persistently malformed
// SUCCESSFUL body (malformedUpstreamBody) is specific to this one show's
// result set and reports harvestShowMalformed, so the scope's other shows are
// still harvested within the remaining slice instead of one poison response
// freezing the whole tracker on synthesized titles indefinitely - unless a
// RUN of malformed shows trips the caller's consecutiveMalformedLatch, the
// signature of an upstream answering 2xx garbage to everything. A Torznab
// <error> document naming a request/parameter code (200-299) - or an HTTP
// status condemning only this request (400/414/422) - is likewise
// show-local (requestScopedHarvestError -> harvestShowFailed): the upstream
// deliberately rejected this one show's query, so its siblings' valid queries
// still run — unless a run of rejections trips the caller's
// consecutiveRejectedLatch.
//
// refused reports whether any page REFUSED one of this show's own pending
// releases for contradictory identity signals (matchHarvest's pendingRejected).
// A show that resolved nothing because its candidates were all refused
// harvested nothing while answering cleanly, which is the caller's no-progress
// signal; rejections of unrelated items on the same broad result page are not.
func (h *harvester) harvestShow(ctx context.Context, u *upstream, g harvestGroup, meta EntryInfo, r *harvestRun, startPage int) (outcome harvestOutcome, nextPage int, refused bool) {
	params := harvestParams(meta, g.scope)
	page := max(startPage, 0)
	for range harvestShowPageCap {
		if !r.pacer.next(ctx) {
			return harvestOK, page, refused
		}
		r.stats.queries++
		results, raw, failure, ok := h.searchHarvestPage(ctx, u, g, params, page)
		if !ok {
			return failure, page, refused
		}
		matched, rejected, pendingRejected := matchHarvest(results, g.scope, r.index, r.titles, r.showTitles, g.keys)
		r.stats.matched += matched
		refused = refused || pendingRejected > 0
		if rejected > 0 {
			// A result whose own identity signals contradict each other is an
			// untrusted upstream response, not an operator fault: it resolves
			// nothing, the item keeps its synthesized title, and the next
			// rebuild retries. WARN would fire per page of a systematically
			// tampered feed, so the count rides Debug plus the harvest_rejected
			// stat on the rebuild's summary line.
			r.stats.rejected += rejected
			h.log.Debug("indexer title harvest results rejected: contradictory identity signals",
				"upstream", u.name, "al_id", g.alID, "page", page, "rejected", rejected)
		}
		if harvestPageComplete(g, r.titles, raw) {
			return harvestOK, 0, refused
		}
		page++
	}
	return harvestOK, page, refused
}

// searchHarvestPage runs one harvest page's upstream query and classifies its
// outcome. The final bool reports whether the page's results are usable; when
// it is false the returned outcome is the one harvestShow must report for this
// show, leaving the page unconsumed so the checkpoint retries it. A done
// context (the time-budget deadline firing mid-query, or shutdown) is silent
// scope-wide exhaustion rather than an upstream fault, so it never warns; any
// other error rides classifyHarvestError's show-local vs scope-wide split.
func (h *harvester) searchHarvestPage(ctx context.Context, u *upstream, g harvestGroup, params url.Values, page int) ([]item, int, harvestOutcome, bool) {
	results, raw, err := u.search(ctx, harvestPage(params, page*harvestPageSize))
	if err == nil {
		return results, raw, harvestOK, true
	}
	if ctx.Err() != nil {
		// The harvest context is done: the time-budget deadline fired
		// mid-query (normal exhaustion, resumed next rebuild at the
		// checkpoint) or the outer context was cancelled (shutdown). Neither
		// warns, and the caller's pacer.spent check ends the rebuild's loop
		// before the latched scope state could matter; the unconsumed page is
		// preserved so the next rebuild retries it.
		return nil, 0, harvestScopeFailed, false
	}
	return nil, 0, h.classifyHarvestError(err, u, g.alID, params.Get("t"), page), false
}

// harvestPageComplete reports whether this show's paging is done after the
// page just consumed: either every journal key now has a harvested title, or
// the short page proved the upstream has nothing older to offer.
func harvestPageComplete(g harvestGroup, titles map[string]string, raw int) bool {
	return !groupPending(g, titles) || raw < harvestPageSize
}

// classifyHarvestError warns about one show's failed (non-cancelled) harvest
// query and maps it to the outcome harvestTitles latches: a persistently
// malformed SUCCESSFUL body stays show-local (harvestShowMalformed, counted
// toward the consecutive-malformed latch), a request-scoped rejection - a
// Torznab <error> document with a request/parameter code (200-299) or a
// request-specific HTTP status (400/414/422) - stays show-local and counts
// toward the consecutive-rejected latch (harvestShowFailed), and
// anything else - an auth/config/availability status or a transport failure -
// condemns the scope (harvestScopeFailed). Every arm names the failing REQUEST
// as well as the show: queryType is the query shape harvestParams chose
// (t=search vs t=tvsearch) and page the offset page, so an operator can tell a
// season-form rejection from a flat-search one and a poisoned deep page from a
// first-page failure. The encoded query and full URL stay out of the log
// deliberately (httpx's redactor drops userinfo and REDACTs every query value
// in the *StatusError that reaches here).
func (h *harvester) classifyHarvestError(err error, u *upstream, alID int, queryType string, page int) harvestOutcome {
	if malformedUpstreamBody(err) {
		h.log.Warn("indexer title harvest response malformed; show keeps its synthesized title this rebuild",
			"upstream", u.name, "al_id", alID, "query_type", queryType, "page", page, "error", err)
		return harvestShowMalformed
	}
	if requestScopedHarvestError(err) {
		h.log.Warn("indexer title harvest request rejected; show keeps its synthesized title this rebuild",
			"upstream", u.name, "al_id", alID, "query_type", queryType, "page", page, "error", err)
		return harvestShowFailed
	}
	h.log.Warn("indexer title harvest query failed; skipping this upstream's remaining shows this rebuild",
		"upstream", u.name, "al_id", alID, "query_type", queryType, "page", page, "error", err)
	return harvestScopeFailed
}

// --- Pending-group collection ---

// harvestGroupKey identifies one show's pending harvest group on one
// tracker: the per-show, per-tracker bucket pendingHarvest collects journal
// keys into before materializing the sorted harvestGroup list.
type harvestGroupKey struct {
	scope string
	alID  int
}

// indexHarvestItem records one harvestable journal item: it appends the
// item's key to its show's per-tracker group and registers the item's
// identity forms (tracker key and info hash) in the global index that maps a
// matched Prowlarr result back to the journal key whose title it supplies.
// A non-harvestable item is left out (see harvestable).
func indexHarvestItem(it *journalItem, scope string, titles map[string]string, infoFor EntryInfoFunc, byShow map[harvestGroupKey][]string, index map[string]string, ambiguous map[string]struct{}) {
	if !harvestable(it, titles, infoFor) {
		return
	}
	if scopeOfKey(it.Key) != scope {
		// A hand-edited or corrupted snapshot can hold an item whose journal
		// key names a DIFFERENT tracker than the feed it sits in. Its key can
		// never satisfy matchHarvest's scope binding, so querying for it
		// would burn up to harvestShowPageCap queries of every rebuild's
		// slice forever with no reachable outcome. It stays counted in
		// syntheticCount's pending total, which keys on the feed item alone -
		// correctly, since it will never receive a harvested title.
		return
	}
	key := harvestGroupKey{scope: scope, alID: it.AniListID}
	byShow[key] = append(byShow[key], it.Key)
	index[it.Key] = it.Key
	h := validInfoHash(it.InfoHash)
	if h == "" {
		return
	}
	if _, bad := ambiguous[h]; bad {
		// Already proven to name more than one pending item.
		return
	}
	if prev, dup := index[h]; dup && prev != it.Key {
		// Two pending journal items publish the SAME info hash (the same
		// bytes curated under two tracker ids, or on two trackers). The
		// hash names the bytes, not the item, so it can corroborate
		// neither: retire it from the index and remember that, so a third
		// occurrence cannot re-register it. resolveHarvestKey's existing
		// "unknown to a partial index" arm then treats it as inconclusive
		// rather than as a contradiction.
		delete(index, h)
		ambiguous[h] = struct{}{}
		return
	}
	index[h] = it.Key
}

// compareHarvestGroups orders harvest groups by tracker scope then AniList ID
// for deterministic query order; cmp.Compare avoids the overflow a plain int
// subtraction could hit on extreme untrusted AniList IDs.
func compareHarvestGroups(a, b harvestGroup) int {
	if c := strings.Compare(a.scope, b.scope); c != 0 {
		return c
	}
	return cmp.Compare(a.alID, b.alID)
}

// pendingHarvest collects the journal items lacking a cached title into
// per-show, per-tracker groups (sorted for deterministic query order) plus a
// global identity index (tracker key and info hash forms) mapping a matched
// Prowlarr result back to the journal key whose title it supplies, and the
// per-journal-key show title the alias policy ranks against. Items
// whose show has no synthesis title source are left out: there is nothing to
// query with, and they retry once the library or the AniList memo knows the
// show.
//
// showTitles is keyed by journal key rather than being one per query because
// the index is GLOBAL: a broad result page fetched for show A routinely
// resolves show B's items too (the opportunistic match), and B's alias choice
// must be made against B's own vocabulary, not A's.
func pendingHarvest(feeds map[string][]journalItem, titles map[string]string, infoFor EntryInfoFunc) (groups []harvestGroup, index, showTitles map[string]string) {
	byShow := make(map[harvestGroupKey][]string)
	index = make(map[string]string)
	ambiguous := make(map[string]struct{})
	for scope, feed := range feeds {
		for i := range feed {
			indexHarvestItem(&feed[i], scope, titles, infoFor, byShow, index, ambiguous)
		}
	}
	groups = make([]harvestGroup, 0, len(byShow))
	for k, keys := range byShow {
		groups = append(groups, harvestGroup{keys: keys, scope: k.scope, alID: k.alID})
	}
	slices.SortFunc(groups, compareHarvestGroups)
	showTitles = make(map[string]string, len(index))
	for _, g := range groups {
		title := strings.TrimSpace(infoFor(g.alID).Title)
		for _, key := range g.keys {
			showTitles[key] = title
		}
	}
	return groups, index, showTitles
}

// harvestable reports whether a journal item is due a harvest query: it still
// serves a synthesized title, carries its journal bookkeeping, and its show
// has a title source to query with.
func harvestable(it *journalItem, titles map[string]string, infoFor EntryInfoFunc) bool {
	if it.Key == "" || it.AniListID <= 0 {
		return false
	}
	if _, done := titles[it.Key]; done {
		return false
	}
	return strings.TrimSpace(infoFor(it.AniListID).Title) != ""
}

// --- Query building ---

// harvestParams builds the one Torznab query for a show on a tracker, from the
// show's synthesis title source. AnimeBytes search is series-level - a plain
// q returns the show's whole torrent set - so a basic search suffices. Nyaa is
// a flat search, so a REAL season (a resolved season above the specials bucket)
// uses the season form (q + season): the season token surfaces both packs
// (named "... S01 ...") and SxxExx-named episodes (S01 prefixes S01E07), which
// is what SeaDex curates; offset paging under the indexer's default
// created/desc ordering then reaches older items. The specials bucket is
// deliberately excluded - "season=0" is not a search a tracker's episode naming
// answers.
func harvestParams(meta EntryInfo, scope string) url.Values {
	q := url.Values{"t": {"search"}, "q": {strings.TrimSpace(meta.Title)}}
	if scope == upstreamNyaa && !meta.IsMovie && meta.SeasonKnown && meta.Season > 0 {
		q.Set("t", "tvsearch")
		q.Set("season", strconv.Itoa(meta.Season))
	}
	return q
}

// harvestPage clones the show query with the paging window applied.
func harvestPage(params url.Values, offset int) url.Values {
	page := maps.Clone(params)
	page.Set("limit", strconv.Itoa(harvestPageSize))
	if offset > 0 {
		page.Set("offset", strconv.Itoa(offset))
	}
	return page
}

// --- Result matching ---

// harvestMaxTitleLen bounds a cached harvested title: real tracker release
// titles are well under this, and the titles map is persisted verbatim into
// the snapshot and rendered into every RSS response, so an oversized title
// from a tampered/garbled upstream body must never enter the cache.
const harvestMaxTitleLen = 512

// matchHarvest matches one page of Prowlarr results back to pending journal
// items by the single journal key each result's identity signals agree on -
// the tracker id parsed from its page URLs (comments/guid, the same
// numeric-validated extraction the search curation match uses) and its info
// hash; contradictory signals fail closed and title nothing - caching each
// matched real title. A resolved key must belong to the queried tracker's
// scope: a result from one upstream must never title the other tracker's
// journal item (the same scope binding the search curation match applies in
// acceptScopedKeys). An already-cached key is never overwritten: torrents
// are immutable, so the first harvested title stands.
//
// showTitles maps each pending journal key to the show title the synthesis
// already trusts for THAT key's show (the arr's own title, or the AniList
// canonical one), used to pick among ALIASES of the same torrent - see
// preferredHarvestTitle. It is keyed per journal key, not per query, because a
// broad result page fetched for one show routinely resolves another pending
// show's items too, and that show's alias must be chosen against its OWN
// vocabulary. A key absent from the map keeps plain first-wins.
//
// It reports the matches AND the contradictory rejections, so a result whose
// own signals disagree is observable. That count is the only report such a
// result gets: a rejected result silently leaves its journal item on the
// synthesized heuristic title, and because the index is rebuilt from the same
// journal each rebuild, a systematic disagreement never self-heals. Results that
// simply are not ours (a season query returns the tracker's whole page) resolve
// no key at all and are NOT counted - they are the overwhelming majority and
// carry no signal.
//
// pendingRejected is the subset of those rejections that touched one of THIS
// group's pending identities (pendingHarvestRefusal, graded against groupKeys):
// the ones that actually refused one of this show's own candidate releases. Only
// that subset licenses the caller's no-progress inference - a result whose
// comments and guid disagree with each other names nothing we asked for, and
// AnimeBytes answers the same broad series-level corpus to every query, so one
// unrelated malformed item repeating across shows must not read as "this scope
// harvests nothing".
func matchHarvest(results []item, scope string, index, titles, showTitles map[string]string, groupKeys []string) (matched, rejected, pendingRejected int) {
	// Collect every candidate title per key BEFORE choosing: AnimeBytes lists
	// one torrent three times (EN / JP / Romaji aliases, distinct ?nh= GUIDs,
	// the SAME torrent id), so all three resolve to one journal key and the
	// choice among them is a policy, not an accident of Prowlarr's ordering
	// (l-f142).
	candidates := make(map[string][]string)
	order := make([]string, 0, len(results))
	for i := range results {
		title := strings.TrimSpace(results[i].Title)
		if title == "" || len(title) > harvestMaxTitleLen {
			continue
		}
		key, conflict := resolveHarvestKey(&results[i], index)
		if conflict {
			// Every contradiction is refused and counted; only one that named a
			// release this rebuild is trying to title says the show harvested
			// nothing.
			rejected++
			pendingRejected += pendingHarvestRefusal(&results[i], index, groupKeys)
			continue
		}
		if key == "" || !strings.HasPrefix(key, scope+":") {
			continue
		}
		if _, done := titles[key]; done {
			continue
		}
		if _, seen := candidates[key]; !seen {
			order = append(order, key)
		}
		candidates[key] = append(candidates[key], title)
	}
	for _, key := range order {
		titles[key] = preferredHarvestTitle(candidates[key], showTitles[key])
		matched++
	}
	return matched, rejected, pendingRejected
}

// pendingHarvestRefusal grades a REFUSED result (resolveHarvestKey reported a
// contradiction): 1 when any of its identity signals - either page-URL tracker
// key, or its info hash - names one of THIS group's pending items (groupKeys),
// 0 when it names none of them. It reports a charge rather than a bool so the
// caller can add it without a second nesting level.
//
// The grade matters because a result whose comments and guid disagree with each
// other is refused before either signal is looked up, so the refusal alone does
// not say whether one of OUR releases was refused or an unrelated item on the
// same broad result page was. Only the former is evidence that this show
// harvested nothing (matchHarvest's pendingRejected, the caller's no-progress
// signal).
//
// The group's own keys - not merely the global pending index - are what the
// grade is measured against, because the caller's inference is group-local: it
// charges the fruitless run when THIS show matched nothing. AnimeBytes answers
// the same broad series-level corpus to every query, so one contradictory item
// belonging to a DIFFERENT pending show repeats across every otherwise ordinary
// miss and would latch the scope after consecutiveFruitlessLatch clean shows
// while the fairness cursor still had time and groups to spend.
func pendingHarvestRefusal(it *item, index map[string]string, groupKeys []string) int {
	for _, id := range []string{trackerKeyFromURL(it.InfoURL), trackerKeyFromURL(it.GUID), it.InfoHash} {
		if id == "" {
			continue
		}
		if key, ok := index[id]; ok && slices.Contains(groupKeys, key) {
			return 1
		}
	}
	return 0
}

// preferredHarvestTitle picks which of a torrent's alias titles is cached and
// served for the item's whole journal window.
//
// AnimeBytes exposes each torrent under its English, Japanese and Romaji titles
// (documented in this app's own notes: the aliases are kept, not deduped,
// because extra titles help the arrs match), all three carrying the same torrent
// id and therefore the same journal key. Taking whichever Prowlarr happened to
// list first meant the served title was a coin flip - and a JP or Romaji alias
// the operator's Sonarr series does not carry makes the RSS item LESS matchable
// than the synthesized title it replaced, which synthesizeTitle deliberately
// builds from the arr's own vocabulary (l-f142).
//
// The policy is English-preferred, expressed against the evidence actually
// available: Torznab carries no language marker, so the alias whose text
// contains the show title the synthesis already trusts (the arr's own title, or
// the AniList canonical one) is the one in the arr's vocabulary. For an
// English-titled series that selects the English alias; for an operator whose
// arr carries the Romaji title it selects Romaji, which is the right answer for
// THAT library. A native-script alias loses either way, since it cannot contain
// a Latin show title.
//
// When the vocabulary cannot be detected - no show title to compare against, or
// no alias containing it - the fallback keeps the alias carrying the most
// arr-parseable text (ASCII letters and digits; first on a tie) rather than
// whichever Prowlarr happened to list first. The aliases differ only in the
// show-title portion of an otherwise identical release name, so that metric is
// monotone in how much Latin release text survives: it beats a native-script
// title, which an arr cannot parse at all, and it is deterministic where
// Prowlarr's ordering is not. Byte or rune length would NOT do - a CJK title is
// three bytes per character and would win on length while being the least
// useful alias. It always returns one of the candidates: an undetected
// vocabulary must never cost the item its harvested title and send it back to
// the synthesized one.
func preferredHarvestTitle(candidates []string, showTitle string) string {
	if len(candidates) == 1 {
		return candidates[0]
	}
	if want := titlekey.Normalize(showTitle); want != "" {
		for _, c := range candidates {
			if candidateContainsTitleKey(c, want) {
				return c
			}
		}
	}
	best, bestScore := candidates[0], asciiAlnums(candidates[0])
	for _, c := range candidates[1:] {
		if n := asciiAlnums(c); n > bestScore {
			best, bestScore = c, n
		}
	}
	return best
}

// candidateContainsTitleKey reports whether the candidate release name carries
// the show's normalized title key as its own vocabulary.
//
// titlekey.Normalize deliberately drops every separator, so a plain normalized
// substring test has no token-boundary evidence at all. That is harmless for a
// title key of real length, but a SHORT key (a one- to three-character show
// title such as "X") occurs inside ordinary release metadata: the "x" in
// "Remux" or "x265" satisfies it on every alias, so the first alias wins
// whatever its vocabulary - defeating the very policy the caller documents.
// A short key therefore requires an EXACT match against a run of the
// candidate's own alphanumeric tokens, which is the boundary evidence the
// normalized form threw away; longer keys keep the punctuation-tolerant
// normalized substring, where an accidental hit is not credible.
func candidateContainsTitleKey(candidate, want string) bool {
	if len(want) >= 4 {
		return strings.Contains(titlekey.Normalize(candidate), want)
	}
	tokens := strings.FieldsFunc(strings.ToLower(candidate), func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'z')
	})
	var joined strings.Builder
	for start := range tokens {
		joined.Reset()
		for _, token := range tokens[start:] {
			joined.WriteString(token)
			if joined.Len() >= len(want) {
				if joined.String() == want {
					return true
				}
				break
			}
		}
	}
	return false
}

// asciiAlnums counts the ASCII letters and digits in s - a proxy for how much of
// a release name an arr's title parser can actually work with.
func asciiAlnums(s string) int {
	n := 0
	for i := range len(s) {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
			n++
		}
	}
	return n
}

// resolveHarvestKey resolves a Prowlarr result to the single journal key its
// identity signals - the tracker keys parsed from its page URLs and its
// (already validated) info hash - agree on. It reports conflict=true when the
// signals CONTRADICT each other: the keys parsed from the two page URLs name
// different releases, or two signals resolve to different journal items. A
// healthy Prowlarr emits one consistent identity per item, so a contradictory
// result is an untrusted response that must not title anything (the same
// fail-closed rule the search curation match applies in acceptScopedKeys).
// conflict=false with an empty key means the result is simply not one of ours -
// the ordinary outcome for most of a season query's page.
//
// An info hash that is absent from the index is INCONCLUSIVE, not
// contradictory, once another signal has resolved. The index is deliberately
// partial - it carries only items still awaiting a title, and only the hashes
// SeaDex itself published - so a Prowlarr Nyaa result (which always reports a
// hash, torznab.go) routinely carries one the index cannot know: AB's
// "<redacted>" form is dropped by validInfoHash and any SeaDex record missing
// the field indexes no hash at all. A hash can also be absent because it was
// RETIRED as ambiguous: two pending items publishing the same hash (the same
// bytes curated under two tracker ids, or listed on both trackers) name the
// bytes rather than either item, so indexHarvestItem drops it from the index
// instead of letting one item win the slot last-write-wins. Treating that
// absence as unreconcilable evidence rejected the result outright, and because
// the index is rebuilt from
// the same journal every rebuild the rejection was PERMANENT: the item kept its
// synthesized heuristic title forever, on an identity disagreement between
// SeaDex and the tracker that nothing in the pipeline reported (d-u5-c2-2).
// Corroboration is what the hash is for, so it can agree or disagree - it
// cannot veto an identity the URLs established on their own. A hash that IS in
// the index and names a different item still conflicts, and a hash that is the
// ONLY signal and unknown still resolves nothing (there is no item to title,
// and nothing contradicted anything). Format skew is not a factor: both sides
// normalize through validInfoHash.
func resolveHarvestKey(it *item, index map[string]string) (key string, conflict bool) {
	kc, kg := trackerKeyFromURL(it.InfoURL), trackerKeyFromURL(it.GUID)
	if kc != "" && kg != "" && kc != kg {
		return "", true
	}
	for _, id := range []string{kc, kg} {
		if id == "" {
			continue
		}
		k, ok := index[id]
		if !ok {
			// A parseable tracker key the pending index does not hold names a
			// release that is not ours; kc == kg here, so nothing disagrees.
			return "", false
		}
		if key != "" && k != key {
			return "", true
		}
		key = k
	}
	if it.InfoHash == "" {
		return key, false
	}
	k, ok := index[it.InfoHash]
	if !ok {
		// Unknown to a partial index: corroborates nothing either way. When no
		// URL resolved, key is still "" and the result matches nothing.
		return key, false
	}
	if key != "" && k != key {
		return "", true
	}
	return k, false
}

// --- Pending accounting ---

// groupPending reports whether any of the group's journal keys still lacks a
// cached title (more paging could still help).
func groupPending(g harvestGroup, titles map[string]string) bool {
	for _, k := range g.keys {
		if _, ok := titles[k]; !ok {
			return true
		}
	}
	return false
}

// syntheticCount totals the journal items across all feeds still serving a
// synthesized title (no cached harvested title), for the snapshot log line -
// whatever the reason: over budget, unmatched, no query source, or no
// configured upstream for their tracker.
func syntheticCount(feeds map[string][]journalItem, titles map[string]string) int {
	n := 0
	for _, feed := range feeds {
		for i := range feed {
			if feed[i].Key == "" {
				continue
			}
			if _, ok := titles[feed[i].Key]; !ok {
				n++
			}
		}
	}
	return n
}

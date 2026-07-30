// Package notify emits findings as structured slog events with cross-cycle
// dedupe - the daemon's NOTIFICATION path (Loki alerting rides these lines).
// Observability is slog-only; there is no metrics endpoint. It is distinct
// from the user-facing report FEATURE (the `report` subcommand's season-level
// audit), which lives in internal/audit.
package notify

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/tracker"
)

// --- Notifier / state reporting ---

// Notifier reports findings as STATE rather than as events: it holds the set
// of conditions currently true and re-emits the whole set on every pass, so
// the alerting stack (Loki rule -> Alertmanager) owns notification policy.
// That is the contract that stack is built for - the Prometheus alerts API
// expects a client to keep re-sending a firing alert until it resolves - and
// it is what makes a notification lost anywhere downstream recoverable: the
// condition is still being reported, so the next rule evaluation re-fires it.
//
// The previous shape emitted each finding exactly once, ever, keyed by a
// persisted dedupe map in state.json. That map is gone: it was the mechanism
// that made a lost notification permanent. Resolution moves with it - a
// finding that stops being reported resolves when the rule's lookback window
// expires, which is why nothing here emits a "resolved" line any more.
//
// SINGLE WRITER: every caller runs inside the cycle body, and one process
// holds one Notifier, so the set is only ever touched by the goroutine driving
// the cycle. The /config/cycle.lock file serializes PROCESSES against each
// other (state.json, feed.json), not accesses to this map - it is the
// single-goroutine ownership that makes the lack of a mutex correct.
type Notifier struct {
	log *slog.Logger
	// ignore is the operator's filters.ignore set (AniList IDs). It suppresses
	// EMISSION only: an ignored finding still enters current, still counts in
	// the summary line, and still appears in report mode. Continuous reporting
	// means Alertmanager re-notifies for as long as a condition holds, so a
	// show the operator has consciously decided not to upgrade would otherwise
	// be re-notified indefinitely.
	ignore map[int]struct{}
	// current is the set of conditions true as of the last completed pass,
	// keyed by dedupe key. It holds whole compare.Findings rather than the
	// trimmed projection the old dedupe record persisted, because every field
	// the emitted line carries has to be re-emittable on the next pass.
	//
	// Its untrusted strings are still BOUNDED at ingest (boundRetained). The
	// old projection's bound existed because the record was written to
	// state.json; deleting the persistence does not delete the reason. These
	// rows are RESIDENT for as long as the condition holds - across every pass,
	// for the process lifetime - so a hostile catalogue's oversized titles,
	// group names and link URLs would otherwise sit in a 256 MiB container
	// bounded only by the fetch's own budget, invisible because the emit path
	// caps per attribute on the way out and never shrinks what it read from
	// (CWE-400).
	current map[string]compare.Finding
}

// NewNotifier builds a Notifier. logger may be nil. ignore is the operator's
// filters.ignore set and may be nil.
func NewNotifier(logger *slog.Logger, ignore map[int]struct{}) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{log: logger, ignore: ignore, current: map[string]compare.Finding{}}
}

// Report replaces the current finding set with findings and emits the whole
// set. It is the only way findings reach the log.
//
// incompleteIDs scopes what replacement may DELETE. An AniList ID in that set
// belongs to an item whose evidence was incomplete this pass (the caller
// passes the union of episode-fetch failures and AniList-degraded entries), so
// its absence from findings is missing data, not evidence of alignment: its
// prior rows are carried forward instead of dropped. Pass nil when every item
// has complete evidence.
//
// Deleting the rows of an item that was NOT flagged incomplete is correct and
// is how a resolved condition stops being reported. A row for an item that has
// left the library entirely is dropped the same way, because a complete pass
// simply does not produce it.
func (n *Notifier) Report(findings []compare.Finding, incompleteIDs map[int]struct{}) {
	next := make(map[string]compare.Finding, len(findings))
	// Last-payload-wins per key. In-batch duplicate keys are reachable (two
	// findings can fold onto one key), and the emitted line must carry the
	// same payload the set retains, so the map write order decides both.
	for i := range findings {
		key := dedupeKey(&findings[i])
		retained := findings[i]
		boundRetained(&retained)
		next[key] = retained
	}
	preserved := 0
	for key := range n.current {
		if _, present := next[key]; present {
			continue
		}
		f := n.current[key]
		if _, incomplete := incompleteIDs[f.AniListID]; !incomplete {
			continue
		}
		next[key] = f
		preserved++
	}
	n.current = next
	n.emitAll(preserved)
}

// boundRetained caps the untrusted strings of a row about to be RETAINED, in
// place. Every value here is parsed from SeaDex data or library file names, and
// a retained row outlives the pass that produced it, so the cap has to happen
// on the way IN - the emit path's own caps bound only what is written to the
// log and leave the resident value whole. capAttr is idempotent, so a row
// carried forward across passes is bounded once and passes through unchanged.
//
// Links is the field that most needs it: the old persisted projection carried
// no URL at all, and one entry can publish many, each an untrusted string.
// The AB grade and Headline flag are typed values and need no bound.
//
// It CLONES the three slices before bounding them. f is a shallow copy of the
// caller's finding, so the slice headers still point at the caller's backing
// arrays: bounding in place would mutate the compare result the audit report
// and the cycle log line also read, turning a retention bound into a silent
// edit of somebody else's data.
func boundRetained(f *compare.Finding) {
	f.RecommendedGroups = slices.Clone(f.RecommendedGroups)
	f.CurrentGroups = slices.Clone(f.CurrentGroups)
	f.Links = slices.Clone(f.Links)
	f.Arr = capAttr(f.Arr)
	f.Title = capAttr(f.Title)
	f.Kind = capAttr(f.Kind)
	f.Reason = capAttr(f.Reason)
	f.Tracker = capAttr(f.Tracker)
	f.Resolution = capAttr(f.Resolution)
	f.Codec = capAttr(f.Codec)
	f.Scope = capAttr(f.Scope)
	f.InfoHash = capAttr(f.InfoHash)
	f.CurrentGroup = capAttr(f.CurrentGroup)
	f.RecommendedGroup = capAttr(f.RecommendedGroup)
	f.Status = compare.Status(capAttr(string(f.Status)))
	// capAttr, NOT capURLAttr: the retention bound is a SIZE bound, while
	// capURLAttr is the emit path's link-destination ENCODER (it percent-encodes
	// for a Markdown sink). Encoding here would hand the emit path an
	// already-encoded value and double-encode every space into %2520.
	f.ReleaseURL = capAttr(f.ReleaseURL)
	f.ArrURL = capAttr(f.ArrURL)
	for i := range f.RecommendedGroups {
		f.RecommendedGroups[i] = capAttr(f.RecommendedGroups[i])
	}
	for i := range f.CurrentGroups {
		f.CurrentGroups[i] = capAttr(f.CurrentGroups[i])
	}
	for i := range f.Links {
		f.Links[i].Tracker = capAttr(f.Links[i].Tracker)
		f.Links[i].URL = capAttr(f.Links[i].URL)
	}
}

// emitAll logs every row of the current set, in a deterministic order so a
// pass is diffable against the one before it, and closes with one summary
// line. preserved is the count carried forward under incompleteIDs.
func (n *Notifier) emitAll(preserved int) {
	keys := make([]string, 0, len(n.current))
	for key := range n.current {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	emitted, suppressed := 0, 0
	for _, key := range keys {
		f := n.current[key]
		if _, ignored := n.ignore[f.AniListID]; ignored {
			suppressed++
			continue
		}
		n.emit(&f)
		emitted++
	}
	n.log.Info("findings reported",
		"total", len(n.current), "emitted", emitted,
		"suppressed", suppressed, "preserved", preserved)
}

// --- Emission / rendering ---

// emit logs a finding at the level its status maps to, with the full field
// set the dashboard and Loki alert key on.
func (n *Notifier) emit(f *compare.Finding) {
	n.log.Log(context.Background(), level(f.Status), message(f.Status), findingKVs(f)...)
}

// maxAttrBytes is the per-attribute volume budget the emit path enforces on
// every untrusted value. The policy itself (the budget, the truncation
// marker, the rune sanitization, and the cap-before-sanitize order) lives in
// internal/logattr, shared with the report's slog path so the two emitters
// cannot drift; this alias keeps the package's own bound readable and pins the
// value the persisted-record cap is sized against.
const maxAttrBytes = logattr.MaxBytes

// attrTruncMarker is the suffix a capped attribute (or persisted string)
// carries so a reader can tell a truncated value from an honest one.
const attrTruncMarker = logattr.TruncMarker

// capAttr renders one untrusted single-value attribute for the JSON slog
// sink through the shared bounded primitive: honest values pass
// byte-identical; a hostile oversized value (SeaDex admits multi-MB URLs, up
// to 512 per entry) is truncated on a rune boundary with the "..." marker so
// one record can never balloon past downstream log-pipeline line limits (alert
// suppression) or amplify memory. A MULTI-SOURCE attribute renders through
// joinGroupsAttr / joinLinksAttr instead (see findingKVs).
func capAttr(s string) string { return logattr.Cap(s) }

// reboundAttr re-applies the per-attribute byte budget to a value a
// post-cap transform may have grown: capAttr's own output can be
// maxAttrBytes+len(attrTruncMarker) once the marker is appended, and both
// Markdown escapers expand the value they walk. An in-budget value passes
// through unchanged, so an honest value stays byte-identical, and the
// marker arithmetic has one home.
func reboundAttr(s string) string {
	if len(s) <= maxAttrBytes {
		return s
	}
	return runesafe.CapBytes(s, maxAttrBytes-len(attrTruncMarker)) + attrTruncMarker
}

// capURLAttr renders one untrusted URL attribute: capAttr's bounded,
// sanitized pass first (so the escaper never walks an unbounded string),
// then the link-destination escaping, then a re-cap because escaping can
// grow the value - the same shape capPersisted uses.
//
// The escaping itself lives in logattr.EscapeLinkDestination, the one home it
// shares with internal/audit's escapeLinkURL: the shipped alerts.yaml renders
// arr_url / nyaa_url / public_url / ab_url as `[label](<attr>)` for
// Discord/Slack, so a ')' (or a space runesafe.Sanitize substituted for a
// hostile rune) would close the destination early and the remainder of the
// value would render as attacker-authored markdown.
func capURLAttr(s string) string {
	return reboundAttr(logattr.EscapeLinkDestination(capAttr(s)))
}

// mdTextEscaper neutralizes the characters an untrusted TEXT value must not
// carry into a Markdown annotation BODY. capAttr bounds and sanitizes a
// value for the JSON slog sink, but it deliberately performs no output
// encoding for a downstream markup sink, so a title such as
// `[security update](https://attacker.example)` or `@everyone` survives it and
// the shipped alerts.yaml interpolates it verbatim into the annotation, where
// it renders as active markup (CWE-116, context-confused output encoding).
// CommonMark/Discord punctuation is backslash-escaped (including '@', which is
// how Discord suppresses an @everyone / @here mention and also covers the
// '<@id>' user-mention form). Unlike logattr.EscapeLinkDestination this is for
// a text SPAN, not a link destination, so '[' and ']' ARE escaped - there is no
// IPv6-literal case to preserve here. It also flattens CR/LF, which capAttr's
// raw label deliberately keeps.
//
// This encoder targets ONE sink: Discord. The receiver is decided outside this
// repo, in cplieger/homelab's `apps/mimir/mimir.yaml`, which provisions the
// bundled Alertmanager with the Discord-receiver fallback config. Encoding for
// two dialects at once meant the sink the operator did NOT choose rendered the
// other's encoding bytes as literal text, so an honest title reached the
// annotation as `Tiger &amp; Bunny`. The Slack-only half - entity-encoding
// '&', '<' and '>' for mrkdwn's `<url|text>` and `<!everyone>` forms - is
// therefore GONE: it never protected against Discord's own mention syntax
// (`@everyone` / `@here` carry no '<') and Discord's `<@id>` mention is already
// covered by the '@' escape, so dropping it removes no Discord defense. The
// backslash escapes are what keep the alert-template link-injection class shut
// and must stay. Switching that Alertmanager receiver to Slack (or any
// mrkdwn sink) requires revisiting this escaper.
var mdTextEscaper = strings.NewReplacer(
	"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]",
	"(", "\\(", ")", "\\)", "~", "\\~", "|", "\\|", "@", "\\@",
	// CR/LF survive capAttr on purpose (runesafe.Sanitize is the
	// keepCRLF=true policy, because the JSON slog handler escapes them for
	// its own sink), but the annotation body is a single line: a newline
	// re-opens the line-start constructs inline escaping cannot reach (a
	// '#' heading, a list bullet, an auto-linked bare URL). They collapse
	// to a space, matching logattr.EscapeLinkDestination's percent-encoding
	// of both.
	"\r", " ", "\n", " ",
)

// capAlertTextAttr renders one untrusted text attribute for a MARKDOWN sink:
// capAttr's bounded, sanitized pass first (so the escaper never walks an
// unbounded string), then the markup escaping, then a re-cap because escaping
// grows the value - the same shape capURLAttr uses. It is the alert-safe twin
// of capAttr: the raw capAttr label stays for Loki search and grouping, and
// this value is what an annotation interpolates.
func capAlertTextAttr(s string) string {
	return trimTruncatedEscape(reboundAttr(mdTextEscaper.Replace(capAttr(s))))
}

// trimTruncatedEscape drops a truncation-orphaned trailing backslash. The
// re-cap in capAlertTextAttr cuts the ESCAPED string, so the cut can fall
// between a backslash and the character it escapes, leaving a value that ends
// "\" immediately before the "..." marker - where the reader sees a dangling
// escape and a Markdown sink reads the marker's first '.' as the escaped
// character. Only a TRUNCATED value can carry one: mdTextEscaper emits
// backslashes in pairs (an input backslash becomes two, an escaped
// metacharacter is preceded by one), so an odd trailing run in the cut prefix is
// exactly the half-emitted escape, and an honest value that genuinely ends in
// "..." keeps its even run untouched.
func trimTruncatedEscape(s string) string {
	body, truncated := strings.CutSuffix(s, attrTruncMarker)
	if !truncated {
		return s
	}
	run := 0
	for i := len(body) - 1; i >= 0 && body[i] == '\\'; i-- {
		run++
	}
	if run%2 == 1 {
		body = body[:len(body)-1]
	}
	return body + attrTruncMarker
}

// findingKVs builds the structured key-value attributes for a finding line.
// It carries the arr deep-link, the split Nyaa/AnimeBytes URLs, the season, and
// a compact seadex_tags line so an alert can render a self-contained,
// clickable notification straight from the labels. Every attribute derived
// from untrusted upstream data (SeaDex/tracker titles, groups, URLs, hashes)
// is passed through capAttr: runesafe.Sanitize (the same policy the audit
// report's slog path applies, because slog's JSONHandler escapes C0 controls
// but emits C1 controls and bidi controls raw) plus a volume cap mirroring
// the bound the dedupe-key path applies to the same data. A LINK-DESTINATION
// attribute (arr_url, release_url, nyaa_url, public_url, ab_url) goes through
// capURLAttr instead, which adds the Markdown escaping the shipped
// alerts.yaml's `[label](<attr>)` rendering requires. A MULTI-SOURCE
// attribute (recommended_groups, release_urls) applies that same policy
// through joinGroupsAttr / joinLinksAttr, which never materialize the
// untrusted aggregate before the cap. Fixed-pattern app values (resolution,
// codec, kind, season, al_id, arr, status) stay raw.
func findingKVs(f *compare.Finding) []any {
	publicLink, abLink := trackerURLs(f.Links)
	return []any{
		"title", capAttr(f.Title),
		// alert_title / alert_recommended_group are the MARKDOWN-safe twins of
		// title / recommended_group: the raw labels keep their meaning for
		// Loki search and `sum by` grouping, while alerts.yaml interpolates
		// these into its Discord annotations so an untrusted title can
		// never render as active markup or a mention (see capAlertTextAttr,
		// which targets that one sink).
		"alert_title", capAlertTextAttr(f.Title),
		"al_id", f.AniListID,
		"arr", f.Arr,
		"arr_url", capURLAttr(library.SafeLogURL(f.ArrURL)),
		"season", f.Season,
		"scope", f.Scope,
		"approx", f.Approx,
		"current_group", capAttr(f.CurrentGroup),
		"recommended_group", capAttr(f.RecommendedGroup),
		"alert_recommended_group", capAlertTextAttr(f.RecommendedGroup),
		"recommended_groups", joinGroupsAttr(f.RecommendedGroups),
		"tracker", capAttr(f.Tracker),
		"resolution", f.Resolution,
		"codec", f.Codec,
		"kind", f.Kind,
		"classification_reason", capAttr(f.Reason),
		"release_url", capURLAttr(f.ReleaseURL),
		"release_urls", joinLinksAttr(f.Links),
		// nyaa_url keeps its name and its meaning: the shipped alerts.yaml
		// renders it as a "[Nyaa]" link, so it may only ever hold a Nyaa URL.
		// A public link from another tracker (AnimeTosho, RuTracker - both
		// supported and present in the catalogue) rides public_url beside the
		// tracker's real name, so the alert can label it truthfully instead of
		// calling it Nyaa (l-f5).
		"nyaa_url", capURLAttr(publicLink.nyaaURL()),
		"public_url", capURLAttr(publicLink.otherURL()),
		// public_tracker is interpolated INSIDE a Markdown link label
		// ([{{ public_tracker }}]({{ public_url }})), so it takes the
		// markup-safe render rather than capAttr: canonicalTracker's
		// bare-host last resort can return a value carrying ']' or '(',
		// which would close the label early. No alert_* twin is needed -
		// every honest value (a canonical tracker name or a hostname)
		// passes the escaper byte-identical, so the Loki grouping label
		// is unchanged.
		"public_tracker", capAlertTextAttr(publicLink.otherTracker()),
		"ab_url", capURLAttr(abLink.url),
		// ab_tracker names the ab_url link the way public_tracker names
		// public_url, and takes the same markup-safe render for the same
		// reason (it is interpolated INSIDE a Markdown link label). It is
		// added BESIDE ab_url rather than narrowing that attribute, so a
		// deployed Loki rule keyed on ab_url keeps working while the label
		// stops claiming AnimeBytes for a link whose host says otherwise.
		"ab_tracker", capAlertTextAttr(abLink.abTracker()),
		"info_hash", capAttr(f.InfoHash),
		"seadex_tags", seadexTags(f),
		"status", string(f.Status),
	}
}

// trackerLinkKind classifies a finding link for trackerURLs' slot routing.
type trackerLinkKind uint8

const (
	trackerLinkPublic trackerLinkKind = iota
	trackerLinkNyaa
	trackerLinkABFallback
	trackerLinkAB
)

// classifyTrackerLink maps a link to its slot kind: definite AnimeBytes
// evidence wins outright, ambiguous evidence is the conservative AB fallback,
// a known Nyaa link is the public Nyaa source, and anything else is a generic
// public link. The switch is exhaustive over filter.ABEvidence, so the three
// grades cannot be tested in the wrong order.
//
// The grade is READ off the link (compare graded it from the raw upstream URL),
// never re-derived here from the published one: that grading has a single home
// in internal/classify, whose invariant is that the evidence must come from the
// raw value because publishing rewrites or erases the host evidence it reads
// (h-f43). What stays this package's policy is the slot PRECEDENCE below.
func classifyTrackerLink(link compare.ReleaseLink) trackerLinkKind {
	switch link.AB {
	case filter.ABDefinite:
		return trackerLinkAB
	case filter.ABAmbiguous:
		return trackerLinkABFallback
	case filter.ABNone:
		if (publicLink{url: link.URL, tracker: link.Tracker}).isNyaa() {
			return trackerLinkNyaa
		}
		return trackerLinkPublic
	default:
		// Unreachable: ABEvidence has exactly the three grades above. A new
		// grade must not silently become a clickable public link, so it
		// routes to the conservative fallback.
		return trackerLinkABFallback
	}
}

// trackerURLs splits a finding's obtainable links into the public and
// AnimeBytes URLs, so an alert can render a distinct public link and AB link.
// AB routing reads the evidence the producer graded from each release's RAW
// upstream URL (compare.ReleaseLink.AB, from classify.ABEvidence), matching
// the obtainability filter (this package's dedupe key, dedupekey.go, keys the
// full obtainable URL set label-insensitively instead): a link graded
// filter.ABDefinite (AB label or animebytes.tv URL host) wins the AB slot
// outright, ahead of filter.ABAmbiguous's conservative fail-closed fallback.
// A malformed, unparseable, or non-ASCII-host raw URL grades ambiguous; such an
// unclassifiable link only fills the AB slot when no definite AnimeBytes
// link exists, so an ambiguous link is never rendered as the clickable
// public URL yet can never displace a genuine AB link either.
//
// The public slot is returned WITH the tracker it came from, because it is not
// always Nyaa: the catalogue also carries AnimeTosho and RuTracker releases
// (both supported by release/trackers.go). The shipped alerts.yaml renders
// `nyaa_url` under a hardcoded `[Nyaa]` label, so a non-Nyaa public URL must
// arrive as `public_url` + `public_tracker` rather than be mislabeled.
//
// The AB slot is returned the same way, and for the mirror-image reason: the
// grade that fills it reads the untrusted SeaDex tracker LABEL first (that is
// what makes it a fail-closed HIDE gate), so an `AB`-labeled link carrying a
// public tracker's page URL lands here legitimately. It is published as
// `ab_url` + `ab_tracker`, and the alert labels it with the name
// `abTracker` resolves rather than a hardcoded `[AB]`, so a link whose host is
// nyaa.si is offered under Nyaa's own name instead of being announced as
// AnimeBytes (l-f121). Routing is unchanged - fail-closed is still right for
// the toggle - only the label stops asserting more than the evidence supports.
//
// Selection is HEADLINE-first, then Nyaa-first WITHIN each affinity tier.
// compare.obtainableLinks ranks the headline candidate's own sources first so
// the rendered link belongs to the group Finding.RecommendedGroup names; a
// tracker-class-only preference would discard that, letting another
// recommended group's Nyaa link outrank a headline-group AnimeTosho link and
// present it as the action for a group it does not belong to.
func trackerURLs(links []compare.ReleaseLink) (public, ab publicLink) {
	var headlineNyaa, headlinePublic, firstNyaa, firstPublic, firstABFallback publicLink
	for i := range links {
		link := publicLink{url: links[i].URL, tracker: links[i].Tracker}
		switch classifyTrackerLink(links[i]) {
		case trackerLinkAB:
			ab.setFirst(link)
		case trackerLinkABFallback:
			firstABFallback.setFirst(link)
		case trackerLinkNyaa:
			if links[i].Headline {
				headlineNyaa.setFirst(link)
			} else {
				firstNyaa.setFirst(link)
			}
		case trackerLinkPublic:
			if links[i].Headline {
				headlinePublic.setFirst(link)
			} else {
				firstPublic.setFirst(link)
			}
		}
	}
	// Headline affinity outranks tracker class; Nyaa (the dominant public
	// tracker) comes first within each tier. setFirst is first-wins, so the
	// order of these four calls IS the preference order.
	public.setFirst(headlineNyaa)
	public.setFirst(headlinePublic)
	public.setFirst(firstNyaa)
	public.setFirst(firstPublic)
	ab.setFirst(firstABFallback)
	return public, ab
}

// publicLink is one public-tracker source: the URL plus the tracker it belongs
// to, so an alert can name the link it renders.
type publicLink struct {
	url     string
	tracker string
}

// setFirst adopts other when this slot is still empty, preserving the
// first-link-wins precedence trackerURLs applies within each slot.
func (p *publicLink) setFirst(other publicLink) {
	if p.url == "" {
		*p = other
	}
}

// canonicalTracker resolves the name to label this link with, through
// tracker.CanonicalName - the one home of the label-then-host ladder, so the
// alert and the season report's links cell name the same link the same way and
// a tracker-table edit reaches both (l-f117). Empty only for a link with
// neither a known host, a known label, nor any host at all.
func (p publicLink) canonicalTracker() string {
	return tracker.CanonicalName(p.tracker, p.url)
}

// isNyaa reports whether the link is Nyaa's - the one public tracker the shipped
// alert template has a hardcoded "[Nyaa]" label for. Resolved through
// canonicalTracker, so a Nyaa URL carrying an alias, odd casing, or no tracker
// label at all still reads as Nyaa instead of falling into the generic slot with
// nothing to name it.
func (p publicLink) isNyaa() bool {
	return p.url != "" && p.canonicalTracker() == tracker.NameNyaa
}

// nyaaURL returns the URL for the legacy nyaa_url attribute: only ever a Nyaa
// link, so the alert's hardcoded "[Nyaa]" label cannot lie.
func (p publicLink) nyaaURL() string {
	if p.isNyaa() {
		return p.url
	}
	return ""
}

// otherURL returns the URL for the public_url attribute: a public link from a
// tracker OTHER than Nyaa (AnimeTosho, RuTracker), which the alert renders under
// the name otherTracker reports.
func (p publicLink) otherURL() string {
	if p.isNyaa() {
		return ""
	}
	return p.url
}

// otherTracker returns the tracker name accompanying otherURL, so the alert can
// label the link with the tracker it actually came from. Resolved through
// canonicalTracker rather than the raw upstream label, so a public link whose
// SeaDex tracker field is an alias, oddly cased, or empty is still named (by its
// host as a last resort) instead of rendering a nameless link. Empty when there
// is no non-Nyaa public link to name.
func (p publicLink) otherTracker() string {
	if p.otherURL() == "" {
		return ""
	}
	return p.canonicalTracker()
}

// abTracker returns the tracker name accompanying the AB slot's URL, so the
// alert labels that link with the tracker it actually came from instead of a
// hardcoded "AB". The slot is filled by a fail-closed grade that trusts the
// SeaDex tracker LABEL first, so its URL is not always an AnimeBytes one: an
// `AB`-labeled record carrying a nyaa.si page URL reads as Nyaa here and the
// alert says so (l-f121). AnimeBytes is the last resort rather than "": the
// slot's own meaning is "AnimeBytes, or something that might be", so an
// unclassifiable URL with no nameable host is still announced as the tracker
// the operator's toggle admitted it under. Empty when there is no AB link.
func (p publicLink) abTracker() string {
	if p.url == "" {
		return ""
	}
	if name := p.canonicalTracker(); name != "" {
		return name
	}
	return tracker.NameAnimeBytes
}

// seadexTags renders a compact descriptive tag line for a finding — the SeaDex
// qualifier (best / incomplete / theoretical-best / mixed-group / unverifiable),
// the release kind, resolution, and dual-audio — for an alert footer. Only best
// releases are ever surfaced, so "alt" never appears.
func seadexTags(f *compare.Finding) string {
	var tags []string
	switch f.Status {
	case compare.StatusBetter:
		tags = append(tags, "best")
	case compare.StatusIncomplete:
		tags = append(tags, "incomplete")
	case compare.StatusTheoretical:
		tags = append(tags, "theoretical-best")
	case compare.StatusMixedGroup:
		tags = append(tags, "mixed-group")
	case compare.StatusUnverifiable:
		tags = append(tags, "unverifiable")
	}
	if f.Kind != "" && f.Kind != string(release.KindUnknown) {
		tags = append(tags, f.Kind)
	}
	if f.Resolution != "" {
		tags = append(tags, f.Resolution)
	}
	if f.DualAudio {
		tags = append(tags, "dual-audio")
	}
	return strings.Join(tags, " · ")
}

// joinLinksAttr renders every obtainable source for the recommended release as
// a space-separated "tracker=url" list, so a finding carries both a Nyaa and an
// AnimeBytes link when the release exists on both, not just the headline one.
// Each tracker and URL is written straight into the bounded joiner - never
// concatenated into a "tracker=url" string or a []string first - so the
// attribute's byte budget bounds the work, not just the result.
//
// Deliberately NOT Markdown-escaped like the single-link attributes
// (capURLAttr): alerts.yaml never renders release_urls as a link destination,
// and escaping inside the joiner would have to materialize the untrusted
// aggregate before the cap applies.
func joinLinksAttr(links []compare.ReleaseLink) string {
	j := logattr.NewJoiner()
	for i := range links {
		if i > 0 && !j.WriteSep(" ") {
			break
		}
		if !j.Write(links[i].Tracker) || !j.WriteSep("=") || !j.Write(links[i].URL) {
			break
		}
	}
	return j.String()
}

// joinGroupsAttr renders the recommended release groups as a comma-separated
// list through the same bounded joiner, for the same reason: the group list is
// untrusted SeaDex data and must not be materialized before the cap applies.
func joinGroupsAttr(groups []string) string {
	j := logattr.NewJoiner()
	for i := range groups {
		if i > 0 && !j.WriteSep(",") {
			break
		}
		if !j.Write(groups[i]) {
			break
		}
	}
	return j.String()
}

// level maps a finding status to its slog level: an actionable better release
// warns, every informational nudge logs at info. It is the emission-level
// policy's one home, beside message() - the sibling half of the same
// status-to-log-line contract - so a new status cannot ship with a message
// entry and a silently wrong level.
func level(s compare.Status) slog.Level {
	if s == compare.StatusBetter {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// message returns the human-facing log message for a finding status.
func message(status compare.Status) string {
	switch status {
	case compare.StatusBetter:
		return "better release available"
	case compare.StatusMixedGroup:
		return "series spans multiple release groups, manual review"
	case compare.StatusIncomplete:
		return "SeaDex entry is incomplete"
	case compare.StatusTheoretical:
		return "SeaDex lists a theoretical best only"
	case compare.StatusUnverifiable:
		return "release group unverifiable, manual review"
	default:
		return "seadex finding"
	}
}

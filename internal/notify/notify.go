// Package notify emits the current finding SET as structured slog events,
// re-stating every row on every pass - the daemon's NOTIFICATION path (Loki
// alerting rides these lines). Nothing is persisted and nothing is deduped
// across cycles; see Notifier for why.
// Observability is slog-only; there is no metrics endpoint. It is distinct
// from the user-facing report FEATURE (the `report` subcommand's season-level
// audit), which lives in internal/audit.
package notify

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/tracker"
)

// --- Notifier / state reporting ---

// Notifier reports findings as STATE rather than as events: it holds the set
// of conditions currently true and re-emits the whole set on every pass, so
// the alerting stack (Loki rule -> Alertmanager) owns notification policy.
type Notifier struct {
	log *slog.Logger
	// ignore is the operator's filters.ignore set (AniList IDs).
	ignore map[int]struct{}
	// current is the set of conditions true as of the last completed pass,
	// keyed by dedupe key.
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
// incompleteIDs scopes what replacement may DELETE.
func (n *Notifier) Report(findings []compare.Finding, incompleteIDs map[int]struct{}) {
	n.report(findings, nil, incompleteIDs)
}

// ReportScoped is Report for a PARTIAL pass: only the rows owned by an AniList
// ID in comparedIDs may be deleted, and every other row is carried forward
// untouched.
func (n *Notifier) ReportScoped(findings []compare.Finding, comparedIDs, incompleteIDs map[int]struct{}) {
	if comparedIDs == nil {
		// Load-bearing: Report overloads a nil set as FULL deletion authority, so
		// forwarding nil would make this partial pass delete every row outside its window.
		comparedIDs = map[int]struct{}{}
	}
	n.report(findings, comparedIDs, incompleteIDs)
}

// Reemit re-emits the current finding set unchanged, comparing nothing.
//
// It exists because findings are STATE, not events: the alert rules read a
// lookback window over the emitted lines, so a condition stops being reported
// only when the app stops emitting it.
func (n *Notifier) Reemit() {
	// Nothing was evaluated, so nothing was eligible for deletion: resolved is 0.
	n.emitAll(0, len(n.current), 0)
}

// report is the shared body. comparedIDs nil means FULL deletion authority
// (every row may be deleted by omission); non-nil bounds it to those owners.
func (n *Notifier) report(findings []compare.Finding, comparedIDs, incompleteIDs map[int]struct{}) {
	next := make(map[string]compare.Finding, len(findings))
	// Last-payload-wins per key.
	for i := range findings {
		key := dedupeKey(&findings[i])
		retained := findings[i]
		boundRetained(&retained)
		next[key] = retained
	}
	preserved, carried, resolved := 0, 0, 0
	for key := range n.current {
		if _, present := next[key]; present {
			continue
		}
		owner := n.current[key].AniListID
		if _, incomplete := incompleteIDs[owner]; incomplete {
			next[key] = n.current[key]
			preserved++
			continue
		}
		if comparedIDs == nil {
			// Full authority: absence IS resolution.
			resolved++
			continue
		}
		if _, authorized := comparedIDs[owner]; !authorized {
			next[key] = n.current[key]
			carried++
			continue
		}
		resolved++
	}
	n.current = next
	n.emitAll(preserved, carried, resolved)
}

// maxRetainedListItems bounds how many elements of a retained row's untrusted
// SLICES survive retention.
const maxRetainedListItems = 64

// maxRetainedElemBytes bounds one ELEMENT of a retained slice, and it is what
// makes the row bound real.
const maxRetainedElemBytes = 256

// capRetainedList clones the retained PREFIX of an untrusted slice, dropping
// anything past maxRetainedListItems. Cloning is what keeps boundRetained's
// aliasing guard (f is a shallow copy, so the header still points at the
// compare result the audit report and the cycle log line also read); cloning
// the PREFIX additionally releases the caller's oversized backing array.
func capRetainedList[T any](s []T) []T {
	if len(s) > maxRetainedListItems {
		s = s[:maxRetainedListItems]
	}
	return slices.Clone(s)
}

// boundRetained caps the UNTRUSTED strings of a row about to be RETAINED, in
// place. Every value it caps is parsed from SeaDex data or library file names,
// and a retained row outlives the pass that produced it, so the cap has to
// happen on the way IN - the emit path's own caps bound only what is written to
// the log and leave the resident value whole.
func boundRetained(f *compare.Finding) {
	f.RecommendedGroups = capRetainedList(f.RecommendedGroups)
	f.CurrentGroups = capRetainedList(f.CurrentGroups)
	f.Links = capRetainedList(f.Links)
	f.Title = capAttr(f.Title)
	f.Reason = capAttr(f.Reason)
	f.Tracker = capAttr(f.Tracker)
	f.InfoHash = capAttr(f.InfoHash)
	f.CurrentGroup = capAttr(f.CurrentGroup)
	f.RecommendedGroup = capAttr(f.RecommendedGroup)
	// capAttr, NOT capURLAttr: the retention bound is a SIZE bound, while
	// capURLAttr is the emit path's link-destination ENCODER (it percent-encodes
	// for a Markdown sink).
	f.ReleaseURL = capAttr(f.ReleaseURL)
	f.ArrURL = capAttr(f.ArrURL)
	// The three untrusted SLICES are bounded per ELEMENT on the measured
	// maxRetainedElemBytes rather than the Loki log-line budget capAttr carries:
	// the count cap alone left the row at 2 MiB (see maxRetainedElemBytes).
	for i := range f.RecommendedGroups {
		f.RecommendedGroups[i] = capRetainedElem(f.RecommendedGroups[i])
	}
	for i := range f.CurrentGroups {
		f.CurrentGroups[i] = capRetainedElem(f.CurrentGroups[i])
	}
	for i := range f.Links {
		f.Links[i].Tracker = capRetainedElem(f.Links[i].Tracker)
		f.Links[i].URL = capRetainedElem(f.Links[i].URL)
	}
}

// capRetainedElem bounds one element of a retained slice. capAttr runs first so
// the value is rune-sanitized under the same policy every other retained field
// gets, then the element budget applies; an in-budget value passes through
// byte-identical, which is what keeps an honest row unchanged across passes
// (capAttr is idempotent and so is a re-cap of an already-short value).
func capRetainedElem(s string) string { return reboundTo(capAttr(s), maxRetainedElemBytes) }

// emitAll logs every row of the current set, in a deterministic order so a
// pass is diffable against the one before it, and closes with one summary line.
func (n *Notifier) emitAll(preserved, carried, resolved int) {
	keys := make([]string, 0, len(n.current))
	for key := range n.current {
		keys = append(keys, key)
	}
	slices.Sort(keys)

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
		"suppressed", suppressed, "preserved", preserved, "carried", carried,
		"resolved", resolved)
}

// --- Emission / rendering ---

// emit logs a finding at the level its status maps to, with the full field
// set the dashboard and Loki alert key on.
func (n *Notifier) emit(f *compare.Finding) {
	n.log.Log(context.Background(), level(f.Status), message(f.Status), findingKVs(f)...)
}

// maxAttrBytes is the per-attribute volume budget the emit path enforces on
// every untrusted value.
const maxAttrBytes = logattr.MaxBytes

// maxAlertTextBytes is the budget for an ALERT-destined TEXT attribute, and it
// is deliberately far below maxAttrBytes: that budget is sized for the Loki LOG
// LINE, while these values are interpolated into the Discord annotation
// alerts.yaml renders.
const maxAlertTextBytes = 512

// maxAlertURLBytes is the same budget for an ALERT-destined URL attribute - one
// the shipped alerts.yaml actually interpolates - and it is MEASURED rather than
// chosen.
const maxAlertURLBytes = 256

// attrTruncMarker is the suffix a capped attribute carries so a reader can
// tell a truncated value from an honest one.
const attrTruncMarker = logattr.TruncMarker

// capAttr renders one untrusted single-value attribute for the JSON slog
// sink through the shared bounded primitive: honest values pass
// byte-identical; a hostile oversized value (SeaDex admits multi-MB URLs, up
// to 512 per entry) is truncated on a rune boundary with the "..." marker so
// one record can never balloon past downstream log-pipeline line limits (alert
// suppression) or amplify memory. A MULTI-SOURCE attribute renders through
// joinGroupsAttr / joinLinksAttr instead (see findingKVs).
func capAttr(s string) string { return logattr.Cap(s) }

// reboundTo re-applies a byte budget to a value a post-cap transform may have
// grown, cutting on a rune boundary and marking the cut. An in-budget value
// passes through unchanged, so an honest value stays byte-identical.
func reboundTo(s string, budget int) string {
	text, _ := runesafe.SanitizeCapped(s, budget, attrTruncMarker)
	return text
}

// capURLAttr renders one untrusted URL attribute: capAttr's bounded,
// sanitized pass first (so the escaper never walks an unbounded string),
// then the link-destination escaping, then a re-cap because escaping can grow the value.
func capURLAttr(s string) string {
	return reboundTo(logattr.EscapeLinkDestination(capAttr(s)), maxAlertURLBytes)
}

// mdTextEscaper neutralizes the characters an untrusted TEXT value must not
// carry into a Markdown annotation BODY.
var mdTextEscaper = strings.NewReplacer(
	"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]",
	"(", "\\(", ")", "\\)", "~", "\\~", "|", "\\|",
	// CR/LF survive capAttr on purpose (the JSON handler escapes them for its own sink),
	// but the annotation body is a single line: a newline re-opens the line-start
	// constructs inline escaping cannot reach.
	"\r", " ", "\n", " ",
)

// capAlertTextAttr renders one untrusted text attribute for a MARKDOWN sink:
// capAttr's bounded, sanitized pass first (so the escaper never walks an
// unbounded string), then the markup escaping, then a re-cap because escaping
// grows the value - the same shape capURLAttr uses.
func capAlertTextAttr(s string) string {
	capped := reboundTo(capAttr(s), maxAlertTextBytes)
	return trimTruncatedEscape(reboundTo(mdTextEscaper.Replace(capped), maxAlertTextBytes))
}

// trimTruncatedEscape drops a truncation-orphaned trailing backslash.
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
// clickable notification straight from the labels.
func findingKVs(f *compare.Finding) []any {
	publicLink, abLink := trackerURLs(f.Links)
	return []any{
		"title", capAttr(f.Title),
		// alert_title / alert_recommended_group are the MARKDOWN-safe twins of title /
		// recommended_group: the raw labels keep their meaning for Loki search and `sum
		// by` grouping, while alerts.yaml interpolates these into Discord annotations.
		"alert_title", capAlertTextAttr(f.Title),
		"al_id", f.AniListID,
		"arr", f.Arr,
		"arr_url", capURLAttr(f.ArrURL),
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
		// release_url takes the LOG-LINE budget (capAttr), not the alert one: alerts.yaml
		// neither groups by it nor renders it, and it can exceed 256 bytes because
		// publishing enforces a canonical host and shape, never a length.
		"release_url", capAttr(f.ReleaseURL),
		"release_urls", joinLinksAttr(f.Links),
		// nyaa_url keeps its name and its meaning: the shipped alerts.yaml
		// renders it as a "[Nyaa]" link, so it may only ever hold a Nyaa URL.
		"nyaa_url", capURLAttr(publicLink.nyaaURL()),
		"public_url", capURLAttr(publicLink.otherURL()),
		// public_tracker is interpolated INSIDE a Markdown link label, so it takes the
		// markup-safe render: canonicalTracker's bare-host last resort can return a value
		// carrying ']' or '(', which would close the label early.
		"public_tracker", capAlertTextAttr(publicLink.otherTracker()),
		"ab_url", capURLAttr(abLink.url),
		// ab_tracker names the ab_url link the way public_tracker names
		// public_url, and takes the same markup-safe render for the same
		// reason (it is interpolated INSIDE a Markdown link label).
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
// public link. The switch is exhaustive over tracker.ABEvidence, so the three
// grades cannot be tested in the wrong order.
func classifyTrackerLink(link compare.ReleaseLink) trackerLinkKind {
	switch link.AB {
	case tracker.ABDefinite:
		return trackerLinkAB
	case tracker.ABAmbiguous:
		return trackerLinkABFallback
	case tracker.ABNone:
		if (publicLink{url: link.URL, tracker: link.Tracker}).isNyaa() {
			return trackerLinkNyaa
		}
		return trackerLinkPublic
	default:
		// Unreachable: ABEvidence has exactly the three grades above.
		return trackerLinkABFallback
	}
}

// trackerURLs splits a finding's obtainable links into the public and
// AnimeBytes URLs, so an alert can render a distinct public link and AB link.
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
	// tracker) comes first within each tier.
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
// a tracker-table edit reaches both. Empty only for a link with
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
// host as a last resort) instead of rendering a nameless link.
func (p publicLink) otherTracker() string {
	if p.otherURL() == "" {
		return ""
	}
	return p.canonicalTracker()
}

// abTracker returns the tracker name accompanying the AB slot's URL, so the
// alert labels that link with the tracker it actually came from instead of a
// hardcoded "AB".
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
// Each link is charged as one unit: a tracker without its "=url" is not a
// source, and rendering one would put a bare tracker name where the attribute's
// readers expect a pair.
func joinLinksAttr(links []compare.ReleaseLink) string {
	j := logattr.NewJoiner()
	for i := range links {
		if i > 0 && !j.WriteSep(" ") {
			break
		}
		if !j.WritePair(links[i].Tracker, "=", links[i].URL) {
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

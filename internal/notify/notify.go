// Package notify emits findings as structured slog events with cross-cycle
// dedupe - the daemon's NOTIFICATION path (Loki alerting rides these lines).
// Observability is slog-only; there is no metrics endpoint. It is distinct
// from the user-facing report FEATURE (the `report` subcommand's season-level
// audit), which lives in internal/audit - this package was named `report`
// until 2026-07, and the rename ended the standing collision between the two.
package notify

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/release"
)

// Alerted is a persisted dedupe record: when the finding was first alerted
// plus the trimmed subset of it the resolution path reads back, keyed in the
// state by the finding's dedupe key.
type Alerted struct {
	AlertedAt time.Time     `json:"alerted_at"`
	Finding   StoredFinding `json:"finding"`
}

// StoredFinding is the subset of a compare.Finding the dedupe record
// persists: exactly the fields read back across cycles - emitResolved's
// resolution line (title, al_id, arr, season, current_group, status,
// recommended_group) and Notify's failed-item preservation scope, keyed on
// AniListID. The record used to persist the full sanitized Finding, but
// everything beyond this set was write-only ballast in state.json (including
// the ArrURL whose on-disk sanitization the trim makes moot: no URL is
// persisted at all). The JSON tags mirror compare.Finding's, so a state file
// written before the trim decodes cleanly (its extra fields are ignored);
// the dedupe key stays the state map's key, so dedupe continuity and
// resolution semantics are unchanged.
type StoredFinding struct {
	Arr              string         `json:"arr"`
	CurrentGroup     string         `json:"current_group,omitempty"`
	RecommendedGroup string         `json:"recommended_group,omitempty"`
	Title            string         `json:"title"`
	Status           compare.Status `json:"status"`
	AniListID        int            `json:"al_id"`
	Season           int            `json:"season,omitempty"`
}

// --- Notifier / cross-cycle dedupe ---

// Notifier emits findings as slog events with cross-cycle dedupe.
type Notifier struct {
	log *slog.Logger
}

// storedFinding projects f onto the trimmed record the dedupe state persists
// (see StoredFinding). The three upstream-controlled strings (title and the
// two group names) are bounded at projection time by capPersisted: the emit
// path's per-attribute cap only bounds what is LOGGED, while these values also
// ride into state.json, where a single multi-MB value could push the encoded
// state past its 32 MiB write bound and freeze dedupe persistence for every
// later cycle (CWE-400). Arr and Status are app-defined vocabularies, so they
// stay raw.
func storedFinding(f *compare.Finding) StoredFinding {
	return StoredFinding{
		Arr:              f.Arr,
		CurrentGroup:     capPersisted(f.CurrentGroup),
		RecommendedGroup: capPersisted(f.RecommendedGroup),
		Title:            capPersisted(f.Title),
		Status:           f.Status,
		AniListID:        f.AniListID,
		Season:           f.Season,
	}
}

// capPersisted bounds one untrusted string for PERSISTENCE in the dedupe
// record. It reuses capAttr's sanitize-and-cap pass, then guarantees the
// result never exceeds maxAttrBytes: capAttr's own output can be
// maxAttrBytes+len(attrTruncMarker) once the marker is appended, which is fine
// for a log line but would let the persisted record drift past the budget the
// state's write bound is sized against. Honest values pass byte-identical, and
// the helper is idempotent, so re-projecting an already-capped record (a value
// read back from legacy state) is a no-op.
func capPersisted(s string) string {
	capped := capAttr(s)
	if len(capped) <= maxAttrBytes {
		return capped
	}
	return runesafe.CapBytes(capped, maxAttrBytes-len(attrTruncMarker)) + attrTruncMarker
}

// NewNotifier builds a Notifier. logger may be nil.
func NewNotifier(logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{log: logger}
}

// Notify emits new findings, suppresses ones already alerted (carrying their
// original alert time forward), logs a one-line resolution for any prior finding
// no longer present, and returns the new dedupe state to persist.
//
// failedItems scopes resolution to items whose evidence is incomplete this
// cycle: a prior finding whose AniList ID is in failedItems belongs to an
// item with missing data (the caller passes the union of episode-fetch
// failures and AniList-degraded entries), so its absence from findings is
// missing data, not evidence of alignment - it is carried forward unresolved
// (original alert time kept, no "finding resolved" line) instead of being
// falsely resolved. Pass nil when every item has complete evidence.
//
// The "findings reported" summary line's counters are defined in emitted-line
// terms: total is the input batch size, new the notifications emitted,
// resolved and preserved the two prior-finding outcomes above, and
// suppressed the remainder of the batch (total-new) - every finding that
// produced no notification, whether because a prior cycle already alerted its
// key OR because an earlier copy in this same batch carried the same key
// (in-batch duplicates are reachable; see collectCurrent).
func (n *Notifier) Notify(findings []compare.Finding, prior map[string]Alerted, failedItems map[int]struct{}, now time.Time) map[string]Alerted {
	current, migratedPrior, newCount := n.collectCurrent(findings, prior, now)

	resolved, preserved := 0, 0
	for key, a := range prior {
		if _, ok := current[key]; ok {
			continue
		}
		if _, migrated := migratedPrior[key]; migrated {
			// This prior record was carried forward under the finding's
			// canonical (folded) key, so its absence under the legacy key is
			// the migration itself, not a resolution.
			continue
		}
		if _, failed := failedItems[a.Finding.AniListID]; failed {
			current[key] = Alerted{AlertedAt: a.AlertedAt, Finding: a.Finding}
			preserved++
			continue
		}
		n.emitResolved(&a.Finding)
		resolved++
	}

	n.log.Info("findings reported",
		"total", len(findings), "new", newCount, "resolved", resolved,
		"preserved", preserved, "suppressed", len(findings)-newCount)
	return current
}

// collectCurrent builds this cycle's dedupe state from findings, emitting one
// notification per key that prior has not already alerted, and returns the
// state, the set of prior keys whose record was migrated onto a canonical key,
// and the number of newly emitted findings.
//
// Each finding's dedupe key is derived once up front (dedupeKeyWithLegacy: key
// construction is this notification boundary's own suppression policy;
// compare hands over semantic findings only). Last-payload-wins with one
// emission per key: precompute each key's final payload, then process keys in
// first-occurrence order using that payload — so the single emitted
// notification carries the same fields the stored record (and any later
// resolution line) persists, instead of a first-copy title contradicting the
// last-copy state.
//
// A finding whose assembled key crossed the aggregate bound also carries its
// pre-fold legacy key: when only the legacy key is present in prior, the
// record is migrated onto the canonical key (alert time preserved, nothing
// emitted) and the legacy key is reported back so the resolution loop does not
// read the migration as a resolution.
func (n *Notifier) collectCurrent(findings []compare.Finding, prior map[string]Alerted, now time.Time) (current map[string]Alerted, migratedPrior map[string]struct{}, newCount int) {
	current = make(map[string]Alerted, len(findings))
	migratedPrior = make(map[string]struct{})
	keys := make([]string, len(findings))
	latest := make(map[string]*compare.Finding, len(findings))
	latestLegacy := make(map[string]string, len(findings))
	for i := range findings {
		key, legacy := dedupeKeyWithLegacy(&findings[i])
		keys[i] = key
		latest[key] = &findings[i]
		latestLegacy[key] = legacy
	}
	for _, key := range keys {
		if _, ok := current[key]; ok {
			// A later copy of a key this batch already handled: the first
			// occurrence stored (and, if new, emitted) the final payload.
			continue
		}
		f := latest[key]
		at, alerted := adoptPrior(prior, migratedPrior, key, latestLegacy[key])
		if !alerted {
			n.emit(f)
			newCount++
			at = now
		}
		current[key] = Alerted{AlertedAt: at, Finding: storedFinding(f)}
	}
	return current, migratedPrior, newCount
}

// adoptPrior reports the alert time of an existing prior record for one
// finding's key — the canonical key first, then its pre-fold legacy key — so a
// key already alerted stays suppressed with its original alert time. A legacy
// key present in prior is recorded in migratedPrior either way, so the caller's
// resolution loop does not read the fold as a resolution.
func adoptPrior(prior map[string]Alerted, migratedPrior map[string]struct{}, key, legacy string) (time.Time, bool) {
	if a, ok := prior[key]; ok {
		if legacy != "" {
			if _, exists := prior[legacy]; exists {
				migratedPrior[legacy] = struct{}{}
			}
		}
		return a.AlertedAt, true
	}
	if legacy != "" {
		if a, ok := prior[legacy]; ok {
			migratedPrior[legacy] = struct{}{}
			return a.AlertedAt, true
		}
	}
	return time.Time{}, false
}

// Baseline records every current finding as already-alerted without emitting
// any, seeding the cross-cycle dedupe table on a cold start (a fresh install or
// a lost cache) so the pre-existing backlog is not dumped as a burst of
// notifications. Steady-state emission resumes on the next cycle via Notify;
// the full current picture is always available on demand through report mode.
func (n *Notifier) Baseline(findings []compare.Finding, now time.Time) map[string]Alerted {
	current := make(map[string]Alerted, len(findings))
	for i := range findings {
		f := &findings[i]
		current[dedupeKey(f)] = Alerted{AlertedAt: now, Finding: storedFinding(f)}
	}
	n.log.Info("cold start: findings baselined without notifying", "total", len(findings))
	return current
}

// --- Emission / rendering ---

// emit logs a finding at the level matching its severity, with the full field
// set the dashboard and Loki alert key on.
func (n *Notifier) emit(f *compare.Finding) {
	level := slog.LevelInfo
	if f.Severity == compare.SevWarn {
		level = slog.LevelWarn
	}
	n.log.Log(context.Background(), level, message(f.Status), findingKVs(f)...)
}

// emitResolved logs a single info line when a prior finding no longer applies,
// reading the trimmed record the dedupe state persisted. The untrusted
// upstream strings (title, groups) ride through capAttr,
// matching findingKVs' policy.
func (n *Notifier) emitResolved(f *StoredFinding) {
	n.log.Info("finding resolved",
		"title", capAttr(f.Title),
		"al_id", f.AniListID,
		"arr", f.Arr,
		"season", f.Season,
		"current_group", capAttr(f.CurrentGroup),
		"status", string(f.Status),
		"recommended_group", capAttr(f.RecommendedGroup))
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
// suppression) or amplify memory.
//
// A MULTI-SOURCE attribute (a joined group or link list) must never be
// materialized and handed to capAttr - joining first would allocate the whole
// untrusted aggregate before the bound applies. Those render through
// joinGroupsAttr / joinLinksAttr on the same joiner instead.
func capAttr(s string) string { return logattr.Cap(s) }

// findingKVs builds the structured key-value attributes for a finding line.
// It carries the arr deep-link, the split Nyaa/AnimeBytes URLs, the season, and
// a compact seadex_tags line so an alert can render a self-contained,
// clickable notification straight from the labels. Every attribute derived
// from untrusted upstream data (SeaDex/tracker titles, groups, URLs, hashes)
// is passed through capAttr: runesafe.Sanitize (the same policy the audit
// report's slog path applies, because slog's JSONHandler escapes C0 controls
// but emits C1 controls and bidi controls raw) plus a volume cap mirroring
// the bound the dedupe-key path applies to the same data. A MULTI-SOURCE
// attribute (recommended_groups, release_urls) applies that same policy
// through joinGroupsAttr / joinLinksAttr, which never materialize the
// untrusted aggregate before the cap. Fixed-pattern app values (resolution,
// codec, kind, season, al_id, arr, status) stay raw.
func findingKVs(f *compare.Finding) []any {
	nyaaURL, abURL := trackerURLs(f.Links)
	return []any{
		"title", capAttr(f.Title),
		"al_id", f.AniListID,
		"arr", f.Arr,
		"arr_url", capAttr(library.SafeLogURL(f.ArrURL)),
		"season", f.Season,
		"current_group", capAttr(f.CurrentGroup),
		"recommended_group", capAttr(f.RecommendedGroup),
		"recommended_groups", joinGroupsAttr(f.RecommendedGroups),
		"tracker", capAttr(f.Tracker),
		"resolution", f.Resolution,
		"codec", f.Codec,
		"kind", f.Kind,
		"classification_reason", capAttr(f.Reason),
		"release_url", capAttr(f.ReleaseURL),
		"release_urls", joinLinksAttr(f.Links),
		"nyaa_url", capAttr(nyaaURL),
		"ab_url", capAttr(abURL),
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
// evidence wins outright, an AB-gated (ambiguous or unclassifiable) link is
// the conservative AB fallback, a known Nyaa link is the public Nyaa source,
// and anything else is a generic public link.
func classifyTrackerLink(link compare.ReleaseLink) trackerLinkKind {
	if filter.DefinitelyAB(link.Tracker, link.URL) {
		return trackerLinkAB
	}
	if filter.ABGated(link.Tracker, link.URL) {
		return trackerLinkABFallback
	}
	if t, known := release.LookupTracker(link.Tracker); known && t.Name == release.TrackerNameNyaa {
		return trackerLinkNyaa
	}
	return trackerLinkPublic
}

// setFirst writes value into dst only when dst is still empty, preserving
// first-link-wins precedence within each slot.
func setFirst(dst *string, value string) {
	if *dst == "" {
		*dst = value
	}
}

// trackerURLs splits a finding's obtainable links into the public (Nyaa) and
// AnimeBytes URLs, so an alert can render a distinct Nyaa link and AB link.
// AB routing is URL-aware, matching the obtainability filter (this package's
// dedupe key, dedupekey.go, keys the full obtainable URL set
// label-insensitively instead): a link with definite AnimeBytes evidence
// (filter.DefinitelyAB -
// AB label or animebytes.tv URL host) wins the AB slot outright, ahead of
// ABGated's conservative fail-closed fallback. ABGated intentionally also
// reads a malformed, unparseable, or non-ASCII-host URL as AB-gated; such an
// unclassifiable link only fills the AB slot when no definite AnimeBytes
// link exists, so an ambiguous link is never rendered as the clickable
// public URL yet can never displace a genuine AB link either. The first
// non-AnimeBytes link is treated as the public/Nyaa source (Nyaa is by far
// the dominant public tracker on SeaDex).
func trackerURLs(links []compare.ReleaseLink) (nyaa, ab string) {
	var firstPublic, firstABFallback string
	for i := range links {
		switch classifyTrackerLink(links[i]) {
		case trackerLinkAB:
			setFirst(&ab, links[i].URL)
		case trackerLinkABFallback:
			setFirst(&firstABFallback, links[i].URL)
		case trackerLinkNyaa:
			setFirst(&nyaa, links[i].URL)
		case trackerLinkPublic:
			setFirst(&firstPublic, links[i].URL)
		}
	}
	setFirst(&nyaa, firstPublic)
	setFirst(&ab, firstABFallback)
	return nyaa, ab
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

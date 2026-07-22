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
// recommended_group) and Report's failed-item preservation scope, keyed on
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
// (see StoredFinding). Raw upstream strings are stored as-is: sanitization
// stays a log-time concern (emitResolved), matching the emit path's policy.
func storedFinding(f *compare.Finding) StoredFinding {
	return StoredFinding{
		Arr:              f.Arr,
		CurrentGroup:     f.CurrentGroup,
		RecommendedGroup: f.RecommendedGroup,
		Title:            f.Title,
		Status:           f.Status,
		AniListID:        f.AniListID,
		Season:           f.Season,
	}
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
func (n *Notifier) Notify(findings []compare.Finding, prior map[string]Alerted, failedItems map[int]struct{}, now time.Time) map[string]Alerted {
	current := make(map[string]Alerted, len(findings))
	newCount := 0
	// Last-payload-wins with one emission per key: precompute each DedupeKey's
	// final payload, then process keys in first-occurrence order using that
	// payload — so the single emitted notification carries the same fields the
	// stored record (and any later resolution line) persists, instead of a
	// first-copy title contradicting the last-copy state.
	latest := make(map[string]*compare.Finding, len(findings))
	for i := range findings {
		latest[findings[i].DedupeKey] = &findings[i]
	}
	for i := range findings {
		f := latest[findings[i].DedupeKey]
		if _, ok := current[f.DedupeKey]; ok {
			// A later copy of a key this batch already handled: the first
			// occurrence stored (and, if new, emitted) the final payload.
			continue
		}
		if a, ok := prior[f.DedupeKey]; ok {
			current[f.DedupeKey] = Alerted{AlertedAt: a.AlertedAt, Finding: storedFinding(f)}
			continue
		}
		n.emit(f)
		newCount++
		current[f.DedupeKey] = Alerted{AlertedAt: now, Finding: storedFinding(f)}
	}

	resolved, preserved := 0, 0
	for key, a := range prior {
		if _, ok := current[key]; ok {
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

// Baseline records every current finding as already-alerted without emitting
// any, seeding the cross-cycle dedupe table on a cold start (a fresh install or
// a lost cache) so the pre-existing backlog is not dumped as a burst of
// notifications. Steady-state emission resumes on the next cycle via Notify;
// the full current picture is always available on demand through report mode.
func (n *Notifier) Baseline(findings []compare.Finding, now time.Time) map[string]Alerted {
	current := make(map[string]Alerted, len(findings))
	for i := range findings {
		f := &findings[i]
		current[f.DedupeKey] = Alerted{AlertedAt: now, Finding: storedFinding(f)}
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
// upstream strings (title, groups) ride through runesafe.Sanitize,
// matching findingKVs' policy.
func (n *Notifier) emitResolved(f *StoredFinding) {
	n.log.Info("finding resolved",
		"title", runesafe.Sanitize(f.Title),
		"al_id", f.AniListID,
		"arr", f.Arr,
		"season", f.Season,
		"current_group", runesafe.Sanitize(f.CurrentGroup),
		"status", string(f.Status),
		"recommended_group", runesafe.Sanitize(f.RecommendedGroup))
}

// findingKVs builds the structured key-value attributes for a finding line.
// It carries the arr deep-link, the split Nyaa/AnimeBytes URLs, the season, and
// a compact seadex_tags line so an alert can render a self-contained,
// clickable notification straight from the labels. Every attribute derived
// from untrusted upstream data (SeaDex/tracker titles, groups, URLs, hashes)
// is passed through runesafe.Sanitize — the same policy the audit
// report's slog path applies — because slog's JSONHandler escapes C0 controls
// but emits C1 controls and bidi controls raw. Fixed-pattern app values
// (resolution, codec, kind, season, al_id, arr, status) stay raw.
func findingKVs(f *compare.Finding) []any {
	nyaaURL, abURL := trackerURLs(f.Links)
	return []any{
		"title", runesafe.Sanitize(f.Title),
		"al_id", f.AniListID,
		"arr", f.Arr,
		"arr_url", runesafe.Sanitize(library.SafeLogURL(f.ArrURL)),
		"season", f.Season,
		"current_group", runesafe.Sanitize(f.CurrentGroup),
		"recommended_group", runesafe.Sanitize(f.RecommendedGroup),
		"recommended_groups", runesafe.Sanitize(strings.Join(f.RecommendedGroups, ",")),
		"tracker", runesafe.Sanitize(f.Tracker),
		"resolution", f.Resolution,
		"codec", f.Codec,
		"kind", f.Kind,
		"classification_reason", runesafe.Sanitize(f.Reason),
		"release_url", runesafe.Sanitize(f.ReleaseURL),
		"release_urls", runesafe.Sanitize(joinLinks(f.Links)),
		"nyaa_url", runesafe.Sanitize(nyaaURL),
		"ab_url", runesafe.Sanitize(abURL),
		"info_hash", runesafe.Sanitize(f.InfoHash),
		"seadex_tags", seadexTags(f),
		"status", string(f.Status),
	}
}

// trackerURLs splits a finding's obtainable links into the public (Nyaa) and
// AnimeBytes URLs, so an alert can render a distinct Nyaa link and AB link.
// AB routing is URL-aware, matching the obtainability filter (compare's
// dedupe key now keys the full obtainable URL set label-insensitively
// instead): a link with definite AnimeBytes evidence (filter.DefinitelyAB -
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
		t, known := release.LookupTracker(links[i].Tracker)
		switch {
		case filter.DefinitelyAB(links[i].Tracker, links[i].URL):
			if ab == "" {
				ab = links[i].URL
			}
		case filter.ABGated(links[i].Tracker, links[i].URL):
			if firstABFallback == "" {
				firstABFallback = links[i].URL
			}
		case known && t.Name == release.TrackerNameNyaa:
			if nyaa == "" {
				nyaa = links[i].URL
			}
		case firstPublic == "":
			firstPublic = links[i].URL
		}
	}
	if nyaa == "" {
		nyaa = firstPublic
	}
	if ab == "" {
		ab = firstABFallback
	}
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

// joinLinks renders every obtainable source for the recommended release as a
// space-separated "tracker=url" list, so a finding carries both a Nyaa and an
// AnimeBytes link when the release exists on both, not just the headline one.
func joinLinks(links []compare.ReleaseLink) string {
	parts := make([]string, 0, len(links))
	for i := range links {
		parts = append(parts, links[i].Tracker+"="+links[i].URL)
	}
	return strings.Join(parts, " ")
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

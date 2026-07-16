// Package report emits findings as structured slog events with cross-cycle
// dedupe. Observability is slog-only (shipped to Loki); there is no metrics
// endpoint.
package report

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cplieger/seadex-scout/internal/compare"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/release"
)

// labelArr is the arr key shared by the finding log lines.
const labelArr = "arr"

// Alerted is a persisted dedupe record: the finding that was alerted and when,
// keyed in the state by the finding's dedupe key.
type Alerted struct {
	AlertedAt time.Time       `json:"alerted_at"`
	Finding   compare.Finding `json:"finding"`
}

// Reporter emits findings as slog events with cross-cycle dedupe.
type Reporter struct {
	log *slog.Logger
}

// storedFinding returns a copy of f safe to persist in state.json: the arr
// deep-link is sanitized the same way the log path sanitizes it, so a
// credentialed public_url never lands on disk. No consumer reads the
// persisted ArrURL, so this is behavior-preserving.
func storedFinding(f *compare.Finding) compare.Finding {
	c := *f
	c.ArrURL = library.SafeLogURL(c.ArrURL)
	return c
}

// NewReporter builds a Reporter. logger may be nil.
func NewReporter(logger *slog.Logger) *Reporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reporter{log: logger}
}

// Report emits new findings, suppresses ones already alerted (carrying their
// original alert time forward), logs a one-line resolution for any prior finding
// no longer present, and returns the new dedupe state to persist.
//
// failedItems scopes resolution on a partial library walk: a prior finding
// whose AniList ID is in failedItems belongs to an item whose episode fetch
// failed this cycle, so its absence from findings is missing data, not
// evidence of alignment - it is carried forward unresolved (original alert
// time kept, no "finding resolved" line) instead of being falsely resolved.
// Pass nil when every item walked cleanly.
func (r *Reporter) Report(findings []compare.Finding, prior map[string]Alerted, failedItems map[int]struct{}, now time.Time) map[string]Alerted {
	current := make(map[string]Alerted, len(findings))
	newCount := 0
	for i := range findings {
		f := &findings[i]
		if a, ok := prior[f.DedupeKey]; ok {
			current[f.DedupeKey] = Alerted{AlertedAt: a.AlertedAt, Finding: storedFinding(f)}
			continue
		}
		r.emit(f)
		newCount++
		current[f.DedupeKey] = Alerted{AlertedAt: now, Finding: storedFinding(f)}
	}

	resolved, preserved := 0, 0
	for key := range prior {
		if _, ok := current[key]; ok {
			continue
		}
		a := prior[key]
		if _, failed := failedItems[a.Finding.AniListID]; failed {
			current[key] = a
			preserved++
			continue
		}
		r.emitResolved(&a.Finding)
		resolved++
	}

	r.log.Info("findings reported",
		"total", len(findings), "new", newCount, "resolved", resolved,
		"preserved", preserved, "suppressed", len(findings)-newCount)
	return current
}

// Baseline records every current finding as already-alerted without emitting
// any, seeding the cross-cycle dedupe table on a cold start (a fresh install or
// a lost cache) so the pre-existing backlog is not dumped as a burst of
// notifications. Steady-state emission resumes on the next cycle via Report;
// the full current picture is always available on demand through report mode.
func (r *Reporter) Baseline(findings []compare.Finding, now time.Time) map[string]Alerted {
	current := make(map[string]Alerted, len(findings))
	for i := range findings {
		f := &findings[i]
		current[f.DedupeKey] = Alerted{AlertedAt: now, Finding: storedFinding(f)}
	}
	r.log.Info("cold start: findings baselined without notifying", "total", len(findings))
	return current
}

// emit logs a finding at the level matching its severity, with the full field
// set the dashboard and Loki alert key on.
func (r *Reporter) emit(f *compare.Finding) {
	level := slog.LevelInfo
	if f.Severity == compare.SevWarn {
		level = slog.LevelWarn
	}
	r.log.Log(context.Background(), level, message(f.Status), findingKVs(f)...)
}

// emitResolved logs a single info line when a prior finding no longer applies.
func (r *Reporter) emitResolved(f *compare.Finding) {
	r.log.Info("finding resolved",
		"title", f.Title,
		"al_id", f.AniListID,
		labelArr, f.Arr,
		"season", f.Season,
		"current_group", f.CurrentGroup,
		"status", string(f.Status),
		"recommended_group", f.RecommendedGroup)
}

// findingKVs builds the structured key-value attributes for a finding line.
// It carries the arr deep-link, the split Nyaa/AnimeBytes URLs, the season, and
// a compact seadex_tags line so an alert can render a self-contained,
// clickable notification straight from the labels.
func findingKVs(f *compare.Finding) []any {
	nyaaURL, abURL := trackerURLs(f.Links)
	return []any{
		"title", f.Title,
		"al_id", f.AniListID,
		labelArr, f.Arr,
		"arr_url", library.SafeLogURL(f.ArrURL),
		"season", f.Season,
		"current_group", f.CurrentGroup,
		"recommended_group", f.RecommendedGroup,
		"recommended_groups", strings.Join(f.RecommendedGroups, ","),
		"tracker", f.Tracker,
		"resolution", f.Resolution,
		"codec", f.Codec,
		"kind", f.Kind,
		"classification_reason", f.Reason,
		"release_url", f.ReleaseURL,
		"release_urls", joinLinks(f.Links),
		"nyaa_url", nyaaURL,
		"ab_url", abURL,
		"info_hash", f.InfoHash,
		"seadex_tags", seadexTags(f),
		"status", string(f.Status),
	}
}

// trackerURLs splits a finding's obtainable links into the public (Nyaa) and
// AnimeBytes URLs, so an alert can render a distinct Nyaa link and AB link. The
// first non-AnimeBytes link is treated as the public/Nyaa source (Nyaa is by
// far the dominant public tracker on SeaDex).
func trackerURLs(links []compare.ReleaseLink) (nyaa, ab string) {
	var firstPublic string
	for i := range links {
		t, known := release.LookupTracker(links[i].Tracker)
		switch {
		case known && t.Name == release.TrackerNameAnimeBytes:
			if ab == "" {
				ab = links[i].URL
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
	return nyaa, ab
}

// seadexTags renders a compact descriptive tag line for a finding — the SeaDex
// qualifier (best / incomplete / theoretical-best / mixed-group), the release
// kind, resolution, and dual-audio — for an alert footer. Only best releases
// are ever surfaced, so "alt" never appears.
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
	default:
		return "seadex finding"
	}
}

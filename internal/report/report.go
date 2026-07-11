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
func (r *Reporter) Report(findings []compare.Finding, prior map[string]Alerted, now time.Time) map[string]Alerted {
	current := make(map[string]Alerted, len(findings))
	newCount := 0
	for i := range findings {
		f := &findings[i]
		if a, ok := prior[f.DedupeKey]; ok {
			current[f.DedupeKey] = Alerted{AlertedAt: a.AlertedAt, Finding: *f}
			continue
		}
		r.emit(f)
		newCount++
		current[f.DedupeKey] = Alerted{AlertedAt: now, Finding: *f}
	}

	resolved := 0
	for key := range prior {
		if _, ok := current[key]; !ok {
			f := prior[key].Finding
			r.emitResolved(&f)
			resolved++
		}
	}

	r.log.Info("findings reported",
		"total", len(findings), "new", newCount, "resolved", resolved, "suppressed", len(findings)-newCount)
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
		"status", string(f.Status),
		"recommended_group", f.RecommendedGroup)
}

// findingKVs builds the structured key-value attributes for a finding line.
func findingKVs(f *compare.Finding) []any {
	return []any{
		"title", f.Title,
		"al_id", f.AniListID,
		labelArr, f.Arr,
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
		"info_hash", f.InfoHash,
		"status", string(f.Status),
	}
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

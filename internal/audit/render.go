package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/library"
)

const (
	reportDirMode  = 0o755
	reportFileMode = 0o644
	// linkSep joins the links within a table cell (a middle dot, not an em dash).
	linkSep = " \u00b7 "
	// emptyCell is shown for a column with no value.
	emptyCell = "-"
)

// verdictDesc is the one-line explanation shown under each verdict section.
var verdictDesc = map[Verdict]string{
	VerdictUnlisted:    "You have a release SeaDex does not list as best or alt.",
	VerdictAlt:         "You have a listed alt; SeaDex marks a different release best.",
	VerdictUnverified:  "Files are present but no release group could be identified, so no comparison was possible.",
	VerdictNoFile:      "The mapped season or movie has no file on disk.",
	VerdictBest:        "You already have SeaDex's best release.",
	VerdictNotOnSeaDex: "In your library and recognized as anime (Fribb-mapped) but SeaDex lists no entry, so there is no recommendation to compare against.",
}

// RenderJSON renders the report as indented JSON (the machine-ingestible copy).
func RenderJSON(r *Report) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// RenderMarkdown renders the report as human-readable Markdown, grouped into a
// section per verdict (most actionable first) with a compact links column.
func RenderMarkdown(r *Report) string {
	var b strings.Builder
	b.WriteString("# SeaDex alignment report\n\n")
	notOnSeaDex := r.Totals[string(VerdictNotOnSeaDex)]
	matched := len(r.Rows) - notOnSeaDex
	fmt.Fprintf(&b, "Generated %s. %d anime with a SeaDex match",
		r.GeneratedAt.UTC().Format(time.RFC3339), matched)
	if notOnSeaDex > 0 {
		fmt.Fprintf(&b, "; %d more in your library that SeaDex does not list", notOnSeaDex)
	}
	b.WriteString(".\n\n")

	b.WriteString("## Summary\n\n| Verdict | Count |\n| --- | --- |\n")
	for _, v := range verdictOrder {
		fmt.Fprintf(&b, "| %s | %d |\n", v, r.Totals[string(v)])
	}
	b.WriteByte('\n')

	for _, v := range verdictOrder {
		rows := rowsWithVerdict(r.Rows, v)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s (%d)\n\n", v, len(rows))
		if desc := verdictDesc[v]; desc != "" {
			fmt.Fprintf(&b, "%s\n\n", desc)
		}
		b.WriteString("| Title | Scope | Your group | SeaDex best | Links |\n")
		b.WriteString("| --- | --- | --- | --- | --- |\n")
		for i := range rows {
			writeRow(&b, &rows[i])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// writeRow writes one Markdown table row for a report row.
func writeRow(b *strings.Builder, row *Row) {
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
		escapeCell(row.Title),
		scopeCell(row),
		escapeCell(orEmpty(strings.Join(row.CurrentGroups, ", "))),
		escapeCell(orEmpty(strings.Join(displayBestGroups(row.Releases), ", "))),
		links(row))
}

// Log emits the report to slog: a summary line then one INFO line per row, so
// the report is queryable in Loki alongside the human-readable Markdown.
func (r *Report) Log(log *slog.Logger) {
	log.Info("report generated",
		"rows", len(r.Rows),
		"have_best", r.Totals[string(VerdictBest)],
		"have_alt", r.Totals[string(VerdictAlt)],
		"have_unlisted", r.Totals[string(VerdictUnlisted)],
		"no_file", r.Totals[string(VerdictNoFile)],
		"unverified", r.Totals[string(VerdictUnverified)],
		"not_on_seadex", r.Totals[string(VerdictNotOnSeaDex)])
	for i := range r.Rows {
		row := &r.Rows[i]
		log.Info("report item",
			"title", row.Title,
			"al_id", row.AniListID,
			"arr", row.Arr,
			"verdict", string(row.Verdict),
			"scope", scopeLabel(row),
			"approx", row.Approx,
			"current_group", strings.Join(row.CurrentGroups, ","),
			"seadex_best", strings.Join(displayBestGroups(row.Releases), ","),
			"arr_url", row.ArrURL,
			"seadex_url", row.SeaDexURL,
			"match_source", row.MatchSource)
	}
}

// reportStampLayout is the UTC timestamp embedded in report filenames: sortable,
// filesystem-safe (no colons), second precision.
const reportStampLayout = "2006-01-02T15-04-05Z"

// WriteFiles renders the report and atomically writes a timestamped Markdown +
// JSON pair into dir (report-<UTC timestamp>.md and .json), creating dir as
// needed. The timestamp (the report's GeneratedAt) keeps successive reports
// from overwriting one another.
func (r *Report) WriteFiles(ctx context.Context, dir string, log *slog.Logger) error {
	base := filepath.Join(dir, "report-"+r.GeneratedAt.UTC().Format(reportStampLayout))
	mdPath, jsonPath := base+".md", base+".json"
	if err := writeAtomic(ctx, mdPath, []byte(RenderMarkdown(r)), log); err != nil {
		return fmt.Errorf("audit: write markdown %s: %w", mdPath, err)
	}
	data, err := RenderJSON(r)
	if err != nil {
		return fmt.Errorf("audit: encode json: %w", err)
	}
	if err := writeAtomic(ctx, jsonPath, data, log); err != nil {
		return fmt.Errorf("audit: write json %s: %w", jsonPath, err)
	}
	log.Info("report written", "markdown", mdPath, "json", jsonPath, "anime", len(r.Rows))
	return nil
}

// writeAtomic writes data to path atomically, warning (not failing) on a
// non-durable write, matching the state store's policy.
func writeAtomic(ctx context.Context, path string, data []byte, log *slog.Logger) error {
	res, err := atomicfile.WriteFile(ctx, path, data,
		atomicfile.WithMkdirMode(reportDirMode),
		atomicfile.WithMode(reportFileMode))
	if err != nil {
		return err
	}
	if !res.Durable {
		log.Warn("report written but not durable", "path", path)
	}
	return nil
}

// scopeCell renders the scope for the Markdown table, appending "(approx)" when
// the comparison used a coarse multi-group bucket.
func scopeCell(row *Row) string {
	if row.Approx {
		return scopeLabel(row) + " (approx)"
	}
	return scopeLabel(row)
}

// scopeLabel renders the comparison scope: "movie", "special", the TVDB season
// ("S2"), or "series" for a whole-series comparison (an absolute-numbered run, a
// title-only match, or a not-on-SeaDex library item).
func scopeLabel(row *Row) string {
	switch {
	case row.Arr == library.ArrRadarr:
		return "movie"
	case row.Special:
		return "special"
	case row.Season > 0:
		return "S" + strconv.Itoa(row.Season)
	default:
		return "series"
	}
}

// links builds the compact links cell: the arr deep-link, the SeaDex entry, and
// each distinct best-release indexer link.
func links(row *Row) string {
	var parts []string
	if row.ArrURL != "" {
		parts = append(parts, mdLink(row.Arr, row.ArrURL))
	}
	if row.SeaDexURL != "" {
		parts = append(parts, mdLink("seadex", row.SeaDexURL))
	}
	seen := make(map[string]struct{}, len(row.Releases))
	for i := range row.Releases {
		rel := &row.Releases[i]
		if !rel.Best || rel.URL == "" {
			continue
		}
		key := rel.Tracker + "|" + rel.URL
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		parts = append(parts, mdLink(orTracker(rel.Tracker), rel.URL))
	}
	if len(parts) == 0 {
		return emptyCell
	}
	return strings.Join(parts, linkSep)
}

// escapeLinkURL percent-encodes the characters in a URL that would break out
// of a Markdown link's ](...) destination or the surrounding table cell/row:
// parentheses, angle brackets, pipes, and every ASCII whitespace form (space,
// tab, vertical tab, form feed, CR, LF). An ordinary URL is unchanged.
func escapeLinkURL(u string) string {
	r := strings.NewReplacer(
		" ", "%20",
		"\t", "%09",
		"\v", "%0B",
		"\f", "%0C",
		"(", "%28",
		")", "%29",
		"<", "%3C",
		">", "%3E",
		"|", "%7C",
		"\n", "%0A",
		"\r", "%0D",
	)
	return r.Replace(u)
}

// mdLink builds a Markdown link with a table-cell-safe label and a
// metacharacter-escaped destination. It emits a link only when the destination
// parses as an http/https URL; any other scheme (javascript:, data:, …) or an
// unparseable destination degrades to the escaped label as plain text, so an
// untrusted tracker URL cannot inject an active non-http link.
func mdLink(label, rawURL string) string {
	safeLabel := escapeCell(label)
	trimmed := strings.TrimSpace(rawURL)
	if u, err := url.Parse(trimmed); err == nil {
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			return "[" + safeLabel + "](" + escapeLinkURL(trimmed) + ")"
		}
	}
	return safeLabel
}

// displayBestGroups returns the distinct best-release groups in their original
// case (deduped case-insensitively), for display.
func displayBestGroups(releases []Release) []string {
	var out []string
	seen := make(map[string]struct{}, len(releases))
	for i := range releases {
		g := releases[i].Group
		if !releases[i].Best || g == "" {
			continue
		}
		key := strings.ToLower(g)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, g)
	}
	return out
}

// rowsWithVerdict returns the rows carrying verdict v, preserving order.
func rowsWithVerdict(rows []Row, v Verdict) []Row {
	var out []Row
	for i := range rows {
		if rows[i].Verdict == v {
			out = append(out, rows[i])
		}
	}
	return out
}

// escapeCell makes a string safe inside a Markdown table cell. It uses HTML
// numeric/character entities instead of backslash escapes so a pre-existing
// backslash in the text cannot cancel an inserted escape (\] or \| could
// otherwise break out of a link label or table cell). It neutralizes the raw
// HTML metacharacters (& < >) so untrusted text such as <img ...> cannot
// survive as raw Markdown HTML, encodes the table/link delimiters (| [ ]) and
// the backslash itself, and flattens CR/LF. strings.NewReplacer performs a
// single non-overlapping left-to-right pass and never re-scans its replacement
// output, so encoding & first does not double-encode the entities it inserts.
func escapeCell(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\\", "&#92;",
		"|", "&#124;",
		"[", "&#91;",
		"]", "&#93;",
		"\n", " ",
		"\r", " ",
	)
	return r.Replace(s)
}

// orEmpty returns the empty-cell marker for a blank string.
func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return emptyCell
	}
	return s
}

// orTracker labels a link by tracker, falling back to "link" for an unnamed one.
func orTracker(tracker string) string {
	if strings.TrimSpace(tracker) == "" {
		return "link"
	}
	return tracker
}

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/seadex-scout/internal/align"
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
	VerdictNoFile:      "The mapped season or movie has no file on disk, or a whole-series comparison found no real season with files.",
	VerdictBest:        "You already have SeaDex's best release.",
	VerdictNotOnSeaDex: "In your library and recognized as anime (Fribb-mapped) but SeaDex lists no entry, so there is no recommendation to compare against.",
}

// renderJSON renders the report as indented JSON (the machine-ingestible copy).
func renderJSON(r *Report) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// renderMarkdown renders the report as human-readable Markdown, grouped into a
// section per verdict (most actionable first) with a compact links column.
func renderMarkdown(r *Report) string {
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
// the report is queryable in Loki alongside the human-readable Markdown. The
// summary's msg is "report summary", deliberately distinct from Scout.Report's
// "report generated" completion line, so a Loki query or counter keyed on
// either message never double-counts a report run.
func (r *Report) Log(log *slog.Logger) {
	stamp := r.GeneratedAt.UTC().Format(time.RFC3339)
	log.Info("report summary",
		"generated_at", stamp,
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
			"generated_at", stamp,
			"title", row.Title,
			"al_id", row.AniListID,
			"arr", row.Arr,
			"verdict", string(row.Verdict),
			"qualifier", string(row.Qualifier),
			"scope", scopeLabel(row),
			"approx", row.Approx,
			"current_group", strings.Join(row.CurrentGroups, ","),
			"seadex_best", strings.Join(displayBestGroups(row.Releases), ","),
			"arr_url", library.SafeLogURL(row.ArrURL),
			"seadex_url", row.SeaDexURL,
			"match_source", row.MatchSource)
	}
}

// reportStampLayout is the UTC timestamp embedded in report filenames: sortable,
// filesystem-safe (no colons), second precision.
const reportStampLayout = "2006-01-02T15-04-05Z"

// reportLockName is the flock target inside the report dir that serializes
// report runs (see AcquireReportLock).
const reportLockName = "report.lock"

// ErrReportRunning is returned by AcquireReportLock when another report run
// already holds the report lock. The report subcommand refuses to run rather
// than racing the other run onto the same timestamped filename pair.
var ErrReportRunning = errors.New("another report is already running")

// AcquireReportLock takes an exclusive, non-blocking flock on report.lock in
// dir (creating dir as needed) and returns a release func. It is held for a
// report run's whole generate+write, so two concurrent report runs - which
// could finish within the same UTC second and target the same
// report-<timestamp>.{md,json} pair - cannot interleave: the second run gets
// ErrReportRunning and refuses (never blocks or waits). A strictly-sequential
// same-second rerun overwriting the same pair is accepted by design (the same
// GeneratedAt second means the same content basis). The lock file is left in
// place on release; unlinking it would open a window where two runs flock
// different inodes and both proceed.
func AcquireReportLock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, reportDirMode); err != nil {
		return nil, fmt.Errorf("audit: create report dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, reportLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, reportFileMode)
	if err != nil {
		return nil, fmt.Errorf("audit: open report lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrReportRunning
		}
		return nil, fmt.Errorf("audit: lock %s: %w", path, err)
	}
	// Closing the file releases the flock; the closure also keeps f reachable
	// so a finalizer cannot close the descriptor (and drop the lock) early.
	return func() { _ = f.Close() }, nil
}

// WriteFiles renders the report and atomically writes a timestamped JSON +
// Markdown pair into dir (report-<UTC timestamp>.json and .md), creating dir
// as needed. The timestamp (the report's GeneratedAt) keeps successive reports
// from overwriting one another.
func (r *Report) WriteFiles(ctx context.Context, dir string, log *slog.Logger) error {
	base := filepath.Join(dir, "report-"+r.GeneratedAt.UTC().Format(reportStampLayout))
	mdPath, jsonPath := base+".md", base+".json"
	// The JSON half is written FIRST, deliberately: a run interrupted between
	// the two writes can leave a .json without its .md, but never a dangling
	// .md without its machine-readable pair.
	data, err := renderJSON(r)
	if err != nil {
		return fmt.Errorf("audit: encode json: %w", err)
	}
	if err := writeAtomic(ctx, jsonPath, data, log); err != nil {
		return fmt.Errorf("audit: write json %s: %w", jsonPath, err)
	}
	if err := writeAtomic(ctx, mdPath, []byte(renderMarkdown(r)), log); err != nil {
		return fmt.Errorf("audit: write markdown %s: %w", mdPath, err)
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

// scopeCell renders the scope for the Markdown table, appending the comparison
// annotations in parentheses: "approx" when the comparison used a coarse
// multi-group bucket, and the daemon-vocabulary qualifier
// (mixed/theoretical/incomplete) when one applies - e.g. "S2 (approx, mixed)".
func scopeCell(row *Row) string {
	var notes []string
	if row.Approx {
		notes = append(notes, "approx")
	}
	if row.Qualifier != "" {
		notes = append(notes, string(row.Qualifier))
	}
	if len(notes) == 0 {
		return scopeLabel(row)
	}
	return scopeLabel(row) + " (" + strings.Join(notes, ", ") + ")"
}

// scopeLabel renders the comparison scope recorded on the row at build time:
// "movie", "special", the TVDB season ("S2"), or "series" for a whole-series
// comparison (an absolute-numbered run, a title-only match, or a not-on-SeaDex
// library item). It is a pure reader of Row.scope — the classification itself
// is the align.Scope decision recorded on the Row, so the label cannot drift
// from the comparison actually performed.
func scopeLabel(row *Row) string {
	switch row.scope {
	case align.ScopeMovie:
		return "movie"
	case align.ScopeSeason:
		return "S" + strconv.Itoa(row.Season)
	case align.ScopeSpecial:
		return "special"
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

// linkURLEscaper backs escapeLinkURL; built once, safe for concurrent use.
var linkURLEscaper = strings.NewReplacer(
	" ", "%20",
	"\t", "%09",
	"\\", "%5C",
	"`", "%60",
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

// escapeLinkURL percent-encodes the characters in a URL that would break out
// of a Markdown link's ](...) destination or the surrounding table cell/row:
// parentheses, angle brackets, pipes, backslash and backtick (the CommonMark
// inline metacharacters still active inside a link destination), and every
// ASCII whitespace form (space, tab, vertical tab, form feed, CR, LF). It also
// percent-encodes the non-ASCII control ranges url.Parse accepts but a
// terminal or Markdown viewer must never receive raw: C1 controls
// (U+0080-U+009F, terminal-escape introducers), the Unicode bidi
// override/isolate runes (U+202A-U+202E, U+2066-U+2069, visual reordering of
// the rendered links cell), and the U+2028/U+2029 line separators.
// Percent-encoding is semantically transparent for a URL, so an ordinary
// destination is unchanged.
func escapeLinkURL(u string) string {
	u = linkURLEscaper.Replace(u)
	var b strings.Builder
	for _, r := range u {
		switch {
		case (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) ||
			(r >= 0x2066 && r <= 0x2069) || r == 0x2028 || r == 0x2029:
			for _, byt := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", byt)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
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

// cellEscaper backs escapeCell; built once, safe for concurrent use.
var cellEscaper = strings.NewReplacer(
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

// stripControl replaces C0 control characters, DEL, the C1 control range
// (U+0080-U+009F, single-rune terminal-escape introducers), and the Unicode
// bidirectional override/isolate characters with a space, so untrusted
// text cannot smuggle terminal escape sequences or visual reordering
// into the rendered Markdown report. CR/LF are already flattened by
// cellEscaper; this catches the rest.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 && r != '\n' && r != '\r':
			return ' '
		case r == 0x7f:
			return ' '
		case r >= 0x80 && r <= 0x9f: // C1 controls (CSI U+009B, OSC U+009D, DCS U+0090): single-rune terminal-escape introducers some UTF-8 terminals honor
			return ' '
		case r >= 0x202a && r <= 0x202e: // LRE/RLE/PDF/LRO/RLO
			return ' '
		case r >= 0x2066 && r <= 0x2069: // LRI/RLI/FSI/PDI
			return ' '
		}
		return r
	}, s)
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
// A stripControl pre-pass removes the remaining C0/DEL/C1 control characters
// and the Unicode bidi override/isolate characters (terminal-escape and visual
// reordering smuggling).
func escapeCell(s string) string {
	return cellEscaper.Replace(stripControl(s))
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

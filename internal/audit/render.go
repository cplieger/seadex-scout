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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/scheduler/v2"
	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
)

const (
	// Reports enumerate the operator's library and can carry private-tracker
	// page links, so newly created report directories and every written
	// report pair are owner-only (least privilege, CWE-732): another local
	// account able to traverse the bind-mounted config tree must not read the
	// inventory. Neither MkdirAll nor an atomic replacement retightens what
	// already exists on disk - the README's upgrade note covers historical
	// reports (`chmod -R go-rwx /config/reports`).
	reportDirMode  = 0o700
	reportFileMode = 0o600
	// linkSep joins the links within a table cell (a middle dot, not an em dash).
	linkSep = " \u00b7 "
	// emptyCell is shown for a column with no value.
	emptyCell = "-"
)

// --- Markdown + JSON rendering ---

// verdictDesc is the one-line explanation shown under each verdict section.
var verdictDesc = map[Verdict]string{
	VerdictUnlisted:    "You have a release SeaDex does not list as best or alt.",
	VerdictAlt:         "You have a listed alt; SeaDex marks a different release best.",
	VerdictUnverified:  "The release-group evidence is unknown on one side (an unidentifiable file or an untagged SeaDex release), so alignment could not be verified.",
	VerdictNoFile:      "The mapped season, movie, or specials bucket has no file on disk, or a whole-series comparison found no real season with files.",
	VerdictBest:        "You already have SeaDex's best release.",
	VerdictNotOnSeaDex: "In your library and recognized as anime (Fribb-mapped) but SeaDex lists no entry, so there is no recommendation to compare against.",
}

// renderJSON renders the report as indented JSON (the machine-ingestible copy).
// It serializes a sanitized copy (sanitizeOutput) rather than the canonical
// Report: encoding/json escapes C0 controls but passes C1 controls (CSI/OSC/ST
// terminal-escape introducers) and Unicode bidi controls through as raw UTF-8,
// which a terminal viewing the file could honor.
func renderJSON(r *Report) ([]byte, error) {
	return json.MarshalIndent(sanitizeOutput(r), "", "  ")
}

// renderMarkdown renders the report as human-readable Markdown, grouped into a
// section per verdict (most actionable first) with a compact links column. A
// degraded run additionally carries the completeness caveat in the header and
// the incomplete-mapping section after the verdict sections; a fully resolved
// run renders byte-identically to the pre-caveat format.
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
	writeIncompleteCaveat(&b, len(r.Incomplete))

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
	writeIncompleteSection(&b, r.Incomplete)
	return b.String()
}

// incompleteHeader is the incomplete-mapping section's Markdown heading text,
// also named by the header caveat so a reader can find the section.
const incompleteHeader = "incomplete (transient AniList failure)"

// writeIncompleteCaveat states the completeness caveat in the report header
// when the run left SeaDex entries unmapped, so a reader cannot take a
// degraded report for a complete audit. Silent on a fully resolved run.
func writeIncompleteCaveat(b *strings.Builder, n int) {
	if n == 0 {
		return
	}
	noun := "entries"
	if n == 1 {
		noun = "entry"
	}
	fmt.Fprintf(b, "**Caveat: this report is incomplete.** %d SeaDex %s could not be mapped to the library this run because of a transient AniList failure; the affected rows may be missing or misfiled. See the %q section below.\n\n",
		n, noun, incompleteHeader)
}

// writeIncompleteSection renders the incomplete-mapping section: one row per
// SeaDex entry whose library mapping could not be resolved this run, listed by
// AniList id with its releases.moe link. Omitted entirely on a fully resolved
// run (matching the JSON key's omitempty), so a total AniList outage that
// affected no entry - or a healthy run - renders no section.
func writeIncompleteSection(b *strings.Builder, incomplete []IncompleteEntry) {
	if len(incomplete) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s (%d)\n\n", incompleteHeader, len(incomplete))
	b.WriteString("These SeaDex entries could not be mapped to the library this run: the AniList lookup that would link them failed transiently. Whether they align is unknown; re-run the report once AniList recovers.\n\n")
	b.WriteString("| AniList ID | SeaDex |\n| --- | --- |\n")
	for i := range incomplete {
		fmt.Fprintf(b, "| %d | %s |\n", incomplete[i].AniListID, mdLink("seadex", incomplete[i].SeaDexURL))
	}
	b.WriteByte('\n')
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

// releaseLinkKey is the structural dedupe identity for a links-cell entry.
// Deduping on a comparable tuple (not a delimiter-joined string) means a
// crafted tracker or URL containing the would-be delimiter cannot collide two
// distinct (tracker, URL) pairs and silently drop a best-release link.
type releaseLinkKey struct {
	tracker, url string
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
	seen := make(map[releaseLinkKey]struct{}, len(row.Releases))
	for i := range row.Releases {
		rel := &row.Releases[i]
		// A curation-warned or unobtainable best is not offered as a grab
		// link: the links cell is an action affordance, and either SeaDex's
		// own curators warn against the release or the daemon's obtainability
		// rule says the operator cannot get it (it is annotated in the
		// SeaDex-best column instead; the daemon and the Torznab feed exclude
		// both the same way).
		if !rel.Best || rel.URL == "" || len(rel.Warnings) > 0 || rel.Unobtainable {
			continue
		}
		key := releaseLinkKey{tracker: rel.Tracker, url: rel.URL}
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

// displayBestGroups returns the distinct best-release groups in their original
// case (deduped case-insensitively), for display. An annotated best - one
// carrying curation warnings, one the daemon's obtainability rule rejected
// (Release.Unobtainable), or both - renders with its notes: "PMR (broken)",
// "PMR (unobtainable)", "SEV (broken, unobtainable)". The column stays
// complete (the report shows raw SeaDex data) while explaining why the
// verdict did not count the release. Clean bests are collected first and win
// the dedupe, so a group genuinely available as a clean best never displays
// annotated.
func displayBestGroups(releases []Release) []string {
	var out []string
	seen := make(map[string]struct{}, len(releases))
	for _, annotatedPass := range []bool{false, true} {
		for i := range releases {
			rel := &releases[i]
			notes := releaseNotes(rel)
			if !rel.Best || rel.Group == "" || (len(notes) > 0) != annotatedPass {
				continue
			}
			key := strings.ToLower(rel.Group)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			label := rel.Group
			if annotatedPass {
				label += " (" + strings.Join(notes, ", ") + ")"
			}
			out = append(out, label)
		}
	}
	return out
}

// releaseNotes returns a release's display annotations: its canonical
// curation-warning tags, plus "unobtainable" when the daemon's obtainability
// rule (filter.Obtainable, computed in classifyReleases) rejected it as
// verdict evidence. The returned slice is always a fresh allocation, so
// callers can append without aliasing Release.Warnings.
func releaseNotes(rel *Release) []string {
	notes := append([]string(nil), rel.Warnings...)
	if rel.Unobtainable {
		notes = append(notes, "unobtainable")
	}
	return notes
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

// --- slog emission ---

// Log emits the report to slog: a summary line then one INFO line per row, so
// the report is queryable in Loki alongside the human-readable Markdown. The
// summary's msg is "report summary", deliberately distinct from Scout.Report's
// "report generated" completion line, so a Loki query or counter keyed on
// either message never double-counts a report run. Cancellation is observed
// between row records (the signal context is one report-wide budget), so a
// shutdown does not spend its grace period synchronously emitting hundreds of
// row lines; the returned error wraps context.Cause, keeping a routine SIGTERM
// off main's ERROR alert. Cancellation is also checked before the summary
// line, so a shutdown that lands before Log is called never emits a
// complete-looking summary with no rows behind it. Every row-derived string is
// passed through sanitizeDisplayText (after URL redaction where applicable):
// slog's JSONHandler escapes C0 controls but emits C1 controls and bidi
// controls raw, so untrusted titles/groups/tracker strings could otherwise
// smuggle terminal escapes or visual reordering into raw log/Loki views.
func (r *Report) Log(ctx context.Context, log *slog.Logger) error {
	if err := interrupted(ctx, "report log"); err != nil {
		return err
	}
	stamp := r.GeneratedAt.UTC().Format(time.RFC3339)
	log.Info("report summary",
		"generated_at", stamp,
		"rows", len(r.Rows),
		"have_best", r.Totals[string(VerdictBest)],
		"have_alt", r.Totals[string(VerdictAlt)],
		"have_unlisted", r.Totals[string(VerdictUnlisted)],
		"no_file", r.Totals[string(VerdictNoFile)],
		"unverified", r.Totals[string(VerdictUnverified)],
		"not_on_seadex", r.Totals[string(VerdictNotOnSeaDex)],
		"incomplete_mappings", len(r.Incomplete))
	for i := range r.Rows {
		if err := interrupted(ctx, "report log"); err != nil {
			return err
		}
		row := &r.Rows[i]
		log.Info("report item",
			"generated_at", stamp,
			"title", sanitizeDisplayText(row.Title),
			"al_id", row.AniListID,
			"arr", sanitizeDisplayText(row.Arr),
			"verdict", string(row.Verdict),
			"qualifier", string(row.Qualifier),
			"scope", scopeLabel(row),
			"approx", row.Approx,
			"current_group", sanitizeDisplayText(strings.Join(row.CurrentGroups, ",")),
			"seadex_best", sanitizeDisplayText(strings.Join(displayBestGroups(row.Releases), ",")),
			"arr_url", sanitizeDisplayText(library.SafeLogURL(row.ArrURL)),
			"seadex_url", sanitizeDisplayText(row.SeaDexURL),
			"match_source", sanitizeDisplayText(row.MatchSource))
	}
	return nil
}

// interrupted maps a done context to the audit-interrupted error for stage,
// wrapping ctx.Err() as the classification token main's shutdown handling
// keys on (errors.Is context.Canceled, keeping a routine SIGTERM off the
// ERROR alert) plus the signal cause for display. It returns nil while the
// context is live, so callers can gate each stage of the report's
// log/persist pipeline on the one report-wide budget.
func interrupted(ctx context.Context, stage string) error {
	if ctx.Err() == nil {
		return nil
	}
	return fmt.Errorf("audit: %s interrupted: %w (cause: %w)", stage, ctx.Err(), context.Cause(ctx))
}

// --- File persistence + report lock ---

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
// same-second rerun does not overwrite either: WriteFiles probes a
// deterministic -2/-3/... suffix for its pair stem while the lock is held
// (see reportPairStem). The flock rides scheduler.TryLock: not-acquired is
// reported without error (mapped to ErrReportRunning here), the kernel
// releases the lock if the process dies (no stale-lock state), and the lock
// file is left in place on release (unlinking it would open a window where
// two runs flock different inodes and both proceed) holding only the current
// holder's acquisition timestamp.
// Errors are stage-plus-redacted-cause only (redactPathErr): dir is the
// secret-capable report.dir config value and these errors reach main's log.
func AcquireReportLock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, reportDirMode); err != nil {
		return nil, fmt.Errorf("audit: create report dir: %w", redactPathErr(dir, err))
	}
	path := filepath.Join(dir, reportLockName)
	lock, ok, err := scheduler.TryLock(path)
	if err != nil {
		return nil, fmt.Errorf("audit: report lock %s: %w", reportLockName, redactPathErr(dir, err))
	}
	if !ok {
		return nil, ErrReportRunning
	}
	return lock.Unlock, nil
}

// WriteFiles renders the report and atomically writes a timestamped JSON +
// Markdown pair into dir (report-<UTC timestamp>.json and .md), creating dir
// as needed. The timestamp (the report's GeneratedAt) keeps successive reports
// from overwriting one another; when a same-second pair already exists (a
// duplicate or rapidly repeated scheduler invocation), a deterministic
// -2/-3/... suffix is probed (reportPairStem) so the earlier report is never
// silently replaced. The caller holds the report lock across the whole
// generate+write, so the probe cannot race a concurrent writer.
func (r *Report) WriteFiles(ctx context.Context, dir string, log *slog.Logger) error {
	// dir is the secret-capable report.dir config value: every slog record
	// below (including atomicfile's own WithLogger diagnostics) rides the
	// redacting logger, and every returned error carries only the stage plus
	// a redacted cause, so the expanded value never reaches Loki or main's
	// error log. Filesystem calls keep the real path.
	log = redactingLogger(log, dir)
	// The signal context is one report-wide budget: check it before each
	// stage (cleanup, stem probing, rendering, the two writes) so a shutdown
	// stops the pipeline instead of spending its grace period on CPU-bound
	// work whose final atomic write would fail with context canceled anyway.
	if err := interrupted(ctx, "report write"); err != nil {
		return err
	}
	// Reap stale atomicfile temps first: a crash (SIGKILL/OOM/power loss)
	// between temp create and rename orphans a .atomicfile-<digits>.tmp in
	// the report dir forever otherwise. The caller holds report.lock, so no
	// concurrent report writer owns an in-flight temp, and CleanupStaleTemps
	// matches only the exact temp-name convention - never a report file.
	// WithLogger keeps the library's own diagnostics (including its one
	// removed-stale-temps INFO) on the report logger; only the top-level
	// readdir failure is unlogged by the library, so that WARN stays here (a
	// not-yet-created dir is skipped silently: WriteFiles creates it at write
	// time and it holds no temps to reap).
	if _, err := atomicfile.CleanupStaleTemps(dir, time.Hour, atomicfile.WithLogger(log)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// No dir attribute: the redacting logger would mask it anyway, and
		// the fixed message already identifies the location as report.dir.
		log.Warn("stale report temp cleanup failed", "error", err)
	}
	base, err := reportPairStem(ctx, dir, r.GeneratedAt)
	if err != nil {
		return err
	}
	mdPath, jsonPath := base+".md", base+".json"
	if interruptErr := interrupted(ctx, "report render"); interruptErr != nil {
		return interruptErr
	}
	// Render from a credential-redacted copy: report rows carry ArrURLs from
	// the raw library snapshot, so a credentialed public_url (userinfo, query
	// token) would otherwise persist verbatim into the report pair even
	// though the state and slog paths strip the same values. Redacting at the
	// persistence sink covers every caller.
	safe := redactReportURLs(r)
	// The JSON half is written FIRST, deliberately: a run interrupted between
	// the two writes can leave a .json without its .md, but never a dangling
	// .md without its machine-readable pair.
	data, err := renderJSON(safe)
	if err != nil {
		return fmt.Errorf("audit: encode json: %w", err)
	}
	if err := writeAtomic(ctx, jsonPath, data, log); err != nil {
		return fmt.Errorf("audit: write json %s: %w", filepath.Base(jsonPath), redactPathErr(dir, err))
	}
	if err := interrupted(ctx, "report markdown render"); err != nil {
		return err
	}
	if err := writeAtomic(ctx, mdPath, []byte(renderMarkdown(safe)), log); err != nil {
		return fmt.Errorf("audit: write markdown %s: %w", filepath.Base(mdPath), redactPathErr(dir, err))
	}
	// Basenames only: the stem is timestamp-derived (never dir-derived), so
	// the success record stays useful without shipping the directory value.
	log.Info("report written", "markdown", filepath.Base(mdPath), "json", filepath.Base(jsonPath), "anime", len(r.Rows))
	return nil
}

// redactReportURLs returns a shallow copy of the report whose rows carry
// credential-free ArrURLs (library.SafeLogURL strips userinfo, query, and
// fragment), so a credentialed arr public_url never lands in the persisted
// report files. The canonical Report is never mutated: the row slice is
// cloned before the URLs are replaced.
func redactReportURLs(r *Report) *Report {
	out := *r
	out.Rows = slices.Clone(r.Rows)
	for i := range out.Rows {
		out.Rows[i].ArrURL = library.SafeLogURL(out.Rows[i].ArrURL)
	}
	return &out
}

// reportPairStem selects a collision-free filename stem for the report pair:
// the second-precision GeneratedAt stem when neither half exists, otherwise
// the first deterministic "-N" suffix (N >= 2) where both the .json and .md
// halves are free. A non-NotExist stat error is surfaced rather than risking
// an overwrite. The caller holds report.lock for the whole generate+write, so
// the probe cannot race a concurrent writer; a strictly-sequential same-second
// rerun therefore gets a suffixed pair instead of silently overwriting the
// earlier report (each run re-walks mutable upstream and library state, so a
// same-second timestamp does not mean the same content). The loop terminates
// because every probed stem must be occupied on disk to advance; each probe
// round observes the report-wide context so a shutdown stops the directory
// scan instead of starting new stat work after cancellation.
func reportPairStem(ctx context.Context, dir string, generatedAt time.Time) (string, error) {
	base := filepath.Join(dir, "report-"+generatedAt.UTC().Format(reportStampLayout))
	stem := base
	for n := 2; ; n++ {
		if err := interrupted(ctx, "report stem probe"); err != nil {
			return "", err
		}
		free := true
		for _, path := range []string{stem + ".json", stem + ".md"} {
			if _, err := os.Stat(path); err == nil {
				free = false
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				// Basename plus redacted cause only: this error reaches
				// main's log and dir is the secret-capable report.dir value.
				return "", fmt.Errorf("audit: probe report path %s: %w", filepath.Base(path), redactPathErr(dir, err))
			}
		}
		if free {
			return stem, nil
		}
		stem = base + "-" + strconv.Itoa(n)
	}
}

// writeAtomic writes data to path atomically. A non-durable write warns (not
// fails), matching the state store's policy: atomicfile itself emits the one
// WARN carrying the causal parent-directory fsync error (WithLogger keeps it
// on the report logger), so no second app-side record is layered on top.
func writeAtomic(ctx context.Context, path string, data []byte, log *slog.Logger) error {
	_, err := atomicfile.WriteFile(ctx, path, data,
		atomicfile.WithLogger(log),
		atomicfile.WithMkdirMode(reportDirMode),
		atomicfile.WithMode(reportFileMode))
	return err
}

// --- Sanitizers + link/cell escaping ---

// linkURLEscaper backs escapeLinkURL; built once, safe for concurrent use.
var linkURLEscaper = strings.NewReplacer(
	" ", "%20",
	"\t", "%09",
	"\\", "%5C",
	"`", "%60",
	"\"", "%22",
	"'", "%27",
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
// inline metacharacters still active inside a link destination), both quotes
// (inert in CommonMark itself, but attribute-context defense for a downstream
// MD-to-HTML conversion emitting the destination into href="..."), and every
// ASCII whitespace form (space, tab, vertical tab, form feed, CR, LF). It also
// percent-encodes the above-ASCII policy runes url.Parse accepts but a
// terminal or Markdown viewer must never receive raw — C1 controls
// (U+0080-U+009F, terminal-escape introducers), the full Unicode Bidi_Control
// set (visual reordering of the rendered links cell), and the U+2028/U+2029
// line separators — classified by runesafe.IsUnsafeNonASCII (the shared
// policy's above-ASCII subset; the escaper's ASCII replacements above cover
// the rest). Percent-encoding is semantically transparent for a URL, so an
// ordinary destination is unchanged.
func escapeLinkURL(u string) string {
	u = linkURLEscaper.Replace(u)
	var b strings.Builder
	for _, r := range u {
		switch {
		case runesafe.IsUnsafeNonASCII(r):
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

// sanitizeDisplayText makes an untrusted string safe for the machine-readable
// outputs (the JSON report file and slog attributes): the unsafe-rune set is
// the shared runesafe policy (C0 controls except CR/LF, which both encoders
// escape; DEL; C1 controls, single-rune terminal-escape introducers emitted
// raw by encoding/json and slog's JSONHandler; Unicode bidi controls; and the
// U+2028/U+2029 line separators), each replaced with a space. Markdown output
// has its own context-aware sanitizers (escapeCell, escapeLinkURL).
func sanitizeDisplayText(s string) string {
	return runesafe.Sanitize(s)
}

// sanitizeOutput returns a deep-enough copy of the report with every untrusted
// string (row text, group lists, release fields, incomplete-mapping links)
// passed through sanitizeDisplayText, for the machine-readable outputs. The
// canonical Report is never mutated: its rows and nested slices are copied
// before sanitizing (each helper below preserves the current nil/empty
// shape). Verdict, Qualifier, and release Warnings are app-defined
// vocabularies (CurationWarnings returns canonical constants, never raw
// upstream tag bytes), not upstream data, and stay as-is.
func sanitizeOutput(r *Report) *Report {
	out := *r
	out.Rows = sanitizedRows(r.Rows)
	out.Incomplete = sanitizedIncomplete(r.Incomplete)
	return &out
}

// sanitizedRows returns a sanitized clone of the report rows: each row's
// scalar strings pass through sanitizeDisplayText and its nested slices are
// replaced by their sanitized clones. Nil rows become []Row{} to preserve
// the pre-review empty-array JSON shape ("rows": []) for a nil-rows Report
// (slices.Clone(nil) is nil, which would render null).
func sanitizedRows(rows []Row) []Row {
	out := slices.Clone(rows)
	if out == nil {
		out = []Row{}
	}
	for i := range out {
		row := &out[i]
		row.Title = sanitizeDisplayText(row.Title)
		row.Arr = sanitizeDisplayText(row.Arr)
		row.ArrURL = sanitizeDisplayText(row.ArrURL)
		row.SeaDexURL = sanitizeDisplayText(row.SeaDexURL)
		row.MatchSource = sanitizeDisplayText(row.MatchSource)
		row.CurrentGroups = sanitizedStrings(row.CurrentGroups)
		row.Releases = sanitizedReleases(row.Releases)
	}
	return out
}

// sanitizedStrings returns a sanitized copy of a string slice; a nil or
// empty slice is returned as-is (never cloned), preserving its JSON shape.
func sanitizedStrings(ss []string) []string {
	if len(ss) == 0 {
		return ss
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = sanitizeDisplayText(s)
	}
	return out
}

// sanitizedReleases returns a sanitized clone of a row's releases (Tracker,
// Group, and URL are upstream data); a nil or empty slice is returned as-is.
func sanitizedReleases(rels []Release) []Release {
	if len(rels) == 0 {
		return rels
	}
	out := slices.Clone(rels)
	for i := range out {
		out[i].Tracker = sanitizeDisplayText(out[i].Tracker)
		out[i].Group = sanitizeDisplayText(out[i].Group)
		out[i].URL = sanitizeDisplayText(out[i].URL)
	}
	return out
}

// sanitizedIncomplete returns a sanitized clone of the incomplete-mapping
// entries (the releases.moe link); a nil or empty slice is returned as-is.
func sanitizedIncomplete(inc []IncompleteEntry) []IncompleteEntry {
	if len(inc) == 0 {
		return inc
	}
	out := slices.Clone(inc)
	for i := range out {
		out[i].SeaDexURL = sanitizeDisplayText(out[i].SeaDexURL)
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
// A runesafe.Sanitize pre-pass removes the remaining C0/DEL/C1 control
// characters, the full Unicode Bidi_Control set, and the U+2028/U+2029 line
// separators (terminal-escape, visual-reordering, and line-break smuggling);
// CR/LF survive that pass by design and are flattened by cellEscaper here.
func escapeCell(s string) string {
	return cellEscaper.Replace(runesafe.Sanitize(s))
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

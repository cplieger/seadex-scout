package audit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
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
	b.WriteString(annotationLegend)

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

// annotationLegend explains the parenthesized annotations the Scope and SeaDex
// best columns carry, so the report file stays self-explanatory for a reader
// who has only the file (the same reason each verdict section carries
// verdictDesc). Without it "S2 (approx, mixed)" and "PMR (broken)" are
// unexplained in both the report and the README.
const annotationLegend = "Scope annotations: `approx` - the comparison used a coarse bucket " +
	"(the season-0 specials bucket, or a whole-series aggregate spanning more than one season or group), " +
	"so the verdict means \"present somewhere in the series\" rather than an exact per-season attribution; " +
	"`mixed` - the scoped groups span more than one group and none of them is a SeaDex best (a manual review); " +
	"`theoretical` - SeaDex names only a theoretical best, so there is nothing concrete to compare against; " +
	"`incomplete` - the SeaDex entry itself is incomplete.\n\n" +
	"SeaDex best annotations: `(broken)` / `(incomplete)` are SeaDex curation warnings, `(url error)` means " +
	"the SeaDex record carries a link value that is not a usable tracker URL (report it upstream), " +
	"`(unknown tracker)` means the record names a tracker this build does not know, so no link could be " +
	"built (report it as a seadex-scout gap, not as bad SeaDex data), and " +
	"`(unobtainable)` means the release has no usable link or sits on a tracker you do not use. An " +
	"annotated release stays listed but never drives the verdict and is never offered as a link. " +
	"`(N best hidden: animebytes)` means N of the entry's SeaDex BEST releases were withheld because you " +
	"have `animebytes` off, so an empty best column there means \"not on a tracker you use\", not \"SeaDex " +
	"lists no best\". An entry whose withheld releases are only alts carries no marker: SeaDex really lists " +
	"no best for it.\n\n"

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
		bestCell(row),
		links(row))
}

// bestCell renders the SeaDex best column: the displayed best groups, plus the
// count of BEST releases the operator's AnimeBytes toggle withheld
// (Row.hiddenAnimeBytesBest, not the all-releases Row.HiddenAnimeBytes). The
// marker exists so a row whose only bests are AnimeBytes releases is
// distinguishable from an entry SeaDex lists no best for - both otherwise show
// an empty best column, no qualifier and a have_unlisted verdict - and until
// now that distinction reached only the JSON copy and the slog line, leaving
// the Markdown artifact (the human-facing half of the pair) unable to make it.
// Counting hidden ALTS here would make the same claim for an entry SeaDex
// genuinely lists no best for, so the projection is best-only. The count leaks
// no AnimeBytes group, tracker, or link.
func bestCell(row *Row) string {
	groups := escapeCell(orEmpty(strings.Join(displayBestGroups(row.Releases), ", ")))
	if row.hiddenAnimeBytesBest == 0 {
		return groups
	}
	return groups + " (" + strconv.Itoa(row.hiddenAnimeBytesBest) + " best hidden: animebytes)"
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
// is the align.Decide scope decision recorded on the Row, so the label cannot drift
// from the comparison actually performed.
func scopeLabel(row *Row) string {
	if row.scope == align.ScopeSeason {
		return "S" + strconv.Itoa(row.Season)
	}
	return row.scope.String()
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
		if !rel.Best || rel.URL == "" || annotated(rel) {
			continue
		}
		key := releaseLinkKey{tracker: rel.Tracker, url: rel.URL}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		parts = append(parts, mdLink(orTracker(tracker.CanonicalName(rel.Tracker, rel.URL)), rel.URL))
	}
	if len(parts) == 0 {
		return emptyCell
	}
	return strings.Join(parts, linkSep)
}

// selectBestGroups streams the distinct best-release groups in the report's
// clean-before-annotated precedence, calling fn once per survivor and stopping
// early when fn returns false. It is the ONE home for the selection rule both
// best-group renderings share - the Markdown cell (displayBestGroups) and the
// bounded slog attribute (joinBestGroupsAttr) - so the two cannot disagree
// about which groups a row lists or in what order. It yields per release and
// never builds a slice, so the bounded consumer still caps before any
// untrusted aggregate is materialized.
func selectBestGroups(releases []Release, fn func(rel *Release, isAnnotated bool) bool) {
	seen := make(map[[sha256.Size]byte]struct{}, len(releases))
	for _, annotatedPass := range []bool{false, true} {
		for i := range releases {
			rel := &releases[i]
			if !rel.Best || rel.Group == "" || annotated(rel) != annotatedPass {
				continue
			}
			key := foldedGroupKey(rel.Group)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			if !fn(rel, annotatedPass) {
				return
			}
		}
	}
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
	selectBestGroups(releases, func(rel *Release, isAnnotated bool) bool {
		label := rel.Group
		if isAnnotated {
			label += " (" + strings.Join(releaseNotes(rel), ", ") + ")"
		}
		out = append(out, label)
		return true
	})
	return out
}

// foldedGroupKey returns the case-insensitive dedupe identity of an untrusted
// release group as a fixed-size digest. It streams unicode.ToLower rune by
// rune into SHA-256 instead of materializing strings.ToLower's result, so a
// hostile SeaDex entry (up to 512 torrents, each admitting a multi-MB group
// string) can never make a dedupe map retain input-sized copies - the same
// no-untrusted-aggregate-allocation contract logattr.Joiner enforces on the
// rendered bytes (CWE-400). The digest is a lookup identity only, never
// displayed, and matches strings.ToLower's per-rune folding, so dedupe
// decisions are unchanged.
func foldedGroupKey(group string) [sha256.Size]byte {
	h := sha256.New()
	var encoded [utf8.UTFMax]byte
	for _, r := range group {
		n := utf8.EncodeRune(encoded[:], unicode.ToLower(r))
		_, _ = h.Write(encoded[:n])
	}
	var key [sha256.Size]byte
	copy(key[:], h.Sum(nil))
	return key
}

// releaseNotes returns a release's display annotations: its canonical
// curation-warning tags, plus "unobtainable" when the daemon's obtainability
// rule (filter.Obtainable, computed in classifyReleases) rejected it as
// verdict evidence, plus "url error" when SeaDex's record carries a url the
// publisher refused, plus "unknown tracker" when the record's tracker is not
// in this app's canonical table. The returned slice is always a fresh
// allocation, so callers can append without aliasing Release.Warnings.
func releaseNotes(rel *Release) []string {
	notes := append([]string(nil), rel.Warnings...)
	if rel.URLError {
		// Listed BEFORE "unobtainable" because it is the more specific and more
		// actionable of the two: the record itself is wrong upstream, which is
		// also usually WHY the release reads unobtainable.
		notes = append(notes, "url error")
	}
	if rel.UnknownTracker {
		// The other refusal, and deliberately NOT spelled as a url error: the
		// remedy is a seadex-scout tracker-table entry, not a SeaDex record fix
		// (l-f127). Mutually exclusive with "url error" by construction (one
		// refusal grade per release), so a row never shows both.
		notes = append(notes, "unknown tracker")
	}
	if rel.Unobtainable {
		notes = append(notes, "unobtainable")
	}
	return notes
}

// annotated reports whether a release carries display annotations - curation
// warnings, or the daemon obtainability rule's rejection. It is the one
// predicate behind both render sites (the grab-links cell excludes an
// annotated best, the SeaDex-best column marks it), so a new annotation class
// added to releaseNotes cannot start leaking into the links cell.
func annotated(rel *Release) bool {
	return len(rel.Warnings) > 0 || rel.Unobtainable || rel.URLError || rel.UnknownTracker
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
// passed through capDisplayText (after URL redaction where applicable):
// slog's JSONHandler escapes C0 controls but emits C1 controls and bidi
// controls raw, so untrusted titles/groups/tracker strings could otherwise
// smuggle terminal escapes or visual reordering into raw log/Loki views, and
// the same values carry no size bound upstream, so each is also capped at the
// per-attribute volume budget the notify emit path uses. The two aggregate
// attributes (current_group, seadex_best) apply that same policy through the
// bounded logattr.Joiner, which never materializes the untrusted aggregate before
// the cap applies.
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
			"title", capDisplayText(row.Title),
			"al_id", row.AniListID,
			"arr", capDisplayText(row.Arr),
			"verdict", string(row.Verdict),
			"qualifier", string(row.Qualifier),
			"scope", scopeLabel(row),
			"approx", row.Approx,
			"hidden_animebytes", row.HiddenAnimeBytes,
			"current_group", joinGroupsAttr(row.CurrentGroups),
			"seadex_best", joinBestGroupsAttr(row.Releases),
			"arr_url", capDisplayText(library.SafeLogURL(row.ArrURL)),
			"seadex_url", capDisplayText(row.SeaDexURL),
			"match_source", capDisplayText(row.MatchSource))
	}
	for i := range r.Incomplete {
		if err := interrupted(ctx, "report log"); err != nil {
			return err
		}
		log.Info("report incomplete mapping",
			"generated_at", stamp,
			"al_id", r.Incomplete[i].AniListID,
			"seadex_url", capDisplayText(r.Incomplete[i].SeaDexURL))
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

// --- File persistence ---

// reportStampLayout is the UTC timestamp embedded in report filenames: sortable,
// filesystem-safe (no colons), second precision.
const reportStampLayout = "2006-01-02T15-04-05Z"

// WriteFiles renders the report and atomically writes a timestamped JSON +
// Markdown pair into dir (report-<UTC timestamp>.json and .md), creating dir
// as needed. The timestamp (the report's GeneratedAt) keeps successive reports
// from overwriting one another; when a same-second pair already exists (a
// duplicate or rapidly repeated scheduler invocation), a deterministic
// -2/-3/... suffix is probed (reportPairStem) so the earlier report is never
// silently replaced. The caller holds the report lock across the whole
// generate+write, so the probe cannot race a concurrent writer.
//
// Interruption contract: every stage up to and including the JSON write
// observes ctx, so a shutdown stops the pipeline before it spends the grace
// period on work it would lose anyway. Publishing the JSON half is the point
// of no return - after it, the pair's completeness rests on the Markdown write,
// which therefore runs on a short detached budget (markdownWriteGrace) rather
// than being abandoned mid-pair. The one remaining half-pair outcome is a
// non-durable JSON half or a hard Markdown write failure, both of which say so
// in the log.
func (r *Report) WriteFiles(ctx context.Context, dir string, log *slog.Logger) error {
	// dir is the secret-capable report.dir config value: every slog record
	// below (including atomicfile's own WithLogger diagnostics) rides the
	// redacting logger, and every returned error carries only the stage plus
	// a redacted cause, so the expanded value never reaches Loki or main's
	// error log. Filesystem calls keep the real path.
	log = redactingLogger(log, dir)
	// The signal context is one report-wide budget: check it before each
	// stage (cleanup, stem probing, rendering, the JSON write) so a shutdown
	// stops the pipeline instead of spending its grace period on CPU-bound
	// work whose atomic write would fail with context canceled anyway.
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
	// The JSON half is written FIRST, deliberately: whatever goes wrong from
	// here, the failure modes leave a .json without its .md - never a dangling
	// .md without its machine-readable pair.
	data, err := renderJSON(safe)
	if err != nil {
		return fmt.Errorf("audit: encode json: %w", err)
	}
	// BOTH halves are rendered here, before either is published: rendering is
	// the only CPU-bound work the Markdown half has, and it must stay behind
	// the report-wide gate above (that gate exists so a shutdown does not
	// spend its grace period rendering). Once the JSON rename commits, the
	// pair's completeness depends on the Markdown WRITE alone, which then runs
	// on a detached budget - so nothing expensive may be left for it (l-f190).
	markdown := []byte(renderMarkdown(safe))
	// The pair ordering only holds when the JSON half's directory entry is
	// crash-durable: atomicfile reports a successful rename whose parent-dir
	// fsync failed as Durable=false with a NIL error, so publishing the
	// Markdown half on that result could leave a recovered .md without its
	// machine-readable pair. Stop with the JSON half only instead - but as a
	// degradation, not a failure: both the bytes and the operator's next action
	// are unaffected by an fsync that did not land, so this returns nil.
	jsonDurable, err := writeReportHalf(ctx, "json", dir, jsonPath, data, log)
	if err != nil {
		return err
	}
	if !jsonDurable {
		log.Warn("report json written but not crash-durable; skipping the markdown half to keep the pair ordering",
			"json", filepath.Base(jsonPath), "anime", len(r.Rows), "durable", false)
		// The run published an artifact and returns success, so it still emits
		// the success record: alerts.yaml's SeadexScoutReportWritten rule keys
		// on this message, and staying silent here blinded it on a run whose
		// machine-readable half IS on disk (l-f188). The empty markdown name is
		// what tells the operator only one half landed - the same reason the
		// non-durable MARKDOWN path below emits it too rather than going quiet.
		reportWritten(log, "", jsonPath, len(r.Rows), false)
		return nil
	}
	// The Markdown half rides a detached, briefly-bounded context: the JSON
	// rename has committed, so from here a cancellation (a SIGTERM, or the
	// composition root's reportWriteGrace expiring on a slow fsync) would
	// half-publish permanently - the next run probes a fresh stem
	// (reportPairStem needs BOTH halves free), leaving the orphaned .json
	// without its .md forever. The human-readable half is the product of a
	// ~25-minute generation and finishing it is milliseconds of I/O on bytes
	// already rendered above, so the ordering guarantee must not be satisfied
	// by losing half the artifact. atomicfile re-checks the context itself, so
	// detaching here is what actually lets the write proceed.
	mdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markdownWriteGrace)
	defer cancel()
	// A non-durable Markdown half cannot create a dangling .md (the .json is
	// already durably committed above), so the pair is complete and the success
	// record must still be emitted - the alert that watches for it would
	// otherwise go silent on a run whose files are both on disk. The durable
	// attribute keeps the line honest about what may not survive a power loss.
	mdDurable, err := writeReportHalf(mdCtx, "markdown", dir, mdPath, markdown, log)
	if err != nil {
		// The JSON half is already durably committed, so this failure publishes
		// half a pair: name the surviving basename, or the operator reading the
		// markdown error cannot tell the machine-readable half landed and is now
		// a pair-less file the app never deletes (reports are pruned by hand).
		log.Warn("report markdown half failed; the json half is published without it",
			"json", filepath.Base(jsonPath), "anime", len(r.Rows), "error", err)
		return err
	}
	reportWritten(log, mdPath, jsonPath, len(r.Rows), mdDurable)
	return nil
}

// markdownWriteGrace bounds the detached Markdown write. It is deliberately
// short: the bytes are already rendered, so this covers one atomic
// write+rename+fsync, and it is spent on top of whatever budget the caller
// still had - main's reportWriteGrace plus this stays inside Docker's default
// 10s stop grace, so the pair lands before SIGKILL.
const markdownWriteGrace = 2 * time.Second

// reportWritten emits the report pair's success record. It is the ONE call site
// of the alert-keyed "report written" message, so every path that publishes an
// artifact and returns nil announces itself the same way: markdown is the empty
// string when only the JSON half landed (the json-only degradation), and
// durable reports whether the LAST published half's directory entry is
// crash-durable. Basenames only: the stem is timestamp-derived (never
// dir-derived), so the record stays useful without shipping the secret-capable
// report.dir value.
func reportWritten(log *slog.Logger, mdPath, jsonPath string, anime int, durable bool) {
	markdown := ""
	if mdPath != "" {
		markdown = filepath.Base(mdPath)
	}
	log.Info("report written", "markdown", markdown, "json", filepath.Base(jsonPath), "anime", anime, "durable", durable)
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

// atomicWriteFile is atomicfile.WriteFile behind a package variable so the
// durability gates in WriteFiles can be exercised with a Durable=false
// result; a parent-directory fsync failure cannot be induced on a test
// filesystem. Production always uses atomicfile.WriteFile.
var atomicWriteFile = atomicfile.WriteFile

// writeAtomic writes data to path atomically under the report pair's fixed
// option set (the report logger, owner-only dir and file modes) and returns
// atomicfile's whole Result rather than just an error, because the caller gates
// the pair ORDERING on Result.Durable. writeReportHalf owns that policy and
// documents the Durable=false-with-a-nil-error contract it turns on.
func writeAtomic(ctx context.Context, path string, data []byte, log *slog.Logger) (atomicfile.Result, error) {
	return atomicWriteFile(ctx, path, data,
		atomicfile.WithLogger(log),
		atomicfile.WithMkdirMode(reportDirMode),
		atomicfile.WithMode(reportFileMode))
}

// writeReportHalf persists one report half and applies the two policies both
// halves share: a hard write failure is wrapped with the stage and the basename
// only (dir is the secret-capable report.dir value, so the cause is redacted)
// and returned as an error, while a rename whose parent-directory fsync failed -
// which atomicfile reports as Durable=false with a NIL error - is reported
// through the durable return value, NOT as an error.
//
// The split is the alerting rule: a non-durable write SUCCEEDED (atomicfile's
// contract is that a nil error means the bytes reached their final path; only
// the directory entry may not survive a power loss), so there is nothing for the
// operator to do and no failure to report - a re-run cannot fix an fsync. It
// still matters for ORDERING, which is why the flag is returned rather than
// swallowed. atomicfile itself emits the one WARN carrying the causal fsync
// error (WithLogger keeps it on the report logger), so no second app-side
// record is layered on top. Keeping both halves on one path is what stops the
// json and markdown stages from drifting apart.
func writeReportHalf(ctx context.Context, stage, dir, path string, data []byte, log *slog.Logger) (durable bool, err error) {
	res, err := writeAtomic(ctx, path, data, log)
	if err != nil {
		return false, fmt.Errorf("audit: write %s %s: %w", stage, filepath.Base(path), redactPathErr(dir, err))
	}
	return res.Durable, nil
}

// --- Sanitizers + link/cell escaping ---

// escapeLinkURL percent-encodes the characters in a URL that would break out
// of a Markdown link's ](...) destination or the surrounding table cell/row.
// The ASCII half is logattr.EscapeLinkDestination, the one home this policy
// shares with internal/notify's alert attributes: parentheses, angle brackets,
// pipes, backslash and backtick (the CommonMark inline metacharacters still
// active inside a link destination), both quotes (inert in CommonMark itself,
// but attribute-context defense for a downstream MD-to-HTML conversion emitting
// the destination into href="..."), and every ASCII whitespace form (space,
// tab, vertical tab, form feed, CR, LF). On top of it this function also
// percent-encodes the above-ASCII policy runes url.Parse accepts but a
// terminal or Markdown viewer must never receive raw — C1 controls
// (U+0080-U+009F, terminal-escape introducers), the full Unicode Bidi_Control
// set (visual reordering of the rendered links cell), and the U+2028/U+2029
// line separators — classified by runesafe.IsUnsafeNonASCII (the shared
// policy's above-ASCII subset; the escaper's ASCII replacements cover the
// rest). Percent-encoding is semantically transparent for a URL, so an
// ordinary destination is unchanged.
func escapeLinkURL(u string) string {
	u = logattr.EscapeLinkDestination(u)
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
// passes the app's ONE structural vouch step for a browser-destined URL
// (internal/displaylink, shared with the tracker-link publisher, the indexer's
// display gate and the snapshot reader): an absolute http(s) URL, free of a
// userinfo authority and of the backslash / tab-newline smuggling shapes a
// browser reads differently from net/url. Anything else - another scheme
// (javascript:, data:), a hidden-host or relative form, an unparseable
// destination - degrades to the escaped label as plain text, exactly as a
// rejected destination already did.
//
// It used to apply its own gate (TrimSpace + url.Parse + an http/https scheme
// check), which was a second, weaker vocabulary for the same knowledge: it
// checked neither the absolute class nor userinfo nor the smuggling shapes, so a
// hidden-host spelling a browser navigates elsewhere still rendered as an active
// link (l-f189/h-f8). Reading the shared home means a newly refused smuggling
// form is learned once, for every gate.
//
// The emitted destination is the classified form's Trimmed string - the
// preprocessed value the vouch step actually judged - so the link a reader
// clicks is the URL that was vouched, not an original spelling a browser would
// silently rewrite.
func mdLink(label, rawURL string) string {
	safeLabel := escapeCell(label)
	f := urlform.Classify(rawURL)
	if !displaylink.VouchForm(&f) {
		return safeLabel
	}
	return "[" + safeLabel + "](" + escapeLinkURL(f.Trimmed) + ")"
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

// maxAttrBytes is the per-attribute volume budget the report's slog path
// enforces on every untrusted value. The policy itself (the budget, the
// truncation marker, the rune sanitization, and the cap-before-sanitize order)
// lives in internal/logattr, shared with the daemon's notify emit path so the
// two slog emitters cannot drift; this alias keeps the package's own bound
// readable.
const maxAttrBytes = logattr.MaxBytes

// capDisplayText is sanitizeDisplayText plus a volume cap: an honest value
// passes byte-identical, an oversized one (SeaDex admits multi-MB group and
// URL strings, up to 512 torrents per entry) is capped on a rune boundary
// with a "..." marker so one Loki record cannot balloon past the pipeline's
// line limit or amplify memory. The cap is applied BEFORE the per-rune
// sanitize (inside the shared primitive) so an oversized value is never fully
// copied.
//
// A MULTI-SOURCE attribute (a joined group or link list) must never be
// materialized and handed to capDisplayText - joining first would allocate the
// whole untrusted aggregate before the bound applies. Those stream through a
// logattr.Joiner instead (joinGroupsAttr / joinBestGroupsAttr).
func capDisplayText(s string) string { return logattr.Cap(s) }

// joinGroupsAttr renders a row's group list as the comma-separated
// current_group attribute through the bounded joiner: the list is untrusted
// SeaDex/arr data and must not be materialized before the cap applies.
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

// joinBestGroupsAttr renders the seadex_best attribute through the same
// bounded joiner. It streams selectBestGroups - the shared selection rule
// (clean bests first, case-insensitive dedupe on the original-case group,
// annotated bests rendered "GRP (broken, unobtainable)") - rather than calling
// displayBestGroups, because that helper builds the complete label slice,
// which is exactly the untrusted aggregate the budget must bound.
func joinBestGroupsAttr(releases []Release) string {
	j := logattr.NewJoiner()
	first := true
	selectBestGroups(releases, func(rel *Release, isAnnotated bool) bool {
		if !first && !j.WriteSep(",") {
			return false
		}
		first = false
		if !j.Write(rel.Group) {
			return false
		}
		return !isAnnotated || writeNotesAttr(j, releaseNotes(rel))
	})
	return j.String()
}

// writeNotesAttr appends an annotated best's parenthesized note list to j,
// matching displayBestGroups' " (broken, unobtainable)" rendering, and reports
// whether the joiner can still accept more.
func writeNotesAttr(j *logattr.Joiner, notes []string) bool {
	if !j.WriteSep(" (") {
		return false
	}
	for i := range notes {
		if i > 0 && !j.WriteSep(", ") {
			return false
		}
		if !j.Write(notes[i]) {
			return false
		}
	}
	return j.WriteSep(")")
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
// survive as raw Markdown HTML, and encodes the table/link delimiters (| [ ])
// and the backslash itself. strings.NewReplacer performs a
// single non-overlapping left-to-right pass and never re-scans its replacement
// output, so encoding & first does not double-encode the entities it inserts.
// A runesafe.SanitizeSingleLine pre-pass removes the C0/DEL/C1 control
// characters, the full Unicode Bidi_Control set, the U+2028/U+2029 line
// separators (terminal-escape, visual-reordering, and line-break smuggling)
// AND CR/LF, which a Markdown table cell - a single-line sink - cannot carry.
func escapeCell(s string) string {
	return cellEscaper.Replace(runesafe.SanitizeSingleLine(s))
}

// orEmpty returns the empty-cell marker for a blank string.
func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return emptyCell
	}
	return s
}

// orTracker labels a link by tracker name, falling back to "link" when the
// canonical resolution names nothing at all (no known host, no known label,
// and no host to fall back on). The name itself comes from
// tracker.CanonicalName, the one home the daemon's alert attributes share, so
// a Nyaa link whose SeaDex tracker field is blank, an alias, or oddly cased is
// labelled "Nyaa" in the report exactly as it is in the alert, and a tracker
// table edit reaches both surfaces.
func orTracker(name string) string {
	if strings.TrimSpace(name) == "" {
		return "link"
	}
	return name
}

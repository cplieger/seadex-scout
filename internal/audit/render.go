package audit

import (
	"context"
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

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/runesafe"
	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/displaylink"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/logattr"
	"github.com/cplieger/seadex-scout/internal/pathredact"
	"github.com/cplieger/seadex-scout/internal/reportfs"
	"github.com/cplieger/seadex-scout/internal/shutdown"
	"github.com/cplieger/seadex-scout/internal/tracker"
	"github.com/cplieger/urlform"
)

const (
	// linkSep joins the links within a table cell (a middle dot, not an em dash).
	linkSep = " \u00b7 "
	// emptyCell is shown for a column with no value.
	emptyCell = "-"
	// unknownCell marks a column whose fact was never established, as distinct
	// from emptyCell's positive "there is nothing here". The angle brackets are
	// load-bearing: escapeCell entity-encodes < and >, so no upstream release
	// group can render this cell's bytes.
	unknownCell = "<unknown>"
)

// verdictDesc is the one-line explanation shown under each verdict section.
var verdictDesc = map[Verdict]string{
	VerdictUnlisted:    "You have a release SeaDex does not list as best or alt.",
	VerdictAlt:         "You have a listed alt; SeaDex marks a different release best.",
	VerdictUnverified:  "The release-group evidence is unknown on one side (an unidentifiable file or an untagged SeaDex release), or the library walk could not read this item's file data at all, so alignment could not be verified.",
	VerdictNoFile:      "The mapped season, movie, or specials bucket has no file on disk, or a whole-series comparison found no real season with files.",
	VerdictBest:        "You already have SeaDex's best release.",
	VerdictNotOnSeaDex: "In your library and recognized as anime (Fribb-mapped) but SeaDex lists no entry, so there is no recommendation to compare against.",
}

// renderJSON renders the report as indented JSON (the machine-ingestible copy).
// It serializes a sanitized copy (sanitizeOutput): encoding/json escapes C0
// controls but passes C1 and bidi controls through as raw UTF-8.
func renderJSON(r *Report) ([]byte, error) {
	return json.MarshalIndent(sanitizeOutput(r), "", "  ")
}

// renderMarkdown renders the report as human-readable Markdown, grouped into a
// section per verdict (most actionable first) with a compact links column. A
// degraded run additionally carries the completeness caveat in the header and
// the incomplete-mapping section after the verdict sections.
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
		b.WriteString("| Title | Scope | Your group | SeaDex best | Notes | Links |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		for i := range rows {
			writeRow(&b, &rows[i])
		}
		b.WriteByte('\n')
	}
	writeIncompleteSection(&b, r.Incomplete)
	return b.String()
}

// annotationLegend explains the parenthesized Scope annotations and the Notes
// column, so the report file stays self-explanatory for a reader who has only
// the file.
const annotationLegend = "Scope annotations: `approx` - the comparison used a coarse bucket " +
	"(the season-0 specials bucket, or a whole-series aggregate spanning more than one season or group), " +
	"so the verdict means \"present somewhere in the series\" rather than an exact per-season attribution; " +
	"`mixed` - the scoped groups span more than one group and none of them is a SeaDex best (a manual review); " +
	"`theoretical` - SeaDex names only a theoretical best, so there is nothing concrete to compare against; " +
	"`incomplete` - the SeaDex entry itself is incomplete.\n\n" +
	"SeaDex best annotations: the `SeaDex best` column holds ONLY upstream SeaDex group text, and " +
	"everything this report has to say about those releases is in the `Notes` column - so a release group " +
	"literally named `SEV (broken)` upstream cannot be read as a warning from us, and a warning from us " +
	"cannot be mistaken for part of a group's name. A Notes entry associates with a best group BY " +
	"POSITION: one `;`-separated entry per group listed in the SeaDex best column, in the same order, " +
	"with `-` for a group we have nothing to say about (a row with no annotations at all shows a single " +
	"`-`). The note words: " +
	"`broken` / `incomplete` are SeaDex curation warnings, `url error` means " +
	"the SeaDex record carries a link value that is not a usable tracker URL (report it upstream), " +
	"`unknown tracker` means the record names a tracker this build does not know, so no link could be " +
	"built (report it as a seadex-scout gap, not as bad SeaDex data), " +
	"`filtered` means one of the release's SeaDex tags is listed for the `report` " +
	"surface in your `filters.exclude_tags`, so you asked for it not to count, and " +
	"`unobtainable` means the release has no usable link or sits on a tracker you do not use. An " +
	"annotated release stays listed. `filtered`, `url error`, `unknown tracker`, and `unobtainable` " +
	"releases never drive the verdict; `broken` / `incomplete` releases still drive it unless their tags " +
	"are configured under `filters.exclude_tags`. Annotated releases are never offered as a link. " +
	"`(N best hidden: animebytes)` means N of the entry's SeaDex BEST releases were withheld because you " +
	"have `animebytes` off, so an empty best column there means \"not on a tracker you use\", not \"SeaDex " +
	"lists no best\". An entry whose withheld releases are only alts carries no marker: SeaDex really lists " +
	"no best for it.\n\n"

// incompleteHeader is the incomplete-mapping section's Markdown heading text,
// also named by the header caveat so a reader can find the section.
const incompleteHeader = "incomplete (transient AniList failure)"

// writeIncompleteCaveat states the completeness caveat in the report header
// when the run left SeaDex entries unmapped. Silent on a fully resolved run.
func writeIncompleteCaveat(b *strings.Builder, n int) {
	if n == 0 {
		return
	}
	noun := "entries"
	if n == 1 {
		noun = "entry"
	}
	fmt.Fprintf(b, "**Caveat: this report is incomplete.** %d SeaDex %s could not be resolved against AniList this run because of a transient failure; each was either left unmapped or mapped from a stale cached title, so the affected rows may be missing, misfiled, or resting on stale evidence. See the %q section below.\n\n",
		n, noun, incompleteHeader)
}

// writeIncompleteSection renders the incomplete-mapping section: one row per
// SeaDex entry whose library mapping could not be resolved this run, listed by
// AniList id with its releases.moe link. Omitted on a fully resolved run.
func writeIncompleteSection(b *strings.Builder, incomplete []IncompleteEntry) {
	if len(incomplete) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s (%d)\n\n", incompleteHeader, len(incomplete))
	b.WriteString("The AniList lookup that would link these SeaDex entries to the library failed transiently this run. Where a cached answer still existed the entry was mapped from it, so a row for it may appear above with a verdict resting on stale titles; otherwise it has no row at all. Either way its alignment is unconfirmed; re-run the report once AniList recovers.\n\n")
	b.WriteString("| AniList ID | SeaDex |\n| --- | --- |\n")
	for i := range incomplete {
		fmt.Fprintf(b, "| %d | %s |\n", incomplete[i].AniListID, mdLink("seadex", incomplete[i].SeaDexURL))
	}
	b.WriteByte('\n')
}

// writeRow writes one Markdown table row for a report row.
func writeRow(b *strings.Builder, row *Row) {
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n",
		escapeCell(row.Title),
		scopeCell(row),
		groupsCell(row),
		bestCell(row),
		notesCell(row),
		links(row))
}

// groupsCell renders the on-disk groups column. A row whose group evidence was
// never established (Row.GroupsUnknown) renders unknownCell rather than the
// empty marker: emptyCell is the positive claim "nothing identifiable is here".
func groupsCell(row *Row) string {
	if row.GroupsUnknown {
		return unknownCell
	}
	return escapeCell(orEmpty(strings.Join(row.CurrentGroups, ", ")))
}

// bestCell renders the SeaDex best column: the displayed best groups, plus the
// count of BEST releases the operator's AnimeBytes toggle withheld
// (Row.HiddenAnimeBytesBest, not the all-releases Row.HiddenAnimeBytes), so a
// row whose only bests are AnimeBytes releases is distinguishable from an entry
// SeaDex lists no best for. The count leaks no group, tracker, or link.
func bestCell(row *Row) string {
	shown := displayBestGroups(row.Releases)
	for i := range shown {
		shown[i] = escapeBestGroup(shown[i])
	}
	groups := orEmpty(strings.Join(shown, ", "))
	if row.HiddenAnimeBytesBest == 0 {
		return groups
	}
	return groups + " (" + strconv.Itoa(row.HiddenAnimeBytesBest) + " best hidden: animebytes)"
}

// notesCell renders the Notes column: this report's annotations of the best
// releases the SeaDex-best column lists, kept out of the upstream group text so
// no group name can be read as a curation warning from us. Association is
// POSITIONAL: one entry per group the best column lists (same selectBestGroups
// order), `;`-separated, with emptyCell for a group carrying no note.
func notesCell(row *Row) string {
	var entries []string
	annotatedAny := false
	selectBestGroups(row.Releases, func(rel *Release, isAnnotated bool) bool {
		if !isAnnotated {
			entries = append(entries, emptyCell)
			return true
		}
		annotatedAny = true
		entries = append(entries, strings.Join(releaseNotes(rel), ", "))
		return true
	})
	if !annotatedAny {
		return emptyCell
	}
	// The note words are this app's own vocabulary; escapeCell is applied anyway.
	return escapeCell(strings.Join(entries, "; "))
}

// scopeCell renders the scope for the Markdown table, appending the comparison
// annotations in parentheses: "approx" for a coarse multi-group bucket and the
// qualifier when one applies - e.g. "S2 (approx, mixed)".
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
// comparison. A pure reader of Row.Scope, so the label cannot drift from the
// comparison actually performed; the JSON renderer publishes the same value
// through align.ScopeKind.MarshalJSON, keeping kind and number separable.
func scopeLabel(row *Row) string {
	if row.Scope == align.ScopeSeason {
		return "S" + strconv.Itoa(row.Season)
	}
	return row.Scope.String()
}

// releaseLinkKey is the structural dedupe identity for a links-cell entry: a
// comparable tuple, not a joined string, so a crafted tracker or URL carrying
// the would-be delimiter cannot collide two distinct pairs.
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
		// A curation-warned or unobtainable best is not offered as a grab link: the
		// cell is an action affordance, and the release is annotated in the Notes
		// column instead. Deliberately annotation-driven, not verdict-driven.
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
// best-group renderings share, and it never builds a slice, so a bounded
// consumer still caps before any untrusted aggregate is materialized.
func selectBestGroups(releases []Release, fn func(rel *Release, isAnnotated bool) bool) {
	seen := make(map[string]struct{}, len(releases))
	for _, annotatedPass := range []bool{false, true} {
		for i := range releases {
			rel := &releases[i]
			if !rel.Best || rel.Group == "" || annotated(rel) != annotatedPass {
				continue
			}
			key := strings.ToLower(rel.Group)
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
// case (deduped case-insensitively), for display. The returned text is UPSTREAM
// SeaDex data and nothing else; this app's annotations are notesCell's column.
// Clean bests are collected first and win the dedupe, which is also what makes
// the positional Notes association stable.
func displayBestGroups(releases []Release) []string {
	var out []string
	selectBestGroups(releases, func(rel *Release, _ bool) bool {
		out = append(out, rel.Group)
		return true
	})
	return out
}

// releaseNotes returns a release's display annotations: its canonical
// curation-warning tags, plus "unobtainable", "url error" and "unknown tracker"
// for the corresponding refusals. The returned slice is always a fresh
// allocation, so callers can append without aliasing Release.Warnings.
func releaseNotes(rel *Release) []string {
	notes := append([]string(nil), rel.Warnings...)
	if rel.URLError {
		// Listed BEFORE "unobtainable": the record itself is wrong upstream, which is
		// also usually WHY the release reads unobtainable.
		notes = append(notes, "url error")
	}
	if rel.UnknownTracker {
		// The other refusal, mutually exclusive with "url error" by construction (one
		// refusal grade per release), and deliberately not spelled as a url error.
		notes = append(notes, "unknown tracker")
	}
	if rel.Unobtainable {
		notes = append(notes, "unobtainable")
	}
	if rel.Filtered {
		// The operator's own tag policy excluded this release, which forfeits its BEST
		// evidence. Without a note the row self-contradicts whenever the excluded tag
		// is not a curation warning.
		notes = append(notes, "filtered")
	}
	return notes
}

// annotated reports whether a release carries display annotations - curation
// warnings, a publisher refusal, the obtainability rule's rejection, or the
// operator's tag policy. It READS releaseNotes rather than restating its class
// list, which is what stops a new annotation class from rendering a note while
// still being offered as a grab link. DISPLAY only: verdict eligibility is
// audit.go's forfeitsBest.
func annotated(rel *Release) bool {
	return len(releaseNotes(rel)) > 0
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

// Log emits the report to slog: a summary line then one INFO line per row, so
// the report is queryable in Loki alongside the Markdown. The summary's msg is
// "report summary", deliberately distinct from Scout.Report's "report
// generated", so a Loki counter keyed on either never double-counts a run.
// Cancellation is observed before the summary and between row records, so a
// shutdown neither emits a rowless summary nor spends its grace on row lines.
// Every untrusted string passes through capDisplayText; the three aggregate
// attributes stream through logattr.Joiner instead of being materialized.
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
		bestGroups, bestNotes := joinBestAttrs(row.Releases)
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
			"hidden_animebytes_best", row.HiddenAnimeBytesBest,
			"current_group", joinGroupsAttr(row.CurrentGroups),
			"groups_unknown", row.GroupsUnknown,
			"seadex_best", bestGroups,
			"seadex_best_notes", bestNotes,
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
// wrapping ctx.Err() as the classification token main's shutdown handling keys
// on (errors.Is context.Canceled) plus the signal cause for display. It returns
// nil while the context is live, so callers can gate each stage on one budget.
func interrupted(ctx context.Context, stage string) error {
	if ctx.Err() == nil {
		return nil
	}
	return shutdown.InterruptedAs(ctx, "audit: "+stage+" interrupted")
}

// reportStampLayout is the UTC timestamp embedded in report filenames: sortable,
// filesystem-safe (no colons), second precision.
const reportStampLayout = "2006-01-02T15-04-05Z"

// WriteFiles renders the report and atomically writes a timestamped JSON +
// Markdown pair into dir (report-<UTC timestamp>.json and .md), creating dir as
// needed. A same-second pair takes a deterministic -2/-3/... suffix
// (reportPairStem), so an earlier report is never silently replaced.
//
// Interruption contract: every stage up to and including the JSON write observes
// ctx. Publishing the JSON half is the point of no return, so the Markdown write
// runs on a short detached budget rather than being abandoned mid-pair.
func (r *Report) WriteFiles(ctx context.Context, dir string, log *slog.Logger) error {
	// dir is the secret-capable report.dir config value: every record below rides
	// the redacting logger, and every returned error carries only the stage plus a
	// redacted cause. Filesystem calls keep the real path.
	log = pathredact.Logger(log, dir)
	// The signal context is one report-wide budget: check it before each stage, so
	// a shutdown stops the pipeline instead of spending its grace on lost work.
	if err := interrupted(ctx, "report write"); err != nil {
		return err
	}
	// Reap stale atomicfile temps first: a crash between temp create and rename
	// orphans a .atomicfile-<digits>.tmp in the report dir forever otherwise. The
	// caller holds report.lock, so no concurrent writer owns an in-flight temp, and
	// a missing dir is not an error.
	if _, err := atomicfile.CleanupStaleTemps(dir, time.Hour, atomicfile.WithLogger(log)); err != nil {
		// No dir attribute: the redacting logger would mask it anyway.
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
	// Render from a credential-redacted copy: report rows carry ArrURLs from the
	// raw library snapshot, so a credentialed public_url would otherwise persist
	// verbatim into the report pair.
	safe := redactReportURLs(r)
	// The JSON half is written FIRST, deliberately: every failure mode from here
	// leaves a .json without its .md, never a .md without its readable pair.
	data, err := renderJSON(safe)
	if err != nil {
		return fmt.Errorf("audit: encode json: %w", err)
	}
	// BOTH halves are rendered here, before either is published: once the JSON
	// rename commits, the pair's completeness depends on the Markdown WRITE alone,
	// which runs on a detached budget, so nothing expensive may be left for it.
	markdown := []byte(renderMarkdown(safe))
	// The pair ordering only holds when the JSON half's directory entry is
	// crash-durable: atomicfile reports a rename whose parent-dir fsync failed as
	// Durable=false with a NIL error, so stop with the JSON half only - as a
	// degradation, not a failure, since the bytes and the next action are the same.
	jsonDurable, err := writeReportHalf(ctx, "json", dir, jsonPath, data, log)
	if err != nil {
		return err
	}
	if !jsonDurable {
		log.Warn("report json written but not crash-durable; skipping the markdown half to keep the pair ordering",
			"json", filepath.Base(jsonPath), "anime", len(r.Rows), "durable", false)
		// The run published an artifact and returns success, so it still emits the
		// success record alerts.yaml's SeadexScoutReportWritten rule keys on. The
		// empty markdown name is what tells the operator only one half landed.
		reportWritten(log, "", jsonPath, len(r.Rows), false)
		return nil
	}
	// The Markdown half rides a detached context: the JSON rename has committed, so
	// from here a cancellation would half-publish permanently - the next run probes
	// a fresh stem (reportPairStem needs BOTH halves free), orphaning the .json.
	// The extra grace is armed ONLY once a shutdown has landed: an unconditional
	// ceiling would cap the SECOND half of the pair below the budget the FIRST half
	// just had, so on a slow mount every report would half-publish permanently.
	mdCtx := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		mdCtx, cancel = context.WithTimeout(mdCtx, markdownWriteGrace)
		defer cancel()
	}
	// A non-durable Markdown half cannot create a dangling .md (the .json is
	// already durably committed), so the pair is complete and the success record
	// must still be emitted; the durable attribute keeps the line honest.
	mdDurable, err := writeReportHalf(mdCtx, "markdown", dir, mdPath, markdown, log)
	if err != nil {
		// The JSON half is already durably committed, so this failure publishes half a
		// pair: name the surviving basename.
		log.Warn("report markdown half failed; the json half is published without it",
			"json", filepath.Base(jsonPath), "anime", len(r.Rows), "error", err)
		return err
	}
	reportWritten(log, mdPath, jsonPath, len(r.Rows), mdDurable)
	return nil
}

// markdownWriteGrace bounds the detached Markdown write AFTER A SHUTDOWN, and
// only then: with no signal pending the caller's context carries no deadline and
// the JSON half is written under exactly that context, so a ceiling on the
// markdown half alone could only lose the pair. It is deliberately short because
// it is spent ON TOP of a budget that has already run out - the bytes are
// rendered, so it covers one atomic write+rename+fsync inside Docker's 10s stop
// grace.
const markdownWriteGrace = 2 * time.Second

// reportWritten emits the report pair's success record. It is the ONE call site
// of the alert-keyed "report written" message: markdown is the empty string when
// only the JSON half landed, and durable reports whether the last published
// half's directory entry is crash-durable. Basenames only, so the record never
// ships the secret-capable report.dir value.
func reportWritten(log *slog.Logger, mdPath, jsonPath string, anime int, durable bool) {
	markdown := ""
	if mdPath != "" {
		markdown = filepath.Base(mdPath)
	}
	log.Info("report written", "markdown", markdown, "json", filepath.Base(jsonPath), "anime", anime, "durable", durable)
}

// redactReportURLs returns a shallow copy of the report whose rows carry
// credential-free ArrURLs, so a credentialed arr public_url never lands in the
// persisted report files. The canonical Report is never mutated.
func redactReportURLs(r *Report) *Report {
	out := *r
	out.Rows = slices.Clone(r.Rows)
	for i := range out.Rows {
		out.Rows[i].ArrURL = library.SafeLogURL(out.Rows[i].ArrURL)
	}
	return &out
}

// reportPairStem selects a collision-free filename stem for the report pair: the
// second-precision GeneratedAt stem when neither half exists, otherwise the
// first deterministic "-N" suffix (N >= 2) where both halves are free. A
// non-NotExist stat error is surfaced rather than risking an overwrite. The loop
// terminates because every probed stem must be occupied on disk to advance, and
// each round observes the report-wide context.
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
				// Basename plus redacted cause only: dir is secret-capable.
				return "", fmt.Errorf("audit: probe report path %s: %w", filepath.Base(path), pathredact.Err(dir, err))
			}
		}
		if free {
			return stem, nil
		}
		stem = base + "-" + strconv.Itoa(n)
	}
}

// atomicWriteFile is atomicfile.WriteFile behind a package variable so the
// durability gates in WriteFiles can be exercised with a Durable=false result;
// a parent-directory fsync failure cannot be induced on a test filesystem.
var atomicWriteFile = atomicfile.WriteFile

// writeAtomic writes data to path atomically under the report pair's fixed
// option set and returns atomicfile's whole Result rather than just an error,
// because the caller gates the pair ORDERING on Result.Durable.
//
// Reports enumerate the operator's library and can carry private-tracker page
// links, so the directory and every written half are owner-only (CWE-732).
func writeAtomic(ctx context.Context, path string, data []byte, log *slog.Logger) (atomicfile.Result, error) {
	// The directory's privacy rule has one home (internal/reportfs): WithMkdirMode
	// goes through MkdirAll's perm argument, which a umask or default ACL filters.
	if err := reportfs.MakeDir(filepath.Dir(path)); err != nil {
		return atomicfile.Result{}, err
	}
	return atomicWriteFile(ctx, path, data,
		atomicfile.WithLogger(log),
		atomicfile.WithMode(reportfs.FileMode))
}

// writeReportHalf persists one report half and applies the two policies both
// halves share: a hard write failure is wrapped with the stage and the basename
// only (the cause is redacted) and returned as an error, while a rename whose
// parent-directory fsync failed - atomicfile's Durable=false with a NIL error -
// is reported through the durable return value, NOT as an error. A non-durable
// write SUCCEEDED, so there is nothing for the operator to do; it still matters
// for ORDERING, which is why the flag is returned rather than swallowed.
func writeReportHalf(ctx context.Context, stage, dir, path string, data []byte, log *slog.Logger) (durable bool, err error) {
	res, err := writeAtomic(ctx, path, data, log)
	if err != nil {
		return false, fmt.Errorf("audit: write %s %s: %w", stage, filepath.Base(path), pathredact.Err(dir, err))
	}
	return res.Durable, nil
}

// escapeLinkURL percent-encodes the characters in a URL that would break out of
// a Markdown link's ](...) destination or the surrounding table cell/row. The
// ASCII half is logattr.EscapeLinkDestination, the one home this policy shares
// with internal/notify's alert attributes; on top of it this percent-encodes the
// above-ASCII policy runes url.Parse accepts but a terminal or Markdown viewer
// must never receive raw (C1 controls, the Bidi_Control set, U+2028/U+2029),
// classified by runesafe.IsUnsafeNonASCII. Percent-encoding is semantically
// transparent, so an ordinary destination is unchanged.
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
// (internal/displaylink): an absolute http(s) URL, free of a userinfo authority
// and of the smuggling shapes a browser reads differently from net/url.
// Anything else degrades to the escaped label as plain text. The emitted
// destination is the classified form's Trimmed string - the value the vouch step
// actually judged, not a spelling a browser would silently rewrite.
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

// bestGroupEscaper encodes the characters the SeaDex-best column's own structure
// uses, on top of escapeCell's cell escapes: the comma separating the listed
// groups - the positional key notesCell and joinBestAttrs bind to - and the
// parentheses delimiting the "(N best hidden: animebytes)" marker. Without it a
// group named "SEV, PMR" reads as two listed groups, shifting every later note.
var bestGroupEscaper = strings.NewReplacer(",", "&#44;", "(", "&#40;", ")", "&#41;")

// escapeBestGroup renders one upstream group for the SeaDex-best column.
func escapeBestGroup(group string) string {
	return bestGroupEscaper.Replace(escapeCell(group))
}

// sanitizeDisplayText makes an untrusted string safe for the machine-readable
// outputs (the JSON report file and slog attributes): the unsafe-rune set is the
// shared runesafe policy, each replaced with a space. Markdown output has its own
// context-aware sanitizers (escapeCell, escapeLinkURL).
func sanitizeDisplayText(s string) string {
	return runesafe.Sanitize(s)
}

// maxAttrBytes is the per-attribute volume budget the report's slog path
// enforces on every untrusted value. The policy itself lives in internal/logattr,
// shared with the daemon's notify emit path; this alias keeps the bound readable.
const maxAttrBytes = logattr.MaxBytes

// capDisplayText is sanitizeDisplayText plus a volume cap: an honest value passes
// byte-identical, an oversized one is capped on a rune boundary with a "..."
// marker, before the per-rune sanitize so it is never fully copied. A
// MULTI-SOURCE attribute must never be materialized and handed to it: those
// stream through a logattr.Joiner (joinGroupsAttr / joinBestAttrs).
func capDisplayText(s string) string { return logattr.Cap(s) }

// joinGroupsAttr renders a row's group list as the comma-separated current_group
// attribute through the bounded joiner: the list is untrusted and must not be
// materialized before the cap applies.
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

// joinBestAttrs renders the seadex_best and seadex_best_notes attributes in ONE
// pass over selectBestGroups, under a COUPLED stop: a piece is admitted only when
// both joiners can take it, so the two attributes always carry the same number of
// positional slots. Two independent budgets cut at different indices, silently
// re-binding a note to a group the line did not carry. groups carries ONLY
// upstream group text, and notes is the empty string when no annotated best
// exists, so a Loki query can test it for emptiness.
func joinBestAttrs(releases []Release) (groups, notes string) {
	gj, nj := logattr.NewJoiner(), logattr.NewJoiner()
	first := true
	annotatedAny := false
	selectBestGroups(releases, func(rel *Release, isAnnotated bool) bool {
		// Both separators are charged together, so one budget refusing a piece
		// stops BOTH attributes and neither can gain a slot the other is missing.
		if !first && (!gj.WriteSep(",") || !nj.WriteSep(";")) {
			return false
		}
		first = false
		if !writeBestGroupAttr(gj, rel.Group) {
			return false
		}
		if !isAnnotated {
			// emptyCell is a positional VALUE, not a separator: it occupies this
			// group's slot exactly as notesCell's placeholder does.
			return nj.Write(emptyCell)
		}
		annotatedAny = true
		return writeNotesAttr(nj, releaseNotes(rel))
	})
	if !annotatedAny {
		return gj.String(), ""
	}
	return gj.String(), nj.String()
}

// writeBestGroupAttr streams one group into j with its commas encoded, so a comma
// in upstream group text cannot read as the seadex_best separator that
// seadex_best_notes binds to positionally. It cuts incrementally rather than
// escaping the whole value first, so a multi-MB group is never copied.
func writeBestGroupAttr(j *logattr.Joiner, group string) bool {
	for {
		before, after, found := strings.Cut(group, ",")
		if !j.Write(before) {
			return false
		}
		if !found {
			return true
		}
		if !j.WriteSep("&#44;") {
			return false
		}
		group = after
	}
}

// writeNotesAttr appends one annotated best's note list to j, matching notesCell's
// comma-joined spelling, and reports whether the joiner can still accept more.
func writeNotesAttr(j *logattr.Joiner, notes []string) bool {
	for i := range notes {
		if i > 0 && !j.WriteSep(", ") {
			return false
		}
		if !j.Write(notes[i]) {
			return false
		}
	}
	return true
}

// sanitizeOutput returns a deep-enough copy of the report with every untrusted
// string passed through sanitizeDisplayText, for the machine-readable outputs.
// The canonical Report is never mutated. Verdict, Qualifier and release Warnings
// are app-defined vocabularies, not upstream data, and stay as-is.
func sanitizeOutput(r *Report) *Report {
	out := *r
	out.Rows = sanitizedRows(r.Rows)
	out.Incomplete = sanitizedIncomplete(r.Incomplete)
	return &out
}

// sanitizedRows returns a sanitized clone of the report rows. Nil rows become
// []Row{} to preserve the empty-array JSON shape ("rows": []), since
// slices.Clone(nil) is nil and would render null.
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
// entities instead of backslash escapes so a pre-existing backslash cannot
// cancel an inserted escape, neutralizes the raw HTML metacharacters (& < >),
// and encodes the table/link delimiters (| [ ]) and the backslash itself.
// strings.NewReplacer makes one non-overlapping left-to-right pass and never
// re-scans its output, so encoding & first does not double-encode. A
// runesafe.SanitizeSingleLine pre-pass removes the control and bidi runes AND
// CR/LF, which a single-line sink cannot carry.
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
// canonical resolution names nothing at all. The name comes from
// tracker.CanonicalName, the one home the daemon's alert attributes share, so a
// blank or oddly-cased SeaDex tracker field labels the same on both surfaces.
func orTracker(name string) string {
	if strings.TrimSpace(name) == "" {
		return "link"
	}
	return name
}

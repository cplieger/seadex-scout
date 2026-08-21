package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/keyenc"
	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/slogx/capture"
)

func TestScopeLabel(t *testing.T) {
	tests := []struct {
		name string
		want string
		row  Row
	}{
		{"movie", "movie", Row{Scope: align.ScopeMovie}},
		{"special", "special", Row{Scope: align.ScopeSpecial}},
		{"numbered season", "S2", Row{Scope: align.ScopeSeason, Season: 2}},
		{"whole series", "series", Row{Scope: align.ScopeWholeSeries}},
		{"zero value defaults to series", "series", Row{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scopeLabel(&tt.row); got != tt.want {
				t.Errorf("scopeLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScopeCellMarksApproxAndQualifier(t *testing.T) {
	if got := scopeCell(&Row{Scope: align.ScopeSeason, Season: 2, Approx: true}); got != "S2 (approx)" {
		t.Errorf("scopeCell() = %q, want \"S2 (approx)\"", got)
	}
	if got := scopeCell(&Row{Scope: align.ScopeSeason, Season: 2}); got != "S2" {
		t.Errorf("scopeCell() = %q, want \"S2\"", got)
	}
	if got := scopeCell(&Row{Scope: align.ScopeSeason, Season: 2, Qualifier: QualifierMixed}); got != "S2 (mixed)" {
		t.Errorf("scopeCell() = %q, want \"S2 (mixed)\"", got)
	}
	if got := scopeCell(&Row{Scope: align.ScopeSeason, Season: 2, Approx: true, Qualifier: QualifierTheoretical}); got != "S2 (approx, theoretical)" {
		t.Errorf("scopeCell() = %q, want \"S2 (approx, theoretical)\"", got)
	}
}

func TestDisplayBestGroups(t *testing.T) {
	rels := []Release{
		{Group: "SubsPlease", Best: true},
		{Group: "subsplease", Best: true},
		{Group: "Erai", Best: false},
	}
	got := displayBestGroups(rels)
	if !reflect.DeepEqual(got, []string{"SubsPlease"}) {
		t.Errorf(`displayBestGroups() = %v, want [SubsPlease] (best-only, case-insensitive dedupe, original case, upstream text only)`, got)
	}
}

func TestEscapeCell(t *testing.T) {
	// Pipes/brackets/backslashes become HTML entities (not backslash escapes,
	// which a pre-existing backslash could otherwise cancel); CR/LF flatten.
	if got := escapeCell("a|b\nc"); got != "a&#124;b c" {
		t.Errorf("escapeCell() = %q, want %q", got, "a&#124;b c")
	}
	// A crafted backslash cannot cancel the delimiter escape.
	if got := escapeCell("x\\]y\\|z"); got != "x&#92;&#93;y&#92;&#124;z" {
		t.Errorf("escapeCell() = %q, want %q", got, "x&#92;&#93;y&#92;&#124;z")
	}
	// Raw HTML metacharacters are neutralized so markup cannot survive.
	if got := escapeCell("<img src=x>&"); got != "&lt;img src=x&gt;&amp;" {
		t.Errorf("escapeCell() = %q, want %q", got, "&lt;img src=x&gt;&amp;")
	}
}

// TestMdLinkAppliesTheSharedStructuralVouch pins the report renderer against the
// app's one structural vouch step for a browser-destined URL (internal/displaylink,
// h-f8/l-f189). The renderer used to apply its own weaker gate - TrimSpace plus
// url.Parse plus an http/https scheme check - which checked neither the absolute
// class, nor userinfo, nor the browser-vs-net/url smuggling shapes, so a spelling
// a browser resolves elsewhere still rendered as an active link a reader clicks.
//
// The emitted destination is the vouched form's cleaned string, so a padded URL
// links to the value that was actually judged rather than to a spelling a browser
// would silently rewrite.
func TestMdLinkAppliesTheSharedStructuralVouch(t *testing.T) {
	tests := map[string]struct{ raw, want string }{
		"plain absolute":            {"https://nyaa.si/view/1", "[t](https://nyaa.si/view/1)"},
		"leading tab trimmed":       {"\thttps://nyaa.si/view/1", "[t](https://nyaa.si/view/1)"},
		"trailing cr trimmed":       {"https://nyaa.si/view/1\r", "[t](https://nyaa.si/view/1)"},
		"leading nul trimmed":       {"\x00https://nyaa.si/view/1", "[t](https://nyaa.si/view/1)"},
		"surrounding space trimmed": {"  https://nyaa.si/view/1  ", "[t](https://nyaa.si/view/1)"},
		// Newly refused: each is a form the old scheme-only gate rendered as an
		// active link even though a browser resolves it somewhere else.
		"hidden host refused":    {"https:nyaa.si/view/1", "t"},
		"one-slash host refused": {"https:/nyaa.si/view/1", "t"},
		"userinfo refused":       {"https://nyaa.si@evil.example/view/1", "t"},
		"backslash refused":      {"https:/\\nyaa.si/view/1", "t"},
		"embedded tab refused":   {"https://nyaa\t.si/view/1", "t"},
		"empty host refused":     {"http://", "t"},
		"protocol relative":      {"//nyaa.si/view/1", "t"},
		"relative refused":       {"/torrents.php?id=1", "t"},
		"non-http refused":       {"javascript:alert(1)", "t"},
		"empty refused":          {"", "t"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := mdLink("t", tc.raw); got != tc.want {
				t.Errorf("mdLink(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEscapeLinkURLEncodesWhitespace(t *testing.T) {
	got := escapeLinkURL("https://x/a\tb\vc\fd e")
	if strings.ContainsAny(got, "\t\v\f \n\r") {
		t.Errorf("escapeLinkURL left raw whitespace: %q", got)
	}
	if want := "https://x/a%09b%0Bc%0Cd%20e"; got != want {
		t.Errorf("escapeLinkURL() = %q, want %q", got, want)
	}
}

func TestEscapeLinkURLEncodesBackslashAndBacktick(t *testing.T) {
	// A trailing backslash would escape the emitted closing ')' in CommonMark,
	// so the destination must carry %5C instead.
	got := escapeLinkURL(`https://x/path\`)
	if want := "https://x/path%5C"; got != want {
		t.Errorf("escapeLinkURL(trailing backslash) = %q, want %q", got, want)
	}
	// The destination is now refused BEFORE escaping: a backslash is one of the
	// browser-vs-net/url smuggling shapes the shared structural vouch step
	// (internal/displaylink) drops rather than vouch, so the cell degrades to the
	// plain label (h-f8/l-f189). The %5C escaping above still guards every
	// destination that IS vouched and happens to carry a backslash-escaped
	// percent form.
	link := mdLink("nyaa", `https://x/path\`)
	if want := "nyaa"; link != want {
		t.Errorf("mdLink(trailing backslash) = %q, want the plain label %q", link, want)
	}
	// A backtick could open a code span across the ']( ' boundary; it must be
	// percent-encoded in the destination.
	got = escapeLinkURL("https://x/a`b")
	if want := "https://x/a%60b"; got != want {
		t.Errorf("escapeLinkURL(backtick) = %q, want %q", got, want)
	}
}

// TestEscapeLinkURLEncodesQuotes pins the attribute-context defense the
// escaper documents: both quote forms are percent-encoded in a link
// destination, so a downstream MD-to-HTML conversion emitting the
// destination into href="..." cannot be broken out of by a crafted URL.
func TestEscapeLinkURLEncodesQuotes(t *testing.T) {
	got := escapeLinkURL(`https://x/a"b'c`)
	if want := "https://x/a%22b%27c"; got != want {
		t.Errorf("escapeLinkURL(quotes) = %q, want %q", got, want)
	}
	link := mdLink("nyaa", `https://x/a"b'c`)
	if want := "[nyaa](https://x/a%22b%27c)"; link != want {
		t.Errorf("mdLink(quotes) = %q, want %q", link, want)
	}
}

func TestLinksBuildsArrSeaDexAndBestOnly(t *testing.T) {
	row := &Row{
		Arr:       "sonarr",
		ArrURL:    "http://sonarr/series/frieren",
		SeaDexURL: "https://releases.moe/154587",
		Releases: []Release{
			{Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
			{Best: false, Tracker: "AB", URL: "https://animebytes.tv/x"},
		},
	}
	got := links(row)
	if !strings.Contains(got, "http://sonarr/series/frieren") {
		t.Error("links must include the arr deep-link")
	}
	if !strings.Contains(got, "https://releases.moe/154587") {
		t.Error("links must include the SeaDex entry link")
	}
	if !strings.Contains(got, "https://nyaa.si/view/1") {
		t.Error("links must include the best-release link")
	}
	if strings.Contains(got, "animebytes.tv/x") {
		t.Error("links must not include a non-best release link")
	}
}

func TestLinksEmptyIsPlaceholder(t *testing.T) {
	if got := links(&Row{}); got != emptyCell {
		t.Errorf("links() = %q, want empty-cell placeholder %q", got, emptyCell)
	}
}

func TestRenderMarkdownAndJSON(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows: []Row{{
			Title: "Frieren", Arr: "sonarr", Verdict: VerdictBest, Season: 1,
			CurrentGroups: []string{"subsplease"},
			Releases:      []Release{{Group: "SubsPlease", Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
		}},
	}
	md := renderMarkdown(r)
	for _, want := range []string{"# SeaDex alignment report", "Frieren", string(VerdictBest)} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	if _, err := renderJSON(r); err != nil {
		t.Errorf("renderJSON: %v", err)
	}
}

func TestRenderMarkdownScopePrecedence(t *testing.T) {
	// Build the rows through assess so the test pins the real classification
	// precedence (movie beats season beats special), not just the label map.
	a := New(Config{})
	movie := &library.Item{
		Arr: library.ArrRadarr, Title: "Movie",
		Groups: []string{"g"}, HasFile: true,
	}
	series := &library.Item{
		Arr: library.ArrSonarr, Title: "Mapped OVA",
		SeasonGroups: map[int][]string{2: {"g"}}, HasFile: true,
	}
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictUnlisted): 2},
		Rows: []Row{
			// A Radarr item scopes as a movie even with a special, seasoned record.
			a.assess(&match.Match{
				Item:   movie,
				Arr:    library.ArrRadarr,
				Record: mapping.Record{Type: "OVA", SeasonTvdb: 2},
			}),
			// A positive Fribb TVDB season wins over the record being a special.
			a.assess(&match.Match{
				Item:   series,
				Arr:    library.ArrSonarr,
				Record: mapping.Record{Type: "OVA", SeasonTvdb: 2},
			}),
		},
	}

	got := renderMarkdown(r)
	if !strings.Contains(got, "| Movie | movie |") {
		t.Errorf("renderMarkdown() did not give movie scope precedence: %s", got)
	}
	if !strings.Contains(got, "| Mapped OVA | S2 |") {
		t.Errorf("renderMarkdown() did not give mapped season scope precedence: %s", got)
	}
}

// recordAttrs collects a record's direct attributes into a map of typed values
// (slog.Value.Any preserves int64/bool/string, unlike a JSON round-trip's
// float64 coercion).
func recordAttrs(r slog.Record) map[string]any {
	out := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.Any()
		return true
	})
	return out
}

func TestReportLogEmitsSummaryAndPerRowLines(t *testing.T) {
	log, rec := capture.New()
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1, string(VerdictNoFile): 2},
		Rows: []Row{{
			Title: "Frieren", Arr: library.ArrSonarr, Verdict: VerdictBest, AniListID: 154587,
			Qualifier: QualifierMixed,
			Season:    1, Scope: align.ScopeSeason, Approx: true, CurrentGroups: []string{"subsplease", "erai-raws"},
			Releases:    []Release{{Group: "SubsPlease", Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
			ArrURL:      "http://sonarr/series/frieren",
			SeaDexURL:   "https://releases.moe/154587",
			MatchSource: "id",
		}},
	}

	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}

	if rec.Len() != 2 {
		t.Fatalf("Log emitted %d records, want 2 (summary + one per row)", rec.Len())
	}
	recs := rec.Records()
	summary, row := recs[0], recs[1]
	if summary.Message != "report summary" {
		t.Errorf("summary msg = %q, want %q", summary.Message, "report summary")
	}
	sAttrs := recordAttrs(summary)
	if sAttrs["rows"] != int64(1) || sAttrs["have_best"] != int64(1) || sAttrs["no_file"] != int64(2) {
		t.Errorf("summary counts = rows:%v have_best:%v no_file:%v, want 1/1/2", sAttrs["rows"], sAttrs["have_best"], sAttrs["no_file"])
	}
	if row.Message != "report item" {
		t.Errorf("row msg = %q, want %q", row.Message, "report item")
	}
	rAttrs := recordAttrs(row)
	want := map[string]any{
		"title":         "Frieren",
		"al_id":         int64(154587),
		"arr":           library.ArrSonarr,
		"verdict":       string(VerdictBest),
		"qualifier":     string(QualifierMixed),
		"scope":         "S1",
		"approx":        true,
		"current_group": "subsplease,erai-raws",
		"seadex_best":   "SubsPlease",
		// No best carries an annotation, so the notes twin is empty.
		"seadex_best_notes": "",
		"arr_url":           "http://sonarr/series/frieren",
		"seadex_url":        "https://releases.moe/154587",
		"match_source":      "id",
	}
	for k, v := range want {
		if rAttrs[k] != v {
			t.Errorf("row attr %q = %v, want %v", k, rAttrs[k], v)
		}
	}
}

// TestReportLogAlreadyCanceledEmitsNothing pins the pre-summary cancellation
// guard: a shutdown that cancels the report context after Scout.Report returns
// but before Log is called must not emit a complete-looking "report summary"
// line with no rows behind it - Log returns the interruption (wrapping
// context.Canceled, so main's shutdown classification keeps a routine SIGTERM
// off the ERROR alert) before any record is emitted.
func TestReportLogAlreadyCanceledEmitsNothing(t *testing.T) {
	log, rec := capture.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := craftedReport().Log(ctx, log)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Log error = %v, want context.Canceled", err)
	}
	if rec.Len() != 0 {
		t.Errorf("Log emitted %d records on an already-canceled context, want 0", rec.Len())
	}
}

func TestRenderMarkdownCountsNotOnSeaDexSeparately(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1, string(VerdictNotOnSeaDex): 2},
		Rows: []Row{
			{Title: "Matched", Arr: library.ArrSonarr, Verdict: VerdictBest},
			{Title: "GapA", Arr: library.ArrSonarr, Verdict: VerdictNotOnSeaDex},
			{Title: "GapB", Arr: library.ArrSonarr, Verdict: VerdictNotOnSeaDex},
		},
	}

	md := renderMarkdown(r)

	if !strings.Contains(md, "1 anime with a SeaDex match") {
		t.Errorf("header must count only matched rows, got: %s", md[:120])
	}
	if !strings.Contains(md, "2 more in your library that SeaDex does not list") {
		t.Errorf("header must mention the not_on_seadex count, got: %s", md[:200])
	}
}

func TestLinksDedupesRepeatedBestAndLabelsUnnamedTracker(t *testing.T) {
	row := &Row{Releases: []Release{
		{Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{Best: true, Tracker: "  ", URL: "https://example.org/t"},
		// Neither a known host, a known label, nor any host at all: the only
		// shape left with nothing to name it, so the "link" last resort holds.
		{Best: true, Tracker: "  ", URL: "http://"},
		// A delimiter-bearing pair: with a string-concatenated dedupe key these
		// two distinct (tracker, URL) tuples collide ("Nyaa|https://x/a" +
		// "https://one.example" == "Nyaa" + "https://x/a|https://one.example")
		// and one link is silently dropped; the structural key keeps both.
		{Best: true, Tracker: "Nyaa|https://x/a", URL: "https://one.example"},
		{Best: true, Tracker: "Nyaa", URL: "https://x/a|https://one.example"},
	}}

	got := links(row)

	if strings.Count(got, "https://nyaa.si/view/1") != 1 {
		t.Errorf("repeated (tracker, URL) best link must appear once, got %q", got)
	}
	// An unlabelled link is named by its own host through tracker.CanonicalName
	// (the ladder the daemon's alert attributes share), so the report says where
	// the link goes instead of the anonymous "link".
	if !strings.Contains(got, "[example.org](https://example.org/t)") {
		t.Errorf("a blank tracker must be labelled by the URL host, got %q", got)
	}
	// The last-resort "link" label still applies, but "http://" carries no host
	// at all (urlform reads it as a hidden-host form), which the shared
	// structural vouch step refuses - so the cell is the plain label rather than
	// an active link to a destination a browser resolves differently (h-f8).
	if !strings.Contains(got, linkSep+"link"+linkSep) {
		t.Errorf("a link with no nameable tracker must fall back to the %q label, got %q", "link", got)
	}
	if strings.Contains(got, "](http://)") {
		t.Errorf("an empty-host destination must not render as an active link, got %q", got)
	}
	// Both delimiter-bearing tuples survive as distinct links: the plain URL
	// as its own destination, and the pipe-bearing URL with the pipe
	// percent-encoded by escapeLinkURL.
	if !strings.Contains(got, "](https://one.example)") {
		t.Errorf("distinct tuple with the delimiter in the tracker was dropped, got %q", got)
	}
	if !strings.Contains(got, "](https://x/a%7Chttps://one.example)") {
		t.Errorf("distinct tuple with the delimiter in the URL was dropped, got %q", got)
	}
}

// craftedReport builds a report whose untrusted strings carry C1 terminal
// escape introducers (CSI U+009B, OSC U+009D, ST U+009C) and Unicode bidi
// controls, for the machine-readable output sanitization tests.
func craftedReport() *Report {
	return &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictUnlisted): 1},
		Rows: []Row{{
			Title:         "Evil\u009bShow\u202e",
			Arr:           "sonarr",
			Verdict:       VerdictUnlisted,
			ArrURL:        "http://sonarr/series/x\u009d",
			SeaDexURL:     "https://releases.moe/1\u200f",
			MatchSource:   "id\u061c",
			CurrentGroups: []string{"grp\u009c"},
			Releases:      []Release{{Group: "g\u0090", Tracker: "trk\u200e", URL: "https://x/\u2028a", Best: true}},
		}},
	}
}

// unsafeOutputRunes are the runes no machine-readable output may carry raw:
// C1 terminal-escape introducers, bidi controls, and line separators.
var unsafeOutputRunes = []rune{'\u009b', '\u009c', '\u009d', '\u0090', '\u202e', '\u200e', '\u200f', '\u061c', '\u2028'}

// TestRenderJSONSanitizesControlAndBidiRunes pins the JSON copy's output
// encoding: encoding/json passes C1 and bidi runes through raw, so renderJSON
// must serialize a sanitized copy — and must not mutate the canonical report.
func TestRenderJSONSanitizesControlAndBidiRunes(t *testing.T) {
	r := craftedReport()

	data, err := renderJSON(r)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}

	for _, bad := range unsafeOutputRunes {
		if strings.ContainsRune(string(data), bad) {
			t.Errorf("renderJSON output carries raw unsafe rune U+%04X", bad)
		}
	}
	if r.Rows[0].Title != "Evil\u009bShow\u202e" || r.Rows[0].CurrentGroups[0] != "grp\u009c" || r.Rows[0].Releases[0].Group != "g\u0090" {
		t.Error("renderJSON mutated the canonical report; it must sanitize a copy")
	}
}

// TestReportLogSanitizesControlAndBidiRunes pins the slog path's output
// encoding: the JSONHandler escapes C0 but emits C1/bidi runes raw, so every
// row-derived string logged by Report.Log must be sanitized first.
func TestReportLogSanitizesControlAndBidiRunes(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	r := craftedReport()

	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "report item") {
		t.Fatalf("Log emitted no report item line: %q", out)
	}
	for _, bad := range unsafeOutputRunes {
		if strings.ContainsRune(out, bad) {
			t.Errorf("Report.Log output carries raw unsafe rune U+%04X", bad)
		}
	}
}

// TestSanitizersCoverBidiAndSeparatorRunes pins the complete unsafe-format-
// rune set on both Markdown sanitizers: every Unicode Bidi_Control character
// (including the U+061C/U+200E/U+200F singleton marks the contiguous ranges
// miss) and the U+2028/U+2029 line separators must be replaced with a space in
// cell text and link labels (escapeCell) and percent-encoded byte-by-byte in
// link destinations (escapeLinkURL) — never emitted raw, where they could
// reorder rendered text or break a table row.
func TestSanitizersCoverBidiAndSeparatorRunes(t *testing.T) {
	runes := []rune{
		'\u061c',                                         // ALM (singleton bidi mark)
		'\u200e',                                         // LRM (singleton bidi mark)
		'\u200f',                                         // RLM (singleton bidi mark)
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e', // LRE/RLE/PDF/LRO/RLO
		'\u2066', '\u2067', '\u2068', '\u2069', // LRI/RLI/FSI/PDI
		'\u2028', '\u2029', // line/paragraph separators (row-boundary break)
	}
	for _, r := range runes {
		t.Run(fmt.Sprintf("U+%04X", r), func(t *testing.T) {
			// Cell text (the same path sanitizes link labels via mdLink).
			in := "a" + string(r) + "b"
			if got := escapeCell(in); got != "a b" {
				t.Errorf("escapeCell(%q) = %q, want %q (unsafe rune replaced with a space)", in, got, "a b")
			}
			// Link destination: the rune's UTF-8 bytes percent-encoded.
			got := escapeLinkURL("https://x/a" + string(r) + "b")
			if strings.ContainsRune(got, r) {
				t.Errorf("escapeLinkURL left U+%04X raw: %q", r, got)
			}
			var enc strings.Builder
			for _, byt := range []byte(string(r)) {
				fmt.Fprintf(&enc, "%%%02X", byt)
			}
			if want := "https://x/a" + enc.String() + "b"; got != want {
				t.Errorf("escapeLinkURL(U+%04X) = %q, want %q", r, got, want)
			}
		})
	}
}

// TestRenderMarkdownOmitsNotOnSeaDexClauseWhenZero pins the header's other
// half: with no not_on_seadex rows the "; N more in your library" clause must
// be absent entirely (relaxing the notOnSeaDex > 0 guard would emit
// "; 0 more in your library that SeaDex does not list").
func TestRenderMarkdownOmitsNotOnSeaDexClauseWhenZero(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows:        []Row{{Title: "Matched", Arr: "sonarr", Verdict: VerdictBest}},
	}

	md := renderMarkdown(r)

	if strings.Contains(md, "more in your library") {
		t.Errorf("header must omit the not_on_seadex clause when the count is zero, got: %s", md[:200])
	}
}

func TestRenderMarkdownEscapesUntrustedRowText(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictUnlisted): 1},
		Rows: []Row{{
			Title:         "Evil|Show <img src=x>",
			Arr:           "sonarr",
			Verdict:       VerdictUnlisted,
			CurrentGroups: []string{"bad|group"},
			Releases:      []Release{{Group: "best[grp]", Best: true}},
		}},
	}

	md := renderMarkdown(r)

	for _, raw := range []string{"Evil|Show", "<img", "bad|group", "best[grp]"} {
		if strings.Contains(md, raw) {
			t.Errorf("renderMarkdown() leaked unescaped untrusted text %q", raw)
		}
	}
	for _, want := range []string{"Evil&#124;Show &lt;img src=x&gt;", "bad&#124;group", "best&#91;grp&#93;"} {
		if !strings.Contains(md, want) {
			t.Errorf("renderMarkdown() missing escaped form %q", want)
		}
	}
}

// TestSanitizeDisplayTextReplacesC0AndDELPreservesCRLF pins the documented
// contract of the machine-readable-output sanitizer on the branches the C1/
// bidi tests do not reach: every C0 control except CR/LF (which both encoders
// escape) and DEL are replaced with a space, CR/LF pass through, and plain
// text is unchanged.
func TestSanitizeDisplayTextReplacesC0AndDELPreservesCRLF(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"C0 escape introducer", "a\x1b[2Jb", "a [2Jb"},
		{"C0 NUL", "a\x00b", "a b"},
		{"C0 BEL", "a\x07b", "a b"},
		{"tab", "a\tb", "a b"},
		{"DEL", "a\x7fb", "a b"},
		{"LF preserved", "a\nb", "a\nb"},
		{"CR preserved", "a\rb", "a\rb"},
		{"plain text unchanged", "Frieren: Beyond Journey's End", "Frieren: Beyond Journey's End"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeDisplayText(tt.in); got != tt.want {
				t.Errorf("sanitizeDisplayText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRenderJSONNilRowsIsEmptyArray pins the JSON shape of a nil-Rows Report:
// "rows" renders as [] (the pre-existing contract; Rows has no omitempty),
// never null - sanitizeOutput's slices.Clone of a nil slice is nil, which
// would otherwise marshal as null and change the machine-readable contract.
func TestRenderJSONNilRowsIsEmptyArray(t *testing.T) {
	r := &Report{GeneratedAt: time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)}
	data, err := renderJSON(r)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	if !strings.Contains(string(data), `"rows": []`) {
		t.Errorf("renderJSON of a nil-Rows report = %s, want \"rows\": []", data)
	}
	if strings.Contains(string(data), `"rows": null`) {
		t.Errorf("renderJSON of a nil-Rows report rendered null rows: %s", data)
	}
}

// TestRenderIncompleteSectionAndCaveat pins the degraded report's rendered
// shape: the Markdown header carries the
// completeness caveat, the affected entries render under the "incomplete
// (transient AniList failure)" header with their AniList ids and releases.moe
// links, and the JSON carries the same list under incomplete_mappings.
func TestRenderIncompleteSectionAndCaveat(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows:        []Row{{Title: "Matched", Arr: "sonarr", Verdict: VerdictBest}},
		Incomplete: []IncompleteEntry{
			{SeaDexURL: "https://releases.moe/20791", AniListID: 20791},
			{SeaDexURL: "https://releases.moe/99999", AniListID: 99999},
		},
	}

	md := renderMarkdown(r)
	if !strings.Contains(md, "**Caveat: this report is incomplete.** 2 SeaDex entries could not be resolved against AniList") {
		t.Errorf("markdown header is missing the completeness caveat:\n%s", md[:400])
	}
	if !strings.Contains(md, "## incomplete (transient AniList failure) (2)") {
		t.Errorf("markdown is missing the incomplete-mapping section header:\n%s", md)
	}
	for _, want := range []string{"| 20791 | [seadex](https://releases.moe/20791) |", "| 99999 | [seadex](https://releases.moe/99999) |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown incomplete section missing row %q:\n%s", want, md)
		}
	}
	// The matched rows still render: incompleteness annotates, never withholds.
	if !strings.Contains(md, "Matched") {
		t.Error("markdown lost the verdict rows on a degraded report")
	}

	data, err := renderJSON(r)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	if !strings.Contains(string(data), `"incomplete_mappings"`) {
		t.Errorf("JSON is missing the incomplete_mappings key: %s", data)
	}
	if !strings.Contains(string(data), `"al_id": 20791`) {
		t.Errorf("JSON incomplete_mappings is missing the affected al_id: %s", data)
	}
}

// TestRenderSingularIncompleteCaveat pins the caveat's singular form so a
// one-entry degradation does not read "1 SeaDex entries".
func TestRenderSingularIncompleteCaveat(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{},
		Incomplete:  []IncompleteEntry{{SeaDexURL: "https://releases.moe/7", AniListID: 7}},
	}
	md := renderMarkdown(r)
	if !strings.Contains(md, "1 SeaDex entry could not be resolved against AniList") {
		t.Errorf("markdown caveat missing the singular form:\n%s", md[:300])
	}
}

// TestRenderCompleteReportOmitsIncompleteSection pins the healthy path's
// unchanged-output contract (the package keeps no golden file, so absence is
// pinned directly): with no incomplete
// mappings the Markdown carries neither the caveat nor the section header and
// the JSON omits the incomplete_mappings key entirely, so a fully healthy
// report renders byte-identically to the pre-section format - and a total
// AniList outage that affected no entry (an empty set) renders the same.
func TestRenderCompleteReportOmitsIncompleteSection(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows:        []Row{{Title: "Matched", Arr: "sonarr", Verdict: VerdictBest}},
	}

	md := renderMarkdown(r)
	for _, absent := range []string{"Caveat", "incomplete (transient AniList failure)"} {
		if strings.Contains(md, absent) {
			t.Errorf("healthy report markdown must not contain %q:\n%s", absent, md)
		}
	}

	data, err := renderJSON(r)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	if strings.Contains(string(data), "incomplete_mappings") {
		t.Errorf("healthy report JSON must omit the incomplete_mappings key: %s", data)
	}
}

// TestRenderJSONSanitizesIncompleteEntries extends the machine-readable
// sanitization contract to the incomplete-mapping section: a crafted URL
// carrying C1/bidi runes is sanitized in the JSON copy without mutating the
// canonical report.
func TestRenderJSONSanitizesIncompleteEntries(t *testing.T) {
	crafted := "https://releases.moe/1\u009b\u202e"
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{},
		Incomplete:  []IncompleteEntry{{SeaDexURL: crafted, AniListID: 1}},
	}

	data, err := renderJSON(r)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	for _, bad := range []rune{'\u009b', '\u202e'} {
		if strings.ContainsRune(string(data), bad) {
			t.Errorf("renderJSON incomplete_mappings carries raw unsafe rune U+%04X", bad)
		}
	}
	if r.Incomplete[0].SeaDexURL != crafted {
		t.Error("renderJSON mutated the canonical report's incomplete entries; it must sanitize a copy")
	}
}

// TestDisplayBestGroupsAnnotatesWarned pins the SeaDex-best column's contract
// after the notes split (l-f192): the column carries upstream group text only,
// an unwarned best of the same group wins the dedupe (a group genuinely
// available as best never displays as the annotated one), and the warned
// group's canonical tags are rendered by the Notes column instead, joined in
// canonical order and positionally aligned with the best column.
func TestDisplayBestGroupsAnnotatesWarned(t *testing.T) {
	rels := []Release{
		{Group: "PMR", Best: true, Warnings: []string{"broken"}},
		{Group: "pmr", Best: true},
		{Group: "SEV", Best: true, Warnings: []string{"broken", "incomplete"}},
	}
	got := displayBestGroups(rels)
	want := []string{"pmr", "SEV"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("displayBestGroups() = %v, want %v", got, want)
	}
	if got, want := notesCell(&Row{Releases: rels}), "-; broken, incomplete"; got != want {
		t.Errorf("notesCell() = %q, want %q", got, want)
	}
}

// TestBestAndNotesColumnsDoNotShareANamespace pins the l-f192 fix at its
// strongest point: the forgery is dead because the two namespaces no longer
// share a display string. A clean best named `SEV (broken)` - and its
// quote-carrying escalation `SEV" (broken) "x`, which defeated the earlier
// quoting fix by forging the quote boundary itself - both render as plain
// upstream text in the best column with an EMPTY Notes cell, so neither can
// produce the rendering of a genuinely broken release (group in the best
// column, `broken` in Notes).
func TestBestAndNotesColumnsDoNotShareANamespace(t *testing.T) {
	forgeries := []string{"SEV (broken)", `SEV" (broken) "x`}
	for _, group := range forgeries {
		row := &Row{Releases: []Release{{Group: group, Best: true}}}
		if got, want := displayBestGroups(row.Releases), []string{group}; !reflect.DeepEqual(got, want) {
			t.Errorf("displayBestGroups(%q) = %v, want %v (upstream text, verbatim)", group, got, want)
		}
		if got := notesCell(row); got != emptyCell {
			t.Errorf("notesCell(%q) = %q, want %q (a forged group annotates nothing)", group, got, emptyCell)
		}
	}

	genuine := &Row{Releases: []Release{{Group: "PMR", Best: true, Warnings: []string{"broken"}}}}
	if got, want := bestCell(genuine), "PMR"; got != want {
		t.Errorf("bestCell(genuinely broken) = %q, want %q", got, want)
	}
	if got, want := notesCell(genuine), "broken"; got != want {
		t.Errorf("notesCell(genuinely broken) = %q, want %q", got, want)
	}
	for _, group := range forgeries {
		if got := bestCell(&Row{Releases: []Release{{Group: group, Best: true}}}); got == bestCell(genuine) && notesCell(genuine) == emptyCell {
			t.Errorf("forged group %q reproduced the genuine rendering", group)
		}
	}
}

// TestNotesCellAssociatesNotesWithTheirGroupByPosition pins the multi-release
// association the split has to preserve: with two best releases where only one
// is annotated, the Notes cell carries one `;`-separated entry per listed group
// in the same order, with the empty marker for the unannotated one - so a
// reader can tell WHICH group the note belongs to without the group name ever
// re-entering the annotation string.
func TestNotesCellAssociatesNotesWithTheirGroupByPosition(t *testing.T) {
	row := &Row{Releases: []Release{
		{Group: "SubsPlease", Best: true},
		{Group: "PMR", Best: true, Warnings: []string{"broken"}, Unobtainable: true},
	}}
	if got, want := bestCell(row), "SubsPlease, PMR"; got != want {
		t.Errorf("bestCell() = %q, want %q", got, want)
	}
	if got, want := notesCell(row), "-; broken, unobtainable"; got != want {
		t.Errorf("notesCell() = %q, want %q", got, want)
	}
}

// TestDisplayBestGroupsAnnotatesUnobtainable pins the obtainability marker
// across the split columns: an unobtainable best keeps its group in the best
// column and carries "unobtainable" in the Notes column (so the rendered facts
// explain why the verdict ignored a visible best), an obtainable best of the
// same group wins the dedupe, and a best that is both warned and unobtainable
// joins its notes without mutating Release.Warnings.
func TestDisplayBestGroupsAnnotatesUnobtainable(t *testing.T) {
	warnings := []string{"broken"}
	rels := []Release{
		{Group: "PMR", Best: true, Unobtainable: true},
		{Group: "pmr", Best: true},
		{Group: "SEV", Best: true, Warnings: warnings, Unobtainable: true},
		{Group: "A&C", Best: true, Unobtainable: true},
	}
	got := displayBestGroups(rels)
	want := []string{"pmr", "SEV", "A&C"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("displayBestGroups() = %v, want %v", got, want)
	}
	if got, want := notesCell(&Row{Releases: rels}), "-; broken, unobtainable; unobtainable"; got != want {
		t.Errorf("notesCell() = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(warnings, []string{"broken"}) {
		t.Errorf("Warnings = %v, want [broken] (annotation must not mutate the release)", warnings)
	}
}

// TestRenderUnobtainableBestAnnotatedInBothProjections pins the rendered
// contract for a SeaDex-listed but unobtainable best: obtainability keeps
// controlling the verdict, and BOTH
// projections surface the divergence - the Markdown SeaDex-best column
// carries the "(unobtainable)" annotation and does NOT offer the release as
// a grab link (the releases.moe link still renders), while the JSON release
// carries an explicit "unobtainable": true marker so machine consumers can
// see why the visible best was ignored. An obtainable release's JSON shape
// is unchanged (the marker is omitted).
func TestRenderUnobtainableBestAnnotatedInBothProjections(t *testing.T) {
	rep := &Report{
		GeneratedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Totals:      map[string]int{string(VerdictUnlisted): 1},
		Rows: []Row{{
			Title:         "Unobtainable Show",
			Arr:           "sonarr",
			SeaDexURL:     "https://releases.moe/11",
			Verdict:       VerdictUnlisted,
			CurrentGroups: []string{"other"},
			Releases: []Release{
				{
					Tracker: "Nyaa", Group: "PMR", URL: "https://nyaa.si/view/901",
					Best: true, Unobtainable: true,
				},
				{Tracker: "Nyaa", Group: "Erai", URL: "https://nyaa.si/view/902"},
			},
			AniListID: 11,
		}},
	}

	md := renderMarkdown(rep)
	if !strings.Contains(md, `| PMR | unobtainable |`) {
		t.Errorf("markdown lacks the unobtainable-best annotation `| PMR | unobtainable |`:\n%s", md)
	}
	if strings.Contains(md, "https://nyaa.si/view/901") {
		t.Errorf("markdown offers the unobtainable release as a grab link:\n%s", md)
	}
	if !strings.Contains(md, "https://releases.moe/11") {
		t.Errorf("markdown lost the SeaDex entry link:\n%s", md)
	}

	data, err := renderJSON(rep)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	if !strings.Contains(string(data), `"unobtainable": true`) {
		t.Errorf("JSON is missing the unobtainable marker on the excluded best: %s", data)
	}
	if n := strings.Count(string(data), `"unobtainable"`); n != 1 {
		t.Errorf("JSON carries %d unobtainable keys, want 1 (omitted on an obtainable release): %s", n, data)
	}
}

// TestRenderMarkdownWarnedBestAnnotatedNotLinked pins the rendered contract
// for a curation-warned best: the group stays in the SeaDex-best column and the
// warning rides the adjacent Notes column (`| PMR | broken |`) so the row stays
// complete and self-explanatory, while the links cell does NOT offer the
// warned release as a grab link (the releases.moe link still renders). The
// needle carries the cell pipes so it cannot be satisfied by the legend, which
// teaches the same spelling.
func TestRenderMarkdownWarnedBestAnnotatedNotLinked(t *testing.T) {
	rep := &Report{
		GeneratedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Totals:      map[string]int{string(VerdictUnlisted): 1},
		Rows: []Row{{
			Title:         "Warned Show",
			Arr:           "sonarr",
			SeaDexURL:     "https://releases.moe/10",
			Verdict:       VerdictUnlisted,
			CurrentGroups: []string{"pmr"},
			Releases: []Release{{
				Tracker: "Nyaa", Group: "PMR", URL: "https://nyaa.si/view/900",
				Best: true, Warnings: []string{"broken"},
			}},
			AniListID: 10,
		}},
	}
	md := renderMarkdown(rep)
	if !strings.Contains(md, `| PMR | broken |`) {
		t.Errorf("markdown lacks the warned-best annotation `| PMR | broken |`:\n%s", md)
	}
	if strings.Contains(md, "https://nyaa.si/view/900") {
		t.Errorf("markdown offers the warned release as a grab link:\n%s", md)
	}
	if !strings.Contains(md, "https://releases.moe/10") {
		t.Errorf("markdown lost the SeaDex entry link:\n%s", md)
	}
}

// cancelAfterHandler wraps a slog.Handler and cancels the given context while
// handling the after-th record, so a test can deterministically cancel the
// report context mid-emission. Log drives it from one goroutine, so the
// counter needs no synchronization; Log never calls WithAttrs/WithGroup, so
// the embedded-handler passthrough is safe.
type cancelAfterHandler struct {
	slog.Handler
	cancel  context.CancelFunc
	after   int
	handled int
}

func (h *cancelAfterHandler) Handle(ctx context.Context, r slog.Record) error {
	h.handled++
	if h.handled == h.after {
		h.cancel()
	}
	return h.Handler.Handle(ctx, r)
}

// TestReportLogCanceledMidRowsStopsEmitting pins Report.Log's per-row
// cancellation checkpoint: cancellation observed between row records stops
// the loop with the report-log stage error, so a shutdown does not spend its
// grace period synchronously emitting the remaining rows.
func TestReportLogCanceledMidRowsStopsEmitting(t *testing.T) {
	base, rec := capture.New()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel during the FIRST row record (record 2: summary is record 1), so
	// the per-row checkpoint fires before row 2 and the remaining rows are
	// never emitted - a shutdown must not spend its grace period on the loop.
	log := slog.New(&cancelAfterHandler{Handler: base.Handler(), cancel: cancel, after: 2})
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 3},
		Rows: []Row{
			{Title: "A", Arr: "sonarr", Verdict: VerdictBest},
			{Title: "B", Arr: "sonarr", Verdict: VerdictBest},
			{Title: "C", Arr: "sonarr", Verdict: VerdictBest},
		},
	}

	err := r.Log(ctx, log)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Log error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "report log interrupted") {
		t.Errorf("error = %q, want the report-log stage context", err)
	}
	if rec.Len() != 2 {
		t.Errorf("Log emitted %d records, want 2 (summary + first row; cancellation stops the loop)", rec.Len())
	}
}

// TestRowsWithVerdict pins the section filter directly: only rows carrying
// the requested verdict are returned, in their original order, and a verdict
// with no rows yields nil. Pinned directly because every existing
// renderMarkdown assertion is a whole-document Contains, which stays green
// when the filter's equality check inverts (rows merely land under the wrong
// section), so membership must be asserted here.
func TestRowsWithVerdict(t *testing.T) {
	rows := []Row{
		{Title: "a", Verdict: VerdictBest},
		{Title: "b", Verdict: VerdictAlt},
		{Title: "c", Verdict: VerdictBest},
	}
	got := rowsWithVerdict(rows, VerdictBest)
	if len(got) != 2 || got[0].Title != "a" || got[1].Title != "c" {
		t.Errorf("rowsWithVerdict(best) = %+v, want the a and c rows in order", got)
	}
	if got := rowsWithVerdict(rows, VerdictNoFile); got != nil {
		t.Errorf("rowsWithVerdict(no_file) = %+v, want nil", got)
	}
}

// TestRenderMarkdownVerdictSectionDescription pins the one-line explanation
// under a verdict section header: the section renders its verdictDesc text
// between the heading and the table. Asserted directly because no other test
// asserts any description text, so inverting the verdictDesc guard (never
// printing a description) would otherwise stay green.
func TestRenderMarkdownVerdictSectionDescription(t *testing.T) {
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Totals:      map[string]int{string(VerdictBest): 1},
		Rows:        []Row{{Title: "Matched", Arr: "sonarr", Verdict: VerdictBest}},
	}

	md := renderMarkdown(r)

	want := "## have_best (1)\n\nYou already have SeaDex's best release.\n\n"
	if !strings.Contains(md, want) {
		t.Errorf("markdown section is missing its description line %q:\n%s", want, md)
	}
}

// TestReportLogRendersAnnotatedBestAttribute pins the seadex_best aggregate's
// clean-first case-insensitive dedupe and the split notes twin through the
// public Log projection: a clean best wins the dedupe over a differently-cased
// annotated twin, seadex_best carries upstream group text only, and
// seadex_best_notes carries one positional entry per listed group. Deleting the
// notes attribute or letting notes leak back into seadex_best fails the exact-
// value assertions.
func TestReportLogRendersAnnotatedBestAttribute(t *testing.T) {
	log, rec := capture.New()
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Rows: []Row{{
			Releases: []Release{
				{Group: "pmr", Best: true, Warnings: []string{"broken"}},
				{Group: "PMR", Best: true},
				{Group: "SEV", Best: true, Warnings: []string{"broken"}, Unobtainable: true},
			},
		}},
	}

	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}

	attrs := recordAttrs(rec.Records()[1])
	if got, want := attrs["seadex_best"], "PMR,SEV"; got != want {
		t.Errorf("seadex_best = %q, want %q", got, want)
	}
	if got, want := attrs["seadex_best_notes"], "-;broken, unobtainable"; got != want {
		t.Errorf("seadex_best_notes = %q, want %q", got, want)
	}
}

// TestReportLogSplitsNotesFromForgedBestGroup pins the l-f192 fix on the Loki
// side (l-f192): a forged group `SEV (broken)` reaches seadex_best as upstream
// text verbatim and this app adds nothing to that value - no quotes, no
// parentheses of its own - while the genuine annotation rides
// seadex_best_notes, positionally aligned with the groups. The Markdown pair
// renders the identical split, so one fact keeps one rendering across both
// sinks. No shipped alert rule reads either attribute, so the wire shape is
// this report's own.
func TestReportLogSplitsNotesFromForgedBestGroup(t *testing.T) {
	log, rec := capture.New()
	releases := []Release{
		{Group: "SEV (broken)", Best: true},
		{Group: "PMR", Best: true, Warnings: []string{"broken"}},
	}
	r := &Report{GeneratedAt: time.Unix(0, 0).UTC(), Rows: []Row{{Releases: releases}}}

	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}

	attrs := recordAttrs(rec.Records()[1])
	wantBest := "SEV (broken),PMR"
	if got := attrs["seadex_best"]; got != wantBest {
		t.Errorf("seadex_best = %q, want %q", got, wantBest)
	}
	if got, want := attrs["seadex_best_notes"], "-;broken"; got != want {
		t.Errorf("seadex_best_notes = %q, want %q", got, want)
	}
	// The parentheses in seadex_best are the forged group's own bytes; the app
	// contributes none of its own to that value, and never a quote.
	if got, _ := attrs["seadex_best"].(string); strings.ContainsAny(strings.ReplaceAll(got, "SEV (broken)", ""), `"()`) {
		t.Errorf("seadex_best = %q carries app-added quotes or parentheses", got)
	}
	// A clean row's attribute carries neither, at all.
	clean := &Report{GeneratedAt: time.Unix(0, 0).UTC(), Rows: []Row{{
		Releases: []Release{{Group: "PMR", Best: true, Warnings: []string{"broken"}}},
	}}}
	cleanLog, cleanRec := capture.New()
	if err := clean.Log(t.Context(), cleanLog); err != nil {
		t.Fatalf("Log: %v", err)
	}
	cleanAttrs := recordAttrs(cleanRec.Records()[1])
	if got, _ := cleanAttrs["seadex_best"].(string); got != "PMR" || strings.ContainsAny(got, `"()`) {
		t.Errorf("seadex_best = %q, want %q with no quotes or parentheses", got, "PMR")
	}
	if got, want := cleanAttrs["seadex_best_notes"], "broken"; got != want {
		t.Errorf("seadex_best_notes = %q, want %q", got, want)
	}
	// Both renderings agree: the Markdown pair splits the same way.
	row := &Row{Releases: releases}
	if got := strings.Join(displayBestGroups(releases), ","); got != wantBest {
		t.Errorf("markdown best cell = %q, want %q (both renderings must agree)", got, wantBest)
	}
	if got, want := notesCell(row), "-; broken"; got != want {
		t.Errorf("markdown notes cell = %q, want %q", got, want)
	}
}

// TestReportLogCapsAggregateAttributes pins the per-attribute volume bound on
// ALL THREE aggregates (current_group, seadex_best, seadex_best_notes): an
// oversized upstream group name is cut on a rune boundary and marked, so one
// report line cannot balloon past the log pipeline's limit. The notes twin
// carries only this app's vocabulary, so its unbounded axis is the ENTRY COUNT
// (an entry admits up to 512 torrents), which the same joiner budget bounds.
// Removing any cap fails the length or the suffix assertion.
func TestReportLogCapsAggregateAttributes(t *testing.T) {
	log, rec := capture.New()
	// Enough annotated bests that the positional notes list alone exceeds the
	// budget: each contributes a distinct group plus "broken" and a separator.
	many := make([]Release, 0, 2048)
	for i := range 2048 {
		many = append(many, Release{Group: "g" + strconv.Itoa(i), Best: true, Warnings: []string{"broken"}})
	}
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Rows: []Row{
			{
				CurrentGroups: []string{strings.Repeat("x", maxAttrBytes+1)},
				Releases:      []Release{{Group: strings.Repeat("y", maxAttrBytes+1), Best: true}},
			},
			{Releases: many},
		},
	}

	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}

	attrs := recordAttrs(rec.Records()[1])
	for _, key := range []string{"current_group", "seadex_best"} {
		got, ok := attrs[key].(string)
		if !ok {
			t.Errorf("%s = %T, want string", key, attrs[key])
			continue
		}
		if len(got) != maxAttrBytes+3 {
			t.Errorf("len(%s) = %d, want %d", key, len(got), maxAttrBytes+3)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("%s = %q, want truncation suffix", key, got)
		}
	}
	notes, ok := recordAttrs(rec.Records()[2])["seadex_best_notes"].(string)
	if !ok {
		t.Fatalf("seadex_best_notes = %T, want string", recordAttrs(rec.Records()[2])["seadex_best_notes"])
	}
	if len(notes) != maxAttrBytes+3 || !strings.HasSuffix(notes, "...") {
		t.Errorf("seadex_best_notes len = %d suffix-marked = %t, want %d and true (bounded by entry count)",
			len(notes), strings.HasSuffix(notes, "..."), maxAttrBytes+3)
	}
}

// TestReportLogEmitsIncompleteMappings pins the slog projection of the
// incomplete-mapping section, which the Markdown and JSON projections already
// cover: the summary's incomplete_mappings count plus the per-entry message,
// AniList ID, and SeaDex URL. Deleting the incomplete loop or its summary count
// leaves the other slog tests green; this one fails.
func TestReportLogEmitsIncompleteMappings(t *testing.T) {
	log, rec := capture.New()
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Incomplete: []IncompleteEntry{{
			AniListID: 20791,
			SeaDexURL: "https://releases.moe/20791",
		}},
	}

	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}

	if rec.Len() != 2 {
		t.Fatalf("Log emitted %d records, want 2 (summary + incomplete mapping)", rec.Len())
	}
	records := rec.Records()
	summaryAttrs := recordAttrs(records[0])
	if got := summaryAttrs["incomplete_mappings"]; got != int64(1) {
		t.Errorf("incomplete_mappings = %v, want 1", got)
	}
	if got, want := records[1].Message, "report incomplete mapping"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	attrs := recordAttrs(records[1])
	if got := attrs["al_id"]; got != int64(20791) {
		t.Errorf("al_id = %v, want 20791", got)
	}
	if got, want := attrs["seadex_url"], "https://releases.moe/20791"; got != want {
		t.Errorf("seadex_url = %q, want %q", got, want)
	}
}

// TestBestGroupDedupeIsBoundedAndCaseInsensitive pins the case-insensitive
// dedupe identity both best-group renderers share: two oversized groups
// differing only in case must collapse to one in the Markdown column AND in the
// slog aggregate, whose own emitted output stays bounded.
func TestBestGroupDedupeIsBoundedAndCaseInsensitive(t *testing.T) {
	huge := strings.Repeat("g", 4*maxAttrBytes)
	releases := []Release{
		{Group: huge, Best: true},
		{Group: strings.ToUpper(huge), Best: true},
	}

	display := displayBestGroups(releases)
	if len(display) != 1 {
		t.Fatalf("displayBestGroups() returned %d groups, want 1 (case-insensitive dedupe)", len(display))
	}
	if display[0] != huge {
		t.Error("displayBestGroups() did not keep the first release's original case")
	}

	attr, _ := joinBestAttrs(releases)
	if len(attr) != maxAttrBytes+3 {
		t.Errorf("len(joinBestAttrs() groups) = %d, want %d (bounded output)", len(attr), maxAttrBytes+3)
	}
	if !strings.HasSuffix(attr, "...") {
		t.Errorf("joinBestAttrs() groups = %q..., want truncation suffix", attr[:16])
	}
	if strings.ContainsRune(attr, 'G') {
		t.Error("joinBestAttrs() groups emitted the deduped upper-case twin")
	}
}

// TestAttrBudgetMirrorsKeyBudget pins the equality maxAttrBytes used to carry
// structurally (it aliased keyenc.MaxComponentBytes before the logattr extraction).
// Both bounds apply to the same untrusted SeaDex values - the attribute budget on the
// emitted line, the component budget on the dedupe key - and logattr states the mirror
// in prose only, so nothing else catches a one-sided change.
func TestAttrBudgetMirrorsKeyBudget(t *testing.T) {
	if maxAttrBytes != keyenc.MaxComponentBytes {
		t.Errorf("maxAttrBytes = %d, want keyenc.MaxComponentBytes = %d", maxAttrBytes, keyenc.MaxComponentBytes)
	}
}

// TestFilteredReleaseIsAnnotatedAndNotLinked isolates the Filtered annotation
// leg h-f7 added to releaseNotes and annotated. The fixture deliberately carries
// NO Warnings: TestAuditExcludedTagBestNotCounted exercises the Broken tag, which
// populates Warnings and so made annotated return true even before the fix - it
// cannot fail if either Filtered check is removed. Here both user-visible
// corrections are the only thing keeping the assertions true: the "filtered"
// note that stops the row self-contradicting (best column lists the group while
// the verdict calls the on-disk copy unlisted), and the suppression of the grab
// link for a release the operator's own filters.exclude_tags policy excluded.
func TestFilteredReleaseIsAnnotatedAndNotLinked(t *testing.T) {
	rel := Release{Group: "PMR", Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1", Filtered: true}
	if len(rel.Warnings) != 0 {
		t.Fatal("fixture carries warnings; the Filtered leg would not be the reason the assertions hold")
	}
	if got := releaseNotes(&rel); !slices.Equal(got, []string{"filtered"}) {
		t.Errorf("releaseNotes() = %v, want [filtered]", got)
	}
	if !annotated(&rel) {
		t.Error("a filtered release is not annotated; it would be offered as a grab link and read as unexplained in the report")
	}
	if got := links(&Row{Releases: []Release{rel}}); got != emptyCell {
		t.Errorf("links() = %q, want %q (an excluded best must not be offered as a one-click grab)", got, emptyCell)
	}
}

// TestReleaseNotesDistinguishesURLErrorFromUnobtainable pins the report's
// upstream-data diagnostic. A SeaDex record whose url field carries a
// value the publisher refuses used to publish a plausible-looking 404 - the live
// catalogue has one, tracker AB with url "Chihiro", a release-group name typed
// into the url field, which became "https://animebytes.tv/Chihiro" - and because
// a link WAS produced it also escaped the unusable-URL accounting, so nothing
// anywhere named the problem.
//
// The link is now dropped and the row says why. It is reported separately from
// "unobtainable" on purpose: the two point the operator at different places. An
// unobtainable release is a consequence of THEIR config (a tracker they do not
// use); a url error is an upstream DATA defect they can go fix at the source. An
// empty url is not an error - SeaDex simply has no link for that release.
func TestReleaseNotesDistinguishesURLErrorFromUnobtainable(t *testing.T) {
	tests := map[string]struct {
		rel  Release
		want []string
	}{
		"healthy release carries no notes": {
			rel: Release{URL: "https://nyaa.si/view/1"},
		},
		"a refused url value is a url error": {
			rel:  Release{URLError: true},
			want: []string{"url error"},
		},
		"an unusable tracker is unobtainable, not a url error": {
			rel:  Release{URL: "https://animebytes.tv/torrents.php?torrentid=1", Unobtainable: true},
			want: []string{"unobtainable"},
		},
		"both apply, url error first as the more actionable": {
			rel:  Release{URLError: true, Unobtainable: true},
			want: []string{"url error", "unobtainable"},
		},
		"curation warnings still lead": {
			rel:  Release{Warnings: []string{"broken"}, URLError: true},
			want: []string{"broken", "url error"},
		},
		"a tracker this build does not carry is named as such": {
			rel:  Release{UnknownTracker: true},
			want: []string{"unknown tracker"},
		},
		"an unknown tracker is also unobtainable, in that order": {
			rel:  Release{UnknownTracker: true, Unobtainable: true},
			want: []string{"unknown tracker", "unobtainable"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := releaseNotes(&tc.rel)
			if !slices.Equal(got, tc.want) {
				t.Errorf("releaseNotes() = %v, want %v", got, tc.want)
			}
			// A url error must exclude the release from the verdict and the grab
			// links, exactly like the other annotation classes.
			if tc.rel.URLError && !annotated(&tc.rel) {
				t.Error("a url-error release is not annotated; it would still drive the verdict and be offered as a link")
			}
			// Same for the other refusal class: an unknown tracker yields no
			// link, so the row must not be offered as one either (l-f127).
			if tc.rel.UnknownTracker && !annotated(&tc.rel) {
				t.Error("an unknown-tracker release is not annotated; it would still drive the verdict and be offered as a link")
			}
		})
	}
}

// TestReportLogRedactsArrURLCredentials pins the slog path's credential
// posture, which only the PERSISTED halves are pinned for today: an arr
// public_url may carry URL userinfo and a query token, and every report row
// carries that link, so Report.Log must emit the library.SafeLogURL-stripped
// form. Loki retains what it ingests, so a dropped strip is an unrecoverable
// credential disclosure with nothing failing.
func TestReportLogRedactsArrURLCredentials(t *testing.T) {
	log, rec := capture.New()
	r := &Report{
		GeneratedAt: time.Unix(0, 0).UTC(),
		Rows: []Row{{
			Title:   "Frieren",
			Arr:     "sonarr",
			Verdict: VerdictBest,
			ArrURL:  "https://admin:hunter2@sonarr.example/series/frieren?apikey=tok3n#frag",
		}},
	}

	if err := r.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}

	if rec.Len() != 2 {
		t.Fatalf("Log emitted %d records, want 2 (summary + one row)", rec.Len())
	}
	attrs := recordAttrs(rec.Records()[1])
	got, ok := attrs["arr_url"].(string)
	if !ok {
		t.Fatalf("arr_url = %T, want string", attrs["arr_url"])
	}
	if want := "https://sonarr.example/series/frieren"; got != want {
		t.Errorf("arr_url = %q, want %q", got, want)
	}
	for _, secret := range []string{"admin", "hunter2", "tok3n", "frag"} {
		if strings.Contains(got, secret) {
			t.Errorf("arr_url = %q, carries credential material %q", got, secret)
		}
	}
}

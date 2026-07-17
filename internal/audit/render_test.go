package audit

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/align"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

func TestScopeLabel(t *testing.T) {
	tests := []struct {
		name string
		want string
		row  Row
	}{
		{"movie", "movie", Row{scope: align.ScopeMovie}},
		{"special", "special", Row{scope: align.ScopeSpecial}},
		{"numbered season", "S2", Row{scope: align.ScopeSeason, Season: 2}},
		{"whole series", "series", Row{scope: align.ScopeWholeSeries}},
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
	if got := scopeCell(&Row{scope: align.ScopeSeason, Season: 2, Approx: true}); got != "S2 (approx)" {
		t.Errorf("scopeCell() = %q, want \"S2 (approx)\"", got)
	}
	if got := scopeCell(&Row{scope: align.ScopeSeason, Season: 2}); got != "S2" {
		t.Errorf("scopeCell() = %q, want \"S2\"", got)
	}
	if got := scopeCell(&Row{scope: align.ScopeSeason, Season: 2, Qualifier: QualifierMixed}); got != "S2 (mixed)" {
		t.Errorf("scopeCell() = %q, want \"S2 (mixed)\"", got)
	}
	if got := scopeCell(&Row{scope: align.ScopeSeason, Season: 2, Approx: true, Qualifier: QualifierTheoretical}); got != "S2 (approx, theoretical)" {
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
		t.Errorf("displayBestGroups() = %v, want [SubsPlease] (best-only, case-insensitive dedupe, original case)", got)
	}
}

func TestGroupSets(t *testing.T) {
	rels := []Release{
		{Group: "SubsPlease", Best: true},
		{Group: "subsplease", Best: true},
		{Group: "Erai", Best: false},
	}
	best, alt := groupSets(rels)
	if !reflect.DeepEqual(best, []string{"subsplease"}) {
		t.Errorf("best = %v, want [subsplease]", best)
	}
	if !reflect.DeepEqual(alt, []string{"erai"}) {
		t.Errorf("alt = %v, want [erai]", alt)
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

func TestMdLinkAllowsOnlyHTTPSchemes(t *testing.T) {
	// http/https destinations render as a Markdown link.
	if got := mdLink("nyaa", "https://nyaa.si/view/1"); got != "[nyaa](https://nyaa.si/view/1)" {
		t.Errorf("mdLink(https) = %q", got)
	}
	// Active non-http schemes and relative/unparseable destinations degrade to
	// the escaped label as plain text (no active link injected).
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,<script>", "/torrents.php?id=1"} {
		got := mdLink("label", bad)
		if strings.Contains(got, "](") {
			t.Errorf("mdLink(%q) = %q, must not emit a link", bad, got)
		}
		if got != "label" {
			t.Errorf("mdLink(%q) = %q, want plain escaped label %q", bad, got, "label")
		}
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
	link := mdLink("nyaa", `https://x/path\`)
	if want := "[nyaa](https://x/path%5C)"; link != want {
		t.Errorf("mdLink(trailing backslash) = %q, want %q", link, want)
	}
	// A backtick could open a code span across the ']( ' boundary; it must be
	// percent-encoded in the destination.
	got = escapeLinkURL("https://x/a`b")
	if want := "https://x/a%60b"; got != want {
		t.Errorf("escapeLinkURL(backtick) = %q, want %q", got, want)
	}
}

func TestClassifyReleasesGatesAnimeBytes(t *testing.T) {
	entry := &seadex.Entry{Torrents: []seadex.Torrent{
		{Tracker: "Nyaa", ReleaseGroup: "SubsPlease", IsBest: true, URL: "https://nyaa.si/view/1"},
		{Tracker: "AB", ReleaseGroup: "Commie", IsBest: false, URL: "/torrents.php?id=1"},
	}}

	off := NewAuditor(Config{}).classifyReleases(entry)
	if len(off) != 1 || off[0].Tracker != "Nyaa" {
		t.Errorf("with AnimeBytes off only the Nyaa release should survive, got %+v", off)
	}

	on := NewAuditor(Config{AnimeBytes: true}).classifyReleases(entry)
	if len(on) != 2 {
		t.Errorf("with AnimeBytes on both releases should be present, got %d", len(on))
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
	a := NewAuditor(Config{SeaDexBaseURL: "https://releases.moe"})
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
			Season:    1, scope: align.ScopeSeason, Approx: true, CurrentGroups: []string{"subsplease", "erai-raws"},
			Releases:    []Release{{Group: "SubsPlease", Best: true, Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}},
			ArrURL:      "http://sonarr/series/frieren",
			SeaDexURL:   "https://releases.moe/154587",
			MatchSource: "id",
		}},
	}

	r.Log(log)

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
		"arr_url":       "http://sonarr/series/frieren",
		"seadex_url":    "https://releases.moe/154587",
		"match_source":  "id",
	}
	for k, v := range want {
		if rAttrs[k] != v {
			t.Errorf("row attr %q = %v, want %v", k, rAttrs[k], v)
		}
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
	if !strings.Contains(got, "[link](https://example.org/t)") {
		t.Errorf("a blank tracker must fall back to the %q label, got %q", "link", got)
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

	r.Log(log)

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
// be absent entirely (a boundary mutation of the notOnSeaDex > 0 guard would
// emit "; 0 more in your library that SeaDex does not list").
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
// shape (test c of mc-degradation-scoping): the Markdown header carries the
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
	if !strings.Contains(md, "**Caveat: this report is incomplete.** 2 SeaDex entries could not be mapped") {
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
	if !strings.Contains(md, "1 SeaDex entry could not be mapped") {
		t.Errorf("markdown caveat missing the singular form:\n%s", md[:300])
	}
}

// TestRenderCompleteReportOmitsIncompleteSection pins the healthy path's
// unchanged-output contract (test d of mc-degradation-scoping; the package
// keeps no golden file, so absence is pinned directly): with no incomplete
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

// TestDisplayBestGroupsAnnotatesWarned pins the SeaDex-best column's warning
// marker: a curation-warned best renders annotated with its canonical tags,
// an unwarned best of the same group wins the dedupe (a group genuinely
// available as best never displays warned), and multiple warnings join in
// canonical order.
func TestDisplayBestGroupsAnnotatesWarned(t *testing.T) {
	rels := []Release{
		{Group: "PMR", Best: true, Warnings: []string{"broken"}},
		{Group: "pmr", Best: true},
		{Group: "SEV", Best: true, Warnings: []string{"broken", "incomplete"}},
	}
	got := displayBestGroups(rels)
	want := []string{"pmr", "SEV (broken, incomplete)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("displayBestGroups() = %v, want %v", got, want)
	}
}

// TestRenderMarkdownWarnedBestAnnotatedNotLinked pins the rendered contract
// for a curation-warned best: the SeaDex-best column carries the warning
// marker ("PMR (broken)") so the row stays complete and self-explanatory,
// while the links cell does NOT offer the warned release as a grab link (the
// releases.moe link still renders).
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
	if !strings.Contains(md, "PMR (broken)") {
		t.Errorf("markdown lacks the warned-best annotation \"PMR (broken)\":\n%s", md)
	}
	if strings.Contains(md, "https://nyaa.si/view/900") {
		t.Errorf("markdown offers the warned release as a grab link:\n%s", md)
	}
	if !strings.Contains(md, "https://releases.moe/10") {
		t.Errorf("markdown lost the SeaDex entry link:\n%s", md)
	}
}

package audit

import (
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
	}}

	got := links(row)

	if strings.Count(got, "https://nyaa.si/view/1") != 1 {
		t.Errorf("repeated (tracker, URL) best link must appear once, got %q", got)
	}
	if !strings.Contains(got, "[link](https://example.org/t)") {
		t.Errorf("a blank tracker must fall back to the %q label, got %q", "link", got)
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

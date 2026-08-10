package audit

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/align"
)

// TestReportJSONWireShapeKeys pins the report JSON's KEY SET, which no
// round-trip test can see: TestWriteFilesWritesTimestampedPair unmarshals back
// into Report, so a renamed struct tag round-trips perfectly while every
// external consumer of report-<stamp>.json breaks. It also pins the
// omitempty half of the contract the code documents ("a fully obtainable row's
// JSON shape is unchanged") by asserting a minimal row carries only its
// always-present keys.
//
// "scope" is one of those always-present keys, deliberately: it is the comparison
// the row's verdict was reached under, so a consumer that cannot read it has to
// re-derive align's dispatch to know what the verdict means (l-f18). It carries
// align.ScopeKind's own String() vocabulary rather than the iota, so the wire, the
// Markdown and the log all name a scope the same way.
//
// The "full" fixture is deliberately MAXIMAL rather than realistic - it sets every
// omitempty field so the key set is complete, which is why it carries mutually
// exclusive facts (groups AND groups_unknown, best AND unobtainable). Semantics are
// pinned by the render and audit tests, not here.
func TestReportJSONWireShapeKeys(t *testing.T) {
	full := &Report{
		GeneratedAt: time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		Totals:      map[string]int{string(VerdictAlt): 1},
		Rows: []Row{{
			Title: "Frieren", Arr: "sonarr", ArrURL: "https://sonarr.example/series/frieren",
			SeaDexURL: "https://releases.moe/154587", Verdict: VerdictAlt, Qualifier: QualifierMixed,
			MatchSource: "id", CurrentGroups: []string{"erai"}, AniListID: 154587, Season: 2,
			Scope:   align.ScopeSeason,
			Special: true, Incomplete: true, Approx: true, HiddenAnimeBytes: 3,
			GroupsUnknown: true,
			Releases: []Release{{
				Tracker: "Nyaa", Group: "PMR", URL: "https://nyaa.si/view/1",
				Warnings: []string{"broken"}, Best: true, Unobtainable: true, URLError: true,
			}},
		}},
		Incomplete: []IncompleteEntry{{SeaDexURL: "https://releases.moe/7", AniListID: 7}},
	}

	data, err := renderJSON(full)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("report json does not parse: %v", err)
	}
	assertJSONKeys(t, "report", decoded, []string{"generated_at", "incomplete_mappings", "rows", "totals"})

	rows, _ := decoded["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want 1", decoded["rows"])
	}
	row, _ := rows[0].(map[string]any)
	assertJSONKeys(t, "row", row, []string{
		"al_id", "approx", "arr", "arr_url", "current_groups", "groups_unknown",
		"hidden_animebytes", "incomplete", "match_source", "qualifier", "releases",
		"scope", "seadex_url", "season", "special", "title", "verdict",
	})

	rels, _ := row["releases"].([]any)
	if len(rels) != 1 {
		t.Fatalf("releases = %v, want 1", row["releases"])
	}
	rel, _ := rels[0].(map[string]any)
	assertJSONKeys(t, "release", rel, []string{
		"best", "group", "tracker", "unobtainable", "url", "url_error", "warnings",
	})

	inc, _ := decoded["incomplete_mappings"].([]any)
	if len(inc) != 1 {
		t.Fatalf("incomplete_mappings = %v, want 1", decoded["incomplete_mappings"])
	}
	incEntry, _ := inc[0].(map[string]any)
	assertJSONKeys(t, "incomplete_mapping", incEntry, []string{"al_id", "seadex_url"})

	minimal := &Report{
		GeneratedAt: time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		Totals:      map[string]int{},
		Rows: []Row{{
			Title: "Bare", Arr: "sonarr", SeaDexURL: "https://releases.moe/1",
			Verdict: VerdictNoFile, MatchSource: "id", AniListID: 1,
		}},
	}
	data, err = renderJSON(minimal)
	if err != nil {
		t.Fatalf("renderJSON(minimal): %v", err)
	}
	decoded = nil
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("minimal report json does not parse: %v", err)
	}
	assertJSONKeys(t, "minimal report", decoded, []string{"generated_at", "rows", "totals"})
	rows, _ = decoded["rows"].([]any)
	row, _ = rows[0].(map[string]any)
	assertJSONKeys(t, "minimal row", row,
		[]string{"al_id", "arr", "match_source", "scope", "seadex_url", "title", "verdict"})
}

// assertJSONKeys compares one decoded object's sorted key set against the
// documented wire shape.
func assertJSONKeys(t *testing.T, what string, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s JSON keys = %v, want %v", what, got, want)
	}
}

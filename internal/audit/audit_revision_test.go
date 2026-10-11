package audit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/seadex"
	"github.com/cplieger/slogx/capture"
)

var (
	revOriginal = release.Revision{Version: 1, Marker: release.RevisionNone}
	revV2       = release.Revision{Version: 2, Marker: release.RevisionVersion}
)

func olderRevisionMatch(incomplete bool) match.Match {
	item := &library.Item{
		Title: "91 Days", Arr: library.ArrSonarr, HasFile: true,
		Groups:          []string{"udf"},
		SeasonGroups:    map[int][]string{1: {"udf"}},
		SeasonRevisions: map[int]map[string]release.Revision{1: {"udf": revOriginal}},
	}
	entry := seadex.Entry{AniListID: 21711, Incomplete: incomplete, Torrents: []seadex.Torrent{{
		IsBest: true, ReleaseGroup: "UDF", Tracker: "Nyaa", URL: "https://nyaa.si/view/1",
		Files: []seadex.File{{Name: "[UDF] 91 Days - 04v2 (BDRip 1080p x264 FLACx2).mkv", Length: 1 << 30}},
	}}}
	return match.Match{Item: item, Arr: library.ArrSonarr, Source: match.SourceID, Entry: entry, Record: mapping.Record{Type: "TV", SeasonTvdb: 1}}
}

func TestAuditOlderRevisionRow(t *testing.T) {
	rep := New(Config{}).Audit([]match.Match{olderRevisionMatch(false)}, nil, nil, nil)
	if len(rep.Rows) != 1 {
		t.Fatalf("Audit rows = %d, want 1", len(rep.Rows))
	}
	row := rep.Rows[0]
	if row.Verdict != VerdictOlderRevision || row.Qualifier != "" {
		t.Errorf("row verdict/qualifier = %q/%q, want %q unqualified", row.Verdict, row.Qualifier, VerdictOlderRevision)
	}
	if row.CurrentRevision != revOriginal || row.BestRevision != revV2 {
		t.Errorf("row revisions = %+v/%+v, want v1/v2", row.CurrentRevision, row.BestRevision)
	}
	if rep.Totals[string(VerdictOlderRevision)] != 1 {
		t.Errorf("Totals[%s] = %d, want 1", VerdictOlderRevision, rep.Totals[string(VerdictOlderRevision)])
	}
	if got := scopeCell(&row); got != "S1 (revision v1, SeaDex v2)" {
		t.Errorf("scopeCell = %q, want %q", got, "S1 (revision v1, SeaDex v2)")
	}
	if got := links(&row); !strings.Contains(got, "https://nyaa.si/view/1") {
		t.Errorf("links(older revision row) = %q, want the UDF release offered as a link", got)
	}
}

func TestAuditOlderRevisionOnIncompleteEntryIsQualified(t *testing.T) {
	rep := New(Config{}).Audit([]match.Match{olderRevisionMatch(true)}, nil, nil, nil)
	if len(rep.Rows) != 1 || rep.Rows[0].Verdict != VerdictOlderRevision || rep.Rows[0].Qualifier != QualifierIncomplete {
		t.Fatalf("Audit(incomplete entry) rows = %+v, want one have_older_revision qualified incomplete", rep.Rows)
	}
	if got := scopeCell(&rep.Rows[0]); got != "S1 (revision v1, SeaDex v2, incomplete)" {
		t.Errorf("scopeCell = %q, want the revision and the qualifier", got)
	}
}

func TestOlderRevisionOrderedAfterAltBeforeUnverified(t *testing.T) {
	alt := slices.Index(verdictOrder, verdictAlt)
	older := slices.Index(verdictOrder, VerdictOlderRevision)
	unverified := slices.Index(verdictOrder, VerdictUnverified)
	if alt < 0 || older != alt+1 || unverified != older+1 {
		t.Errorf("verdictOrder = %v, want have_older_revision right after have_alt and before unverified", verdictOrder)
	}
}

func TestRenderOlderRevisionSectionAndLog(t *testing.T) {
	rep := New(Config{}).Audit([]match.Match{olderRevisionMatch(false)}, nil, nil, nil)
	rep.GeneratedAt = time.Unix(0, 0).UTC()
	md := renderMarkdown(&rep)
	for _, want := range []string{
		"## have_older_revision (1)",
		verdictDesc[VerdictOlderRevision],
		"`revision v1, SeaDex v2`",
		"| S1 (revision v1, SeaDex v2) |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("renderMarkdown lacks %q:\n%s", want, md)
		}
	}
	data, err := renderJSON(&rep)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var decoded struct {
		Rows []struct {
			CurrentRevision map[string]any `json:"current_revision"`
			BestRevision    map[string]any `json:"best_revision"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Rows) != 1 {
		t.Fatalf("renderJSON = %s (%v), want one row", data, err)
	}
	if got := decoded.Rows[0].BestRevision; got["version"] != float64(2) || got["marker"] != "version" {
		t.Errorf("best_revision JSON = %v, want version 2 marker version", got)
	}

	log, rec := capture.New()
	if err := rep.Log(t.Context(), log); err != nil {
		t.Fatalf("Log: %v", err)
	}
	recs := rec.Records()
	if len(recs) != 2 {
		t.Fatalf("Log records = %d, want summary + one row", len(recs))
	}
	if got := recordAttrs(recs[0])["have_older_revision"]; got != int64(1) {
		t.Errorf("summary have_older_revision = %v, want 1", got)
	}
	rowAttrs := recordAttrs(recs[1])
	if rowAttrs["current_revision"] != "v1" || rowAttrs["best_revision"] != "v2" || rowAttrs["verdict"] != string(VerdictOlderRevision) {
		t.Errorf("row attrs current_revision/best_revision/verdict = %v/%v/%v, want v1/v2/%s",
			rowAttrs["current_revision"], rowAttrs["best_revision"], rowAttrs["verdict"], VerdictOlderRevision)
	}
}

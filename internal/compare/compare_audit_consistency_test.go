package compare

import (
	"testing"

	"github.com/cplieger/seadex-scout/internal/audit"
	"github.com/cplieger/seadex-scout/internal/filter"
	"github.com/cplieger/seadex-scout/internal/library"
	"github.com/cplieger/seadex-scout/internal/mapping"
	"github.com/cplieger/seadex-scout/internal/match"
	"github.com/cplieger/seadex-scout/internal/seadex"
)

// TestCompareAuditConsistency runs the SAME (item, entry) through both align
// consumers - the daemon's compare pass and the audit report - and pins that
// they tell one story across the five states the shared vocabulary covers:
// no-file, aligned, mixed-group (>1 group, not aligned), theoretical-only, and
// incomplete. The daemon is report-by-exception (silence is its no-file and
// aligned outcome); the audit enumerates, carrying the daemon's vocabulary as
// the row Qualifier.
func TestCompareAuditConsistency(t *testing.T) {
	nyaaBest := seadex.Entry{AniListID: 1, Torrents: []seadex.Torrent{
		{IsBest: true, ReleaseGroup: "SubsPlease", Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
	}}
	seasonRec := mapping.Record{Type: "TV", SeasonTvdb: 1}
	wholeRec := mapping.Record{Type: "TV"}

	tests := []struct {
		name          string
		seasons       map[int][]string
		record        mapping.Record
		entry         seadex.Entry
		wantStatus    Status // "" = the daemon is silent
		wantVerdict   audit.Verdict
		wantQualifier audit.Qualifier
	}{
		{
			name:        "season no-file: daemon silent, audit no_file",
			seasons:     map[int][]string{2: {"erai-raws"}},
			record:      seasonRec,
			entry:       nyaaBest,
			wantVerdict: audit.VerdictNoFile,
		},
		{
			name:        "season aligned: daemon silent, audit have_best",
			seasons:     map[int][]string{1: {"subsplease"}},
			record:      seasonRec,
			entry:       nyaaBest,
			wantVerdict: audit.VerdictBest,
		},
		{
			name:        "season aligned multi-group: daemon silent (alignment wins), audit have_best unqualified",
			seasons:     map[int][]string{1: {"subsplease", "erai-raws"}},
			record:      seasonRec,
			entry:       nyaaBest,
			wantVerdict: audit.VerdictBest,
		},
		{
			name:          "season mixed-group not aligned: daemon mixed_group_manual, audit qualifier mixed",
			seasons:       map[int][]string{1: {"a", "b"}},
			record:        seasonRec,
			entry:         nyaaBest,
			wantStatus:    StatusMixedGroup,
			wantVerdict:   audit.VerdictUnlisted,
			wantQualifier: audit.QualifierMixed,
		},
		{
			name:          "season theoretical-only: daemon theoretical_best, audit qualifier theoretical",
			seasons:       map[int][]string{1: {"a"}},
			record:        seasonRec,
			entry:         seadex.Entry{AniListID: 1, TheoreticalBest: "a stated remux"},
			wantStatus:    StatusTheoretical,
			wantVerdict:   audit.VerdictUnlisted,
			wantQualifier: audit.QualifierTheoretical,
		},
		{
			name:          "season incomplete nothing recommended: daemon incomplete, audit qualifier incomplete",
			seasons:       map[int][]string{1: {"a"}},
			record:        seasonRec,
			entry:         seadex.Entry{AniListID: 1, Incomplete: true},
			wantStatus:    StatusIncomplete,
			wantVerdict:   audit.VerdictUnlisted,
			wantQualifier: audit.QualifierIncomplete,
		},
		{
			name:    "season incomplete with listed best not aligned: daemon and audit both mark incomplete",
			seasons: map[int][]string{1: {"erai-raws"}},
			record:  seasonRec,
			entry: func() seadex.Entry {
				e := nyaaBest
				e.Incomplete = true
				return e
			}(),
			wantStatus:    StatusIncomplete,
			wantVerdict:   audit.VerdictUnlisted,
			wantQualifier: audit.QualifierIncomplete,
		},
		{
			name:        "whole-series no real season: daemon silent, audit no_file",
			seasons:     map[int][]string{0: {"subsplease"}},
			record:      wholeRec,
			entry:       nyaaBest,
			wantVerdict: audit.VerdictNoFile,
		},
		{
			name:        "whole-series aligned multi-group: daemon silent, audit have_best unqualified",
			seasons:     map[int][]string{1: {"subsplease"}, 2: {"subsplease", "erai-raws"}},
			record:      wholeRec,
			entry:       nyaaBest,
			wantVerdict: audit.VerdictBest,
		},
		{
			name:          "whole-series mixed-group not aligned: daemon mixed_group_manual, audit qualifier mixed",
			seasons:       map[int][]string{1: {"subsplease"}, 2: {"erai-raws"}},
			record:        wholeRec,
			entry:         nyaaBest,
			wantStatus:    StatusMixedGroup,
			wantVerdict:   audit.VerdictUnlisted,
			wantQualifier: audit.QualifierMixed,
		},
		{
			name:          "whole-series single-group not aligned: daemon better_release, audit unqualified have_unlisted",
			seasons:       map[int][]string{1: {"erai-raws"}, 2: {"erai-raws"}},
			record:        wholeRec,
			entry:         nyaaBest,
			wantStatus:    StatusBetter,
			wantVerdict:   audit.VerdictUnlisted,
			wantQualifier: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &library.Item{Title: "Same Story", Arr: library.ArrSonarr, SeasonGroups: tt.seasons, HasFile: true}
			m := match.Match{Item: item, Arr: library.ArrSonarr, Source: match.SourceID, Entry: tt.entry, Record: tt.record}

			findings := comparer(filter.Options{}, false).Compare([]match.Match{m})
			if tt.wantStatus == "" {
				if len(findings) != 0 {
					t.Errorf("daemon must be silent, got %+v", findings)
				}
			} else {
				if len(findings) != 1 {
					t.Fatalf("daemon findings = %d, want 1: %+v", len(findings), findings)
				}
				if findings[0].Status != tt.wantStatus {
					t.Errorf("daemon status = %q, want %q", findings[0].Status, tt.wantStatus)
				}
			}

			rep := audit.NewAuditor(audit.Config{SeaDexBaseURL: "https://releases.moe"}).Audit([]match.Match{m}, nil, nil)
			if len(rep.Rows) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(rep.Rows))
			}
			row := rep.Rows[0]
			if row.Verdict != tt.wantVerdict {
				t.Errorf("audit verdict = %q, want %q", row.Verdict, tt.wantVerdict)
			}
			if row.Qualifier != tt.wantQualifier {
				t.Errorf("audit qualifier = %q, want %q", row.Qualifier, tt.wantQualifier)
			}
		})
	}
}

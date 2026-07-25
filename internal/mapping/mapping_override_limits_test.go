package mapping

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseOverrides_rejectsTooManyDistinctRecords(t *testing.T) {
	var input strings.Builder
	input.WriteByte('[')
	for id := 1; id <= maxOverrideRecords+1; id++ {
		if id > 1 {
			input.WriteByte(',')
		}
		input.WriteString(`{"anilist_id":`)
		input.WriteString(strconv.Itoa(id))
		input.WriteByte('}')
	}
	input.WriteByte(']')

	set, err := parseOverrides([]byte(input.String()))
	if err == nil {
		t.Fatal("parseOverrides with 65,537 distinct records = nil error, want record-cap rejection")
	}
	if !strings.Contains(err.Error(), "overrides exceed cap 65536 records") {
		t.Errorf("parseOverrides error = %q, want record-cap rejection", err)
	}
	if set.records != nil || set.unknown != nil || set.duplicates != nil || set.applied != 0 || set.skipped != 0 {
		t.Errorf("parseOverrides record-cap error carried a partial result: %+v", set)
	}
}

// TestParseOverrides_oversizedRecordSkippedWithoutMaterialization pins the
// pre-check allocation bound (CWE-770 regression): a single valid multi-MB
// record whose tmdb_movies array carries hundreds of thousands of compact ids
// is counted oversized with at most maxOverrideIDsPerRecord elements ever
// decoded (the rest are token-skipped by decodeCappedArray, never allocated),
// a sibling record whose unknown key carries an equally huge array value is
// applied with only the key name retained (the value is skipped, never
// materialized into a map[string]json.RawMessage), and a plain sibling still
// applies - the file parses cleanly instead of creating memory pressure that
// scales with element count.
func TestParseOverrides_oversizedRecordSkippedWithoutMaterialization(t *testing.T) {
	const ids = 300_000
	var b strings.Builder
	b.WriteString(`[{"anilist_id":5,"type":"movie","tmdb_movies":[`)
	for i := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteString(`]},{"anilist_id":6,"type":"movie","junk":[`)
	for i := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteString(`]},{"anilist_id":2,"type":"tv","tvdb_id":100}]`)

	set, err := parseOverrides([]byte(b.String()))
	if err != nil {
		t.Fatalf("parseOverrides error: %v", err)
	}
	if set.oversized != 1 {
		t.Errorf("oversized = %d, want 1 (the over-cap tmdb_movies record)", set.oversized)
	}
	if set.applied != 2 {
		t.Errorf("applied = %d, want 2 (the unknown-key record and the plain sibling)", set.applied)
	}
	if len(set.records) != 2 || set.records[0].AniListID != 6 || set.records[1].AniListID != 2 {
		t.Errorf("records = %+v, want ids [6 2] (the oversized record skipped, never truncated)", set.records)
	}
	if len(set.records[0].TmdbMovies) != 0 {
		t.Errorf("unknown-key record TmdbMovies = %v, want empty (the huge value belonged to an unknown key and must be skipped)", set.records[0].TmdbMovies)
	}
	if len(set.unknown) != 1 || set.unknown[0] != "junk" {
		t.Errorf("unknown keys = %v, want [junk] (name retained, value skipped)", set.unknown)
	}
	if set.skipped != 0 {
		t.Errorf("skipped = %d, want 0", set.skipped)
	}
}

// overCapIDs renders a JSON array of maxOverrideIDsPerRecord+1 compact ids,
// one element past decodeCappedArray's cap.
func overCapIDs() string {
	ids := make([]string, maxOverrideIDsPerRecord+1)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	return "[" + strings.Join(ids, ",") + "]"
}

// TestParseOverrides_duplicateIDArrayLastOccurrenceWins pins the last-wins
// rule for a duplicate id-array key's OVER-CAP state, not just its value:
// JSON duplicate keys are decoded in document order and this decoder promises
// encoding/json parity, so a later valid occurrence replaces an earlier
// over-cap one (its effective value is the small array, so the record must
// apply), a later over-cap occurrence replaces an earlier valid one (the
// record is skipped), and the two arrays are tracked independently so a valid
// imdb_ids duplicate can never clear an over-cap final tmdb_movies.
func TestParseOverrides_duplicateIDArrayLastOccurrenceWins(t *testing.T) {
	tests := []struct {
		name          string
		record        string
		wantOversized int
		wantApplied   int
		wantMovies    []int
	}{
		{
			name:          "over-cap occurrence superseded by a valid one",
			record:        `{"anilist_id":5,"type":"movie","tmdb_movies":` + overCapIDs() + `,"tmdb_movies":[42]}`,
			wantOversized: 0,
			wantApplied:   1,
			wantMovies:    []int{42},
		},
		{
			name:          "valid occurrence superseded by an over-cap one",
			record:        `{"anilist_id":5,"type":"movie","tmdb_movies":[42],"tmdb_movies":` + overCapIDs() + `}`,
			wantOversized: 1,
			wantApplied:   0,
		},
		{
			name:          "a valid imdb_ids duplicate does not clear an over-cap tmdb_movies",
			record:        `{"anilist_id":5,"type":"movie","tmdb_movies":` + overCapIDs() + `,"imdb_ids":["tt1"],"imdb_ids":["tt2"]}`,
			wantOversized: 1,
			wantApplied:   0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, err := parseOverrides([]byte("[" + tc.record + "]"))
			if err != nil {
				t.Fatalf("parseOverrides error: %v", err)
			}
			if set.oversized != tc.wantOversized {
				t.Errorf("oversized = %d, want %d", set.oversized, tc.wantOversized)
			}
			if set.applied != tc.wantApplied {
				t.Errorf("applied = %d, want %d", set.applied, tc.wantApplied)
			}
			if tc.wantMovies == nil {
				if len(set.records) != 0 {
					t.Errorf("records = %+v, want none (the record is oversized)", set.records)
				}
				return
			}
			if len(set.records) != 1 {
				t.Fatalf("records = %+v, want the one applied record", set.records)
			}
			if got := set.records[0].TmdbMovies; len(got) != len(tc.wantMovies) || got[0] != tc.wantMovies[0] {
				t.Errorf("TmdbMovies = %v, want %v (the last occurrence's effective value)", got, tc.wantMovies)
			}
		})
	}
}

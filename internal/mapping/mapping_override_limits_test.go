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

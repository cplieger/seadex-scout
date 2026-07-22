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

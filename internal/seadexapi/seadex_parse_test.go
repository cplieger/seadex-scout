package seadexapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestChunkComplete pins the keyset walk's completeness decision table,
// including the arms the HTTP-level tests never reach in-package: a FULL chunk
// always continues (the filter asked for everything after the cursor, so a
// full chunk cannot be the last), a SHORT chunk completes even with a count
// mismatch (pagination over a live collection can shift counts, which stays
// finishFetch's WARN), an EMPTY first chunk completes so finishFetch's
// empty-catalogue guard converts it into an error, and an EMPTY later chunk
// completes only while the reported totalItems is already satisfied - one with
// entries still outstanding is a truncated-view error.
func TestChunkComplete(t *testing.T) {
	tests := []struct {
		name          string
		page          int
		itemCount     int
		fetched       int
		reportedTotal int
		wantDone      bool
		wantErr       bool
	}{
		{name: "full chunk continues", page: 1, itemCount: perPage, fetched: perPage, reportedTotal: 1500, wantDone: false},
		{name: "full chunk continues even with the total satisfied", page: 2, itemCount: perPage, fetched: 1000, reportedTotal: 1000, wantDone: false},
		{name: "short chunk completes", page: 3, itemCount: 12, fetched: 1012, reportedTotal: 1012, wantDone: true},
		{name: "short chunk with a count mismatch completes", page: 3, itemCount: 12, fetched: 1012, reportedTotal: 1013, wantDone: true},
		{name: "single short chunk completes", page: 1, itemCount: 7, fetched: 7, reportedTotal: 7, wantDone: true},
		{name: "empty first chunk completes", page: 1, itemCount: 0, wantDone: true},
		{name: "empty first chunk with outstanding items completes", page: 1, itemCount: 0, reportedTotal: 3, wantDone: true},
		{name: "empty later chunk with the total satisfied completes", page: 2, itemCount: 0, fetched: 500, reportedTotal: 500, wantDone: true},
		{name: "empty later chunk with outstanding items errors", page: 2, itemCount: 0, fetched: 500, reportedTotal: 501, wantErr: true},
		{name: "empty later chunk with no reported total errors", page: 2, itemCount: 0, fetched: perPage, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, err := chunkComplete(tc.page, tc.itemCount, tc.fetched, tc.reportedTotal)
			if (err != nil) != tc.wantErr {
				t.Fatalf("chunkComplete(%d, %d, %d, %d) error = %v, wantErr %v",
					tc.page, tc.itemCount, tc.fetched, tc.reportedTotal, err, tc.wantErr)
			}
			if err == nil && done != tc.wantDone {
				t.Errorf("chunkComplete(%d, %d, %d, %d) done = %v, want %v",
					tc.page, tc.itemCount, tc.fetched, tc.reportedTotal, done, tc.wantDone)
			}
		})
	}
}

// TestAdvanceCursor pins the keyset cursor's fail-closed advance rules over a
// whole chunk: every record's (created, id) pair must be usable and sort
// strictly after its predecessor, and the LAST record becomes the next
// position, while a missing pair, one carrying filter-unsafe bytes, one longer
// than maxCursorValueBytes, a pair
// identical to the current position (an upstream ignoring the filter), a pair
// that REGRESSES behind it, and within-page disorder all
// error rather than looping or skipping records.
func TestAdvanceCursor(t *testing.T) {
	prev := cursor{created: "2026-01-02 03:04:05.000Z", id: "aaa"}
	tests := []struct {
		name    string
		items   []pbEntry
		want    cursor
		wantErr bool
	}{
		{
			name:  "last record advances the cursor",
			items: []pbEntry{{ID: "aab", Created: prev.created}, {ID: "bbb", Created: "2026-01-03 00:00:00.000Z"}},
			want:  cursor{created: "2026-01-03 00:00:00.000Z", id: "bbb"},
		},
		{
			name:  "surrounding whitespace trimmed",
			items: []pbEntry{{ID: "  bbb  ", Created: "  2026-01-03 00:00:00.000Z  "}},
			want:  cursor{created: "2026-01-03 00:00:00.000Z", id: "bbb"},
		},
		{name: "missing id errors", items: []pbEntry{{Created: prev.created}}, wantErr: true},
		{name: "missing created errors", items: []pbEntry{{ID: "bbb"}}, wantErr: true},
		{name: "quote in the cursor errors", items: []pbEntry{{ID: `b"b`, Created: prev.created}}, wantErr: true},
		{name: "backslash in the cursor errors", items: []pbEntry{{ID: `b\b`, Created: prev.created}}, wantErr: true},
		{name: "control byte in the cursor errors", items: []pbEntry{{ID: "b\nb", Created: prev.created}}, wantErr: true},
		{name: "unchanged position errors", items: []pbEntry{{ID: prev.id, Created: prev.created}}, wantErr: true},
		{name: "lower created errors", items: []pbEntry{{ID: "zzz", Created: "2026-01-01 00:00:00.000Z"}}, wantErr: true},
		{name: "same created with a lower id errors", items: []pbEntry{{ID: "aa", Created: prev.created}}, wantErr: true},
		{
			name:    "within-page disorder errors",
			items:   []pbEntry{{ID: "ccc", Created: prev.created}, {ID: "bbb", Created: prev.created}},
			wantErr: true,
		},
		{
			name:    "a repeated record within the page errors",
			items:   []pbEntry{{ID: "ccc", Created: prev.created}, {ID: "ccc", Created: prev.created}},
			wantErr: true,
		},
		{
			name:    "an unusable pair on a non-final record errors",
			items:   []pbEntry{{ID: "", Created: prev.created}, {ID: "bbb", Created: "2026-01-03 00:00:00.000Z"}},
			wantErr: true,
		},
		{name: "oversized cursor value errors", items: []pbEntry{{ID: strings.Repeat("x", maxCursorValueBytes+1), Created: prev.created}}, wantErr: true},
		{
			name:  "cursor value exactly at the length cap advances",
			items: []pbEntry{{ID: strings.Repeat("x", maxCursorValueBytes), Created: "2026-01-03 00:00:00.000Z"}},
			want:  cursor{created: "2026-01-03 00:00:00.000Z", id: strings.Repeat("x", maxCursorValueBytes)},
		},
		{name: "DEL byte in the cursor errors", items: []pbEntry{{ID: "b\x7fb", Created: prev.created}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := advanceCursor(tc.items, prev)
			if (err != nil) != tc.wantErr {
				t.Fatalf("advanceCursor error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if got != prev {
					t.Errorf("advanceCursor = %+v, want the previous position kept on error", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("advanceCursor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAdvanceCursorBoundsRejectedValueInDiagnostic pins the diagnostic budget on
// the rejection arms: the cursor values they quote are untrusted upstream text
// bounded only by maxPageBytes, and internal/scout logs the resulting error
// verbatim as a slog attribute, so an oversized value must reach the log capped
// and marked rather than balloon one Loki record (the app-wide runesafe policy).
func TestAdvanceCursorBoundsRejectedValueInDiagnostic(t *testing.T) {
	huge := strings.Repeat("x", 64<<10)
	tests := []struct {
		name  string
		items []pbEntry
	}{
		{
			name:  "rejected oversized id",
			items: []pbEntry{{ID: huge, Created: "2026-01-03 00:00:00.000Z"}},
		},
		{
			name:  "rejected oversized created",
			items: []pbEntry{{ID: "bbb", Created: huge}},
		},
		{
			// No id at all takes the no-usable-keyset-cursor arm, which quotes
			// the RAW upstream fields before filterSafe has bounded anything.
			name:  "rejected oversized created with no id",
			items: []pbEntry{{ID: "", Created: huge}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := advanceCursor(tc.items, cursor{created: "2026-01-02 03:04:05.000Z", id: "aaa"})
			if err == nil {
				t.Fatal("advanceCursor must reject an oversized cursor value")
			}
			msg := err.Error()
			if strings.Contains(msg, huge) {
				t.Error("the diagnostic must not carry the whole upstream value")
			}
			if !strings.Contains(msg, "...") {
				t.Errorf("the diagnostic must mark the value as truncated, got %q", msg)
			}
			// The message is the fixed prose plus two bounded values; a few
			// hundred bytes of headroom keeps this pinned to the budget rather
			// than to the exact wording.
			if len(msg) > 4*maxLoggedCursorBytes {
				t.Errorf("the diagnostic is %d bytes, want it bounded by the logged-value cap", len(msg))
			}
		})
	}
}

// TestCursorFilter pins the rendered PocketBase filter: the composite keyset
// predicate (a later created, or the same created with a greater id) with both
// values quoted, so equal-timestamp records advance by id instead of stalling.
func TestCursorFilter(t *testing.T) {
	got := cursor{created: "2026-01-02 03:04:05.000Z", id: "abc"}.filter()
	want := `(created>"2026-01-02 03:04:05.000Z"||(created="2026-01-02 03:04:05.000Z"&&id>"abc"))`
	if got != want {
		t.Errorf("cursor.filter() = %q, want %q", got, want)
	}
	if (cursor{}).set() {
		t.Error("the zero cursor must report no position (the first chunk is unfiltered)")
	}
	if !(cursor{id: "abc"}).set() {
		t.Error("a cursor carrying an id must report a position")
	}
}

// TestDecodePageCaseInsensitiveKeysMatchUnmarshal is a json.Unmarshal oracle
// for the token-level decoder's field matching: encoding/json accepts a
// case-insensitive field-name match when no exact match exists, so an
// upper-cased envelope from a drifted upstream must decode identically through
// decodePage instead of silently zeroing every field (an empty catalogue).
func TestDecodePageCaseInsensitiveKeysMatchUnmarshal(t *testing.T) {
	body := []byte(`{"TOTALITEMS":1,"TOTALPAGES":1,"ITEMS":[{"ALID":7,"NOTES":"n",` +
		`"THEORETICALBEST":"tb","INCOMPLETE":true,"EXPAND":{"TRS":[{"RELEASEGROUP":"PMR",` +
		`"TRACKER":"Nyaa","INFOHASH":"abc","URL":"https://nyaa.si/view/1","ISBEST":true,` +
		`"DUALAUDIO":true,"FILES":[{"name":"a.mkv","length":1}],"TAGS":["t"]}]}}]}`)

	got, _, err := decodePage(body, maxPageElements)
	if err != nil {
		t.Fatalf("decodePage: %v", err)
	}
	var want pbList
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatalf("json.Unmarshal oracle: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodePage = %+v, want json.Unmarshal parity %+v", got, want)
	}
	if len(got.Items) != 1 || got.Items[0].AlID != 7 || len(got.Items[0].Expand.Trs) != 1 {
		t.Errorf("upper-case envelope lost data: %+v", got)
	}
}

// TestDecodePageDuplicateExpandNullMatchesUnmarshal is a json.Unmarshal oracle
// for duplicate-key null handling: json.Unmarshal treats null into the
// non-pointer pbExpand struct as a no-op, so a torrent-bearing "expand"
// followed by a duplicate "expand":null must preserve the decoded torrents
// instead of silently zeroing them.
func TestDecodePageDuplicateExpandNullMatchesUnmarshal(t *testing.T) {
	body := []byte(`{"totalItems":1,"totalPages":1,"items":[{"alID":7,` +
		`"expand":{"trs":[{"releaseGroup":"PMR","tracker":"Nyaa","isBest":true,` +
		`"url":"https://nyaa.si/view/1"}]},"expand":null}]}`)

	got, _, err := decodePage(body, maxPageElements)
	if err != nil {
		t.Fatalf("decodePage: %v", err)
	}
	var want pbList
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatalf("json.Unmarshal oracle: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodePage = %+v, want json.Unmarshal parity %+v", got, want)
	}
	if len(got.Items) != 1 || len(got.Items[0].Expand.Trs) != 1 {
		t.Fatalf("duplicate expand:null wiped the decoded torrents: %+v", got)
	}
	if got.Items[0].Expand.Trs[0].ReleaseGroup != "PMR" {
		t.Errorf("torrent group = %q, want PMR preserved", got.Items[0].Expand.Trs[0].ReleaseGroup)
	}
}

// TestDecodePageDuplicateExpandObjectMatchesUnmarshal is the object arm of the
// duplicate-key oracle: json.Unmarshal decodes a duplicate "expand" object
// INTO the same struct value, overwriting only the fields it carries, so a
// torrent-bearing "expand" followed by a duplicate empty "expand":{} must
// preserve the decoded torrents instead of replacing the whole struct.
func TestDecodePageDuplicateExpandObjectMatchesUnmarshal(t *testing.T) {
	body := []byte(`{"totalItems":1,"totalPages":1,"items":[{"alID":7,` +
		`"expand":{"trs":[{"releaseGroup":"PMR","tracker":"Nyaa","isBest":true,` +
		`"url":"https://nyaa.si/view/1"}]},"expand":{}}]}`)

	got, _, err := decodePage(body, maxPageElements)
	if err != nil {
		t.Fatalf("decodePage: %v", err)
	}
	var want pbList
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatalf("json.Unmarshal oracle: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodePage = %+v, want json.Unmarshal parity %+v", got, want)
	}
	if len(got.Items) != 1 || len(got.Items[0].Expand.Trs) != 1 {
		t.Fatalf("duplicate expand:{} wiped the decoded torrents: %+v", got)
	}
	if got.Items[0].Expand.Trs[0].ReleaseGroup != "PMR" {
		t.Errorf("torrent group = %q, want PMR preserved", got.Items[0].Expand.Trs[0].ReleaseGroup)
	}
}

// TestDecodePageDuplicateItemsMergeMatchesUnmarshal is the ARRAY arm of the
// duplicate-key oracle: json.Unmarshal decodes a duplicate array-valued key
// INTO the already-populated slice element-wise (struct elements merge
// field-wise, and a shorter second occurrence truncates to the new length),
// so a duplicate "items" whose second occurrence carries a partial element
// must merge into the first instead of replacing it with a fresh slice.
func TestDecodePageDuplicateItemsMergeMatchesUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "partial second element merges",
			body: `{"items":[{"alID":1,"notes":"x"}],"items":[{"alID":2}]}`,
		},
		{
			name: "shorter second occurrence truncates while merging",
			body: `{"items":[{"alID":1,"notes":"x"},{"alID":3,"notes":"y"}],"items":[{"alID":2}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := decodePage([]byte(tc.body), maxPageElements)
			if err != nil {
				t.Fatalf("decodePage: %v", err)
			}
			var want pbList
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatalf("json.Unmarshal oracle: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("decodePage = %+v, want json.Unmarshal parity %+v", got, want)
			}
			if len(got.Items) != 1 || got.Items[0].AlID != 2 || got.Items[0].Notes != "x" {
				t.Errorf("duplicate items did not merge element-wise: %+v", got.Items)
			}
		})
	}
}

// TestDecodePageDuplicateItemsRegrowMatchesUnmarshal is the regrow arm of
// the duplicate-key oracle: a duplicate array key that shrinks and then
// regrows within retained capacity re-exposes the stale backing element
// (stdlib SetLen), while a regrow after an empty occurrence starts from a
// fresh zeroed slice (stdlib replaces the backing on an empty array).
func TestDecodePageDuplicateItemsRegrowMatchesUnmarshal(t *testing.T) {
	bodies := []string{
		`{"items":[{"alID":1,"notes":"x"},{"alID":3,"notes":"y"}],"items":[{"alID":2}],"items":[{"alID":9},{}]}`,
		`{"items":[{"alID":1,"notes":"x"},{"alID":3,"notes":"y"}],"items":[],"items":[{},{}]}`,
		`{"items":[{"expand":{"trs":[{"tags":["a","b"],"tags":[],"tags":[null]}]}}]}`,
	}
	for _, body := range bodies {
		got, _, err := decodePage([]byte(body), maxPageElements)
		if err != nil {
			t.Fatalf("decodePage(%s): %v", body, err)
		}
		var want pbList
		if err := json.Unmarshal([]byte(body), &want); err != nil {
			t.Fatalf("json.Unmarshal oracle: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("decodePage(%s) = %+v, want json.Unmarshal parity %+v", body, got, want)
		}
	}
}

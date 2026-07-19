package seadex

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestParsePBTime pins the tolerant PocketBase timestamp parsing: both
// space-separated layouts (with and without fractional seconds) and RFC3339
// parse, while empty, whitespace, and garbage values fall to the zero time
// (which sorts oldest, so an unparseable record lands at the feed's tail
// instead of erroring the fetch).
func TestParsePBTime(t *testing.T) {
	tests := []struct {
		want time.Time
		name string
		in   string
	}{
		{name: "fractional space layout", in: "2026-01-02 03:04:05.000Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "whole-second space layout", in: "2026-01-02 03:04:05Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "rfc3339", in: "2026-01-02T03:04:05Z", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "surrounding whitespace trimmed", in: "  2026-01-02 03:04:05Z  ", want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "empty is zero", in: "", want: time.Time{}},
		{name: "whitespace only is zero", in: "   ", want: time.Time{}},
		{name: "garbage is zero", in: "not a timestamp", want: time.Time{}},
		{name: "unsupported layout is zero", in: "02/01/2026 03:04", want: time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePBTime(tc.in); !got.Equal(tc.want) {
				t.Errorf("parsePBTime(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestPageComplete pins the pagination-completeness decision table, including
// the arm the HTTP-level tests never reach in-package: an empty FINAL page (or
// an empty FIRST page when the API reports zero total pages) completes cleanly
// only while the reported totalItems is already satisfied — an empty page with
// entries still outstanding is a truncated-view error, as is an empty page
// before the reported total, and ANY page with invalid metadata (totalPages
// < 1 — the empty first page being the one exception — or a page, empty or
// not, past the reported total) errors rather than being accepted as a
// complete catalogue.
func TestPageComplete(t *testing.T) {
	tests := []struct {
		name          string
		page          int
		itemCount     int
		totalPages    int
		fetched       int
		reportedTotal int
		wantDone      bool
		wantErr       bool
	}{
		{name: "mid page continues", page: 1, itemCount: 500, totalPages: 3, fetched: 500, reportedTotal: 1500, wantDone: false},
		{name: "final page with items completes", page: 3, itemCount: 12, totalPages: 3, fetched: 1012, reportedTotal: 1012, wantDone: true},
		{name: "final page with items and count mismatch completes", page: 3, itemCount: 12, totalPages: 3, fetched: 1012, reportedTotal: 1013, wantDone: true},
		{name: "single page completes", page: 1, itemCount: 7, totalPages: 1, fetched: 7, reportedTotal: 7, wantDone: true},
		{name: "empty final page with satisfied total completes", page: 2, itemCount: 0, totalPages: 2, fetched: 500, reportedTotal: 500, wantDone: true},
		{name: "empty final page with outstanding items errors", page: 2, itemCount: 0, totalPages: 2, fetched: 500, reportedTotal: 501, wantErr: true},
		{name: "empty single page with zero totals completes", page: 1, itemCount: 0, totalPages: 1, wantDone: true},
		{name: "empty first page with outstanding items errors", page: 1, itemCount: 0, totalPages: 1, reportedTotal: 3, wantErr: true},
		{name: "empty page with zero total completes", page: 1, itemCount: 0, totalPages: 0, wantDone: true},
		{name: "later empty page with zero total errors", page: 2, itemCount: 0, totalPages: 0, fetched: 500, reportedTotal: 500, wantErr: true},
		{name: "later empty page with negative total errors", page: 2, itemCount: 0, totalPages: -1, fetched: 500, reportedTotal: 500, wantErr: true},
		{name: "empty page before total errors", page: 2, itemCount: 0, totalPages: 3, fetched: 500, reportedTotal: 1500, wantErr: true},
		{name: "empty page past reported total errors", page: 3, itemCount: 0, totalPages: 2, fetched: 1000, reportedTotal: 1000, wantErr: true},
		{name: "non-empty page with zero total errors", page: 1, itemCount: 500, totalPages: 0, fetched: 500, reportedTotal: 500, wantErr: true},
		{name: "non-empty page with negative total errors", page: 1, itemCount: 500, totalPages: -1, fetched: 500, reportedTotal: 500, wantErr: true},
		{name: "non-empty page past reported total errors", page: 4, itemCount: 5, totalPages: 3, fetched: 1505, reportedTotal: 1505, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, err := pageComplete(tc.page, tc.itemCount, tc.totalPages, tc.fetched, tc.reportedTotal)
			if (err != nil) != tc.wantErr {
				t.Fatalf("pageComplete(%d, %d, %d, %d, %d) error = %v, wantErr %v",
					tc.page, tc.itemCount, tc.totalPages, tc.fetched, tc.reportedTotal, err, tc.wantErr)
			}
			if err == nil && done != tc.wantDone {
				t.Errorf("pageComplete(%d, %d, %d, %d, %d) done = %v, want %v",
					tc.page, tc.itemCount, tc.totalPages, tc.fetched, tc.reportedTotal, done, tc.wantDone)
			}
		})
	}
}

// TestEntryHasTheoreticalBest pins the theoretical-best predicate both
// consumers branch on (compare's theoretical_best info finding and audit's
// theoretical qualifier): a named theoretical best reports true, empty and
// whitespace-only false (untrusted PocketBase text names nothing).
func TestEntryHasTheoreticalBest(t *testing.T) {
	if (&Entry{}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = true for empty TheoreticalBest, want false")
	}
	if (&Entry{TheoreticalBest: " \t "}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = true for whitespace-only TheoreticalBest, want false")
	}
	if !(&Entry{TheoreticalBest: "a stated remux"}).HasTheoreticalBest() {
		t.Error("HasTheoreticalBest() = false with TheoreticalBest set, want true")
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

// TestInfoHashRedacted pins the redaction predicate's tolerant matching on the
// untrusted upstream value: the exact literal, case variants, and surrounding
// whitespace all read as redacted, while a real hash, an empty string, and a
// near-miss do not.
func TestInfoHashRedacted(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "exact literal", in: "<redacted>", want: true},
		{name: "upper-cased", in: "<REDACTED>", want: true},
		{name: "mixed case", in: "<Redacted>", want: true},
		{name: "surrounding whitespace", in: "  <redacted>  ", want: true},
		{name: "real hash", in: "143ed15e5e3df072ae91adaeb149973a887590dd", want: false},
		{name: "empty", in: "", want: false},
		{name: "near-miss without brackets", in: "redacted", want: false},
		{name: "embedded in longer value", in: "x<redacted>", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := InfoHashRedacted(tc.in); got != tc.want {
				t.Errorf("InfoHashRedacted(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

package seadex

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cplieger/jsonx/bounded"
)

// FuzzDecodePage is a differential fuzz target for the schema-aware bounded
// page decoder: for any body, decodePage must match the json.Unmarshal
// oracle it documents parity with. If json.Unmarshal accepts a body,
// decodePage must either accept it with a deeply-equal pbList or reject it
// for exactly one reason - a jsonx/bounded cardinality cap or element
// budget, the one deliberate divergence from stdlib. If json.Unmarshal
// rejects a body, decodePage must reject it too (never accept what stdlib
// refuses). This guards the whole parity surface at once: case-insensitive
// key matching, null-into-container no-ops, duplicate-key overwrite order,
// trailing-data strictness, scalar type errors, and nil-vs-empty slice
// identity (jsonx/bounded matches stdlib exactly: null → nil, `[]` → empty
// non-nil, absent → untouched), so the comparison is a plain DeepEqual.
func FuzzDecodePage(f *testing.F) {
	seeds := []string{
		`{"totalItems":1,"totalPages":1,"items":[{"alID":7,"notes":"n","theoreticalBest":"tb","updated":"2026-01-02 03:04:05.000Z","incomplete":true,"expand":{"trs":[{"releaseGroup":"PMR","tracker":"Nyaa","infoHash":"abc","url":"https://nyaa.si/view/1","isBest":true,"dualAudio":true,"files":[{"name":"a.mkv","length":1}],"tags":["best"]}]}}]}`,
		`{"TOTALITEMS":1,"TOTALPAGES":1,"ITEMS":[{"ALID":7,"EXPAND":{"TRS":[]}}]}`,
		`{"totalItems":1,"totalPages":1,"items":[{"alID":7,"expand":{"trs":[{"releaseGroup":"PMR"}]},"expand":null}]}`,
		`{"items":null,"totalPages":null,"totalItems":null}`,
		`null`,
		`{}`,
		`{"unknown":{"deep":[{"nested":true}]},"items":[]}`,
		`{"unknown":1e1000,"items":[{}]}`,
		`{"items":[{"alID":1}],"items":[{"alID":2}]}`,
		`{"items":[{"alID":1,"notes":"x"}],"items":[{"alID":2}]}`,
		`{"items":[{"alID":1,"notes":"x"},{"alID":3,"notes":"y"}],"items":[{"alID":2}]}`,
		`{"items":[{"alID":1,"notes":"x"},{"alID":3,"notes":"y"}],"items":[{"alID":2}],"items":[{"alID":9},{}]}`,
		`{"items":[{"expand":{"trs":[{"tags":["a","b"],"tags":[],"tags":[null]}]}}]}`,
		`{"items":[{"expand":{"trs":[{"files":[{"name":"a","length":1},{"name":"b","length":2}],"files":[{"name":"c"}],"files":[{},{"length":9}]}]}}]}`,
		`{"totalItems":"not-a-number"}`,
		`{"items":{}}`,
		`{"items":[`,
		`{"totalPages":1} trailing`,
		`[]`,
		`true`,
		``,
		`{"items":[{"expand":{"trs":[{"files":null,"tags":null}]}}]}`,
		`{"items":[{`,
		`{"items":[5]}`,
		`{"items":[{"notes":5}]}`,
		`{"items":[{"expand":{`,
		`{"items":[{"expand":[]}]}`,
		`{"items":[{"expand":{"trs":{}}}]}`,
		`{"items":[{"expand":{"trs":[5]}}]}`,
		`{"items":[{"expand":{"trs":[{`,
		`{"items":[{"expand":{"trs":[{"url":5}]}}]}`,
		`{"items":[{"expand":{"trs":[{"files":[`,
		`{"items":[{"expand":{"trs":[{"files":[5]}]}}]}`,
		`{"items":[{"expand":{"trs":[{"tags":[5]}]}}]}`,
		`{"items":[{"alID":1}],"items":null}`,
		`{"items":[{"expand":{"trs":[{"files":[{"name":"a","length":1}],"files":null,"tags":["x"],"tags":null}]}}]}`,
		`{"unknown":{"deep":[`,
		`{"`,
		`{"items":[{"`,
		`{"items":[{"expand":{"`,
		`{"items":[{"expand":{"trs":[{"`,
		`{"items":[{"expand":{"unknown":1}}]}`,
		`{"items":[{"expand":{"trs":[{"unknown":1}]}}]}`,
		`{} []`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, elems, gotErr := decodePage(body, maxPageElements)
		var want pbList
		wantErr := json.Unmarshal(body, &want)
		if gotErr != nil {
			boundsCap := errors.Is(gotErr, bounded.ErrArrayCap) || errors.Is(gotErr, bounded.ErrElementBudget)
			if wantErr == nil && !boundsCap {
				t.Errorf("decodePage(%q) = error %v, but json.Unmarshal accepts it (only cardinality caps and the element budget may diverge)", body, gotErr)
			}
			return
		}
		// Accounting invariants on the element count the fetch-wide budget
		// (maxTotalElements, charged by fetchAndAppend) is billed from: an
		// accepted page never reports more elements than its budget, and
		// never fewer than it retained (duplicate occurrences only add).
		if elems > maxPageElements {
			t.Errorf("decodePage(%q) accepted but charged %d elements, over the %d budget", body, elems, maxPageElements)
		}
		if retained := retainedElements(got); elems < retained {
			t.Errorf("decodePage(%q) charged %d elements, fewer than the %d it retained (budget undercharge)", body, elems, retained)
		}
		if wantErr != nil {
			t.Errorf("decodePage(%q) accepted a body json.Unmarshal rejects: %v", body, wantErr)
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("decodePage(%q) = %+v, want json.Unmarshal parity %+v", body, got, want)
		}
	})
}

// retainedElements counts the array elements retained in a decoded pbList
// (items + torrents + files + tags): a lower bound on the decoder's charged
// element count, since duplicate key occurrences and truncated-away elements
// only add charges.
func retainedElements(l pbList) int {
	n := len(l.Items)
	for i := range l.Items {
		trs := l.Items[i].Expand.Trs
		n += len(trs)
		for j := range trs {
			n += len(trs[j].Files) + len(trs[j].Tags)
		}
	}
	return n
}

// FuzzInfoHashRedacted_caseAndWhitespaceInvariant pins the redaction
// predicate's two documented metamorphic contracts over the untrusted SeaDex
// info-hash field - case folding (EqualFold) and surrounding-whitespace
// tolerance (TrimSpace) - and cross-checks that the redaction sentinel can
// never also pass ValidInfoHash (the two gates partition the same input).
func FuzzInfoHashRedacted_caseAndWhitespaceInvariant(f *testing.F) {
	f.Add("<redacted>")
	f.Add("<REDACTED>")
	f.Add("  <Redacted>\t")
	f.Add("redacted")
	f.Add("143ed15e5e3df072ae91adaeb149973a887590dd")
	f.Add("")
	f.Fuzz(func(t *testing.T, h string) {
		got := infoHashRedacted(h)
		if upper := infoHashRedacted(strings.ToUpper(h)); upper != got {
			t.Errorf("infoHashRedacted case invariance for %q = %v after uppercasing, want %v", h, upper, got)
		}
		if padded := infoHashRedacted(" \t\n" + h + "\r\n "); padded != got {
			t.Errorf("infoHashRedacted whitespace invariance for %q = %v after padding, want %v", h, padded, got)
		}
		if got && ValidInfoHash(h) != "" {
			t.Errorf("infoHashRedacted(%q) and ValidInfoHash(%q) both accepted the same value", h, h)
		}
	})
}

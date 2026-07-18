package seadex

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// FuzzDecodePage is a differential fuzz target for the hand-written
// token-level page decoder: for any body, decodePage must match the
// json.Unmarshal oracle it documents parity with. If json.Unmarshal accepts a
// body, decodePage must either accept it with a deeply-equal pbList or reject
// it for exactly one reason - a cardinality cap ("exceeded cap"), the one
// deliberate divergence from stdlib. If json.Unmarshal rejects a body,
// decodePage must reject it too (never accept what stdlib refuses). This
// guards the whole parity surface at once: case-insensitive key matching,
// null-into-container no-ops, duplicate-key overwrite order, trailing-data
// strictness, and scalar type errors. Two known divergences are scoped out:
// decodePage leaves an empty JSON array as a nil slice where json.Unmarshal
// allocates an empty one (normalized before comparing; no consumer
// distinguishes them), and duplicate fold-equal keys within one object are
// skipped entirely - json.Unmarshal MERGES a duplicate container element-wise
// into already-decoded data while decodePage replaces it wholesale, a
// deliberate hostile-input divergence whose supported case (a duplicate
// "expand":null) the unit tests pin directly.
func FuzzDecodePage(f *testing.F) {
	seeds := []string{
		`{"totalItems":1,"totalPages":1,"items":[{"alID":7,"notes":"n","theoreticalBest":"tb","updated":"2026-01-02 03:04:05.000Z","incomplete":true,"expand":{"trs":[{"releaseGroup":"PMR","tracker":"Nyaa","infoHash":"abc","url":"https://nyaa.si/view/1","isBest":true,"dualAudio":true,"files":[{"name":"a.mkv","length":1}],"tags":["best"]}]}}]}`,
		`{"TOTALITEMS":1,"TOTALPAGES":1,"ITEMS":[{"ALID":7,"EXPAND":{"TRS":[]}}]}`,
		`{"totalItems":1,"totalPages":1,"items":[{"alID":7,"expand":{"trs":[{"releaseGroup":"PMR"}]},"expand":null}]}`,
		`{"items":null,"totalPages":null,"totalItems":null}`,
		`null`,
		`{}`,
		`{"unknown":{"deep":[{"nested":true}]},"items":[]}`,
		`{"items":[{"alID":1}],"items":[{"alID":2}]}`,
		`{"totalItems":"not-a-number"}`,
		`{"items":{}}`,
		`{"items":[`,
		`{"totalPages":1} trailing`,
		`[]`,
		`true`,
		``,
		`{"items":[{"expand":{"trs":[{"files":null,"tags":null}]}}]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if hasFoldDuplicateKeys(body) {
			t.Skip("duplicate-key merge semantics deliberately diverge from json.Unmarshal")
		}
		got, gotErr := decodePage(body)
		var want pbList
		wantErr := json.Unmarshal(body, &want)
		if gotErr != nil {
			if wantErr == nil && !strings.Contains(gotErr.Error(), "exceeded cap") {
				t.Errorf("decodePage(%q) = error %v, but json.Unmarshal accepts it (only cardinality caps may diverge)", body, gotErr)
			}
			return
		}
		if wantErr != nil {
			t.Errorf("decodePage(%q) accepted a body json.Unmarshal rejects: %v", body, wantErr)
			return
		}
		if !reflect.DeepEqual(normalizePBList(got), normalizePBList(want)) {
			t.Errorf("decodePage(%q) = %+v, want json.Unmarshal parity %+v", body, got, want)
		}
	})
}

// normalizePBList maps every nil slice to an empty one so the oracle
// comparison ignores the nil-vs-empty divergence (decodePage leaves an empty
// JSON array nil; json.Unmarshal allocates), which no consumer can observe.
func normalizePBList(l pbList) pbList {
	if l.Items == nil {
		l.Items = []pbEntry{}
	}
	for i := range l.Items {
		trs := l.Items[i].Expand.Trs
		if trs == nil {
			l.Items[i].Expand.Trs = []Torrent{}
			continue
		}
		for j := range trs {
			if trs[j].Files == nil {
				trs[j].Files = []File{}
			}
			if trs[j].Tags == nil {
				trs[j].Tags = []string{}
			}
		}
	}
	return l
}

// hasFoldDuplicateKeys reports whether any single JSON object in body carries
// two keys equal under Unicode case folding. Such bodies are excluded from
// the differential oracle (duplicate-container merge-vs-replace divergence);
// a walk error reports false, since both decoders will reject the body anyway.
func hasFoldDuplicateKeys(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		keys     []string
		isObject bool
		wantKey  bool
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				stack = append(stack, frame{isObject: true, wantKey: true})
			case '[':
				stack = append(stack, frame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return false
				}
				if stack[len(stack)-1].isObject {
					stack[len(stack)-1].wantKey = true
				}
			}
			continue
		}
		if len(stack) == 0 {
			return false
		}
		top := &stack[len(stack)-1]
		if !top.isObject {
			continue
		}
		if top.wantKey {
			key, ok := tok.(string)
			if !ok {
				return false
			}
			for _, seen := range top.keys {
				if strings.EqualFold(seen, key) {
					return true
				}
			}
			top.keys = append(top.keys, key)
			top.wantKey = false
			continue
		}
		top.wantKey = true
	}
}

package seadex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// windowSince is the fixed Since every window test in this file walks from,
// and wantWindowFilter is the conjunct it must render to. Both are literals
// rather than derived from windowFilter: the filter is the WIRE contract with
// PocketBase, so a test that computed it from the production helper would keep
// passing through a format change the upstream would reject.
var windowSince = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const wantWindowFilter = `updated>"2026-01-02 03:04:05.000Z"`

// windowOptions is the Options every window test passes.
func windowOptions() Options { return Options{Mode: FetchWindow, Since: windowSince} }

// TestFetchWindowRequestContract pins the windowed walk's wire request across
// both chunks: the changed-since conjunct rides EVERY page, on the second page
// it is ANDed after the keyset cursor's own clause rather than replacing it,
// and the sort stays the immutable (created, id) pair.
//
// That last part is the whole design and the reason to assert the literal query
// string: sorting on `updated` instead would let a record edited mid-walk move
// between chunks and be skipped, which is exactly the class the keyset
// migration closed. A window that paged on its own selection column would
// silently reopen it.
func TestFetchWindowRequestContract(t *testing.T) {
	var filters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filters = append(filters, q.Get("filter"))
		if got := q.Get("sort"); got != "created,id" {
			t.Errorf("sort query = %q, want created,id (a window must still page on the immutable pair)", got)
		}
		if got := q.Get("page"); got != "1" {
			t.Errorf("page query = %q, want 1 (a keyset walk always reads the first page of the remainder)", got)
		}
		if got := q.Get("perPage"); got != strconv.Itoa(perPage) {
			t.Errorf("perPage query = %q, want %d", got, perPage)
		}
		if got := q.Get("expand"); got != "trs" {
			t.Errorf("expand query = %q, want trs", got)
		}
		if len(filters) == 1 {
			// A FULL first chunk keeps the keyset walk going, so a second
			// request carries a cursor as well as the window.
			fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
			return
		}
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, keysetRecords(perPage+1, 1))
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).FetchEntries(context.Background(), windowOptions())
	if err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(entries) != perPage+1 {
		t.Fatalf("entries = %d, want %d", len(entries), perPage+1)
	}
	if len(filters) != 2 {
		t.Fatalf("requests = %d, want 2", len(filters))
	}
	if filters[0] != wantWindowFilter {
		t.Errorf("first page filter = %q, want exactly %q (no cursor yet, so the window is the whole filter)",
			filters[0], wantWindowFilter)
	}
	// keysetRecords shares one created value across the chunk, so the cursor is
	// the last record's (created, id) pair.
	wantCursor := fmt.Sprintf(`(created>"2026-01-02 03:04:05.000Z"||(created="2026-01-02 03:04:05.000Z"&&id>"rec%06d"))`, perPage)
	want := wantCursor + "&&" + wantWindowFilter
	if filters[1] != want {
		t.Errorf("second page filter = %q, want %q (the cursor clause ANDed with the window's)", filters[1], want)
	}
}

// TestFetchWindowFullModeSendsNoWindowConjunct is the negative half of the
// contract above: a full walk must not narrow itself to a window, and its first
// page carries no filter at all. Without this a Since accidentally honoured in
// FetchFull would truncate the catalogue every reconcile while every window
// test stayed green.
func TestFetchWindowFullModeSendsNoWindowConjunct(t *testing.T) {
	var filters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters = append(filters, r.URL.Query().Get("filter"))
		fmt.Fprintf(w, `{"totalItems":1,"totalPages":1,"items":[%s]}`, keysetRecords(1, 1))
	}))
	defer server.Close()

	// A Since set on a FULL fetch is ignored, which is what makes FetchFull the
	// safe zero value of the mode.
	if _, err := NewClient(server.Client(), server.URL, 0, nil).
		FetchEntries(context.Background(), Options{Since: windowSince}); err != nil {
		t.Fatalf("FetchEntries returned error: %v", err)
	}
	if len(filters) != 1 {
		t.Fatalf("requests = %d, want 1", len(filters))
	}
	if filters[0] != "" {
		t.Errorf("full-walk first page filter = %q, want empty", filters[0])
	}
}

// TestFetchWindowEmptyResultSucceeds pins the completeness split's headline
// case: an empty window is a SUCCESSFUL fetch of nothing (measured upstream, 6
// of 90 days carried no change at all), while the identical response is an
// error for a full walk because SeaDex is never legitimately empty. Both arms
// are asserted here, against the same server, because the guard's value is
// entirely in the difference.
func TestFetchWindowEmptyResultSucceeds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":0,"totalPages":1,"items":[]}`)
	}))
	defer server.Close()

	client := NewClient(server.Client(), server.URL, 0, nil)
	entries, err := client.FetchEntries(context.Background(), windowOptions())
	if err != nil {
		t.Fatalf("windowed FetchEntries returned error: %v (an empty window is a successful tick)", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %d, want 0", len(entries))
	}
	if _, err := client.FetchEntries(context.Background(), Options{}); err == nil {
		t.Error("full FetchEntries returned nil error on an empty catalogue, want the empty-catalogue refusal")
	}
}

// TestFetchWindowBelowHalfShortfallSucceeds pins the below-half guard as
// full-mode-only. That guard refuses a walk that lost more than half of what
// the API said the collection holds, which is a catalogue-scale judgment: a
// window's totalItems counts MATCHING records, and a short terminal chunk of
// them is ordinary. Applying it here would fail most productive ticks.
func TestFetchWindowBelowHalfShortfallSucceeds(t *testing.T) {
	// One record delivered against a reported total of 1000: far below half,
	// and terminal because the chunk is short.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"totalItems":1000,"totalPages":%d,"items":[%s]}`, 1000/perPage+1, keysetRecords(1, 1))
	}))
	defer server.Close()

	logger, recorder := capture.New()
	client := NewClient(server.Client(), server.URL, 0, logger)
	entries, err := client.FetchEntries(context.Background(), windowOptions())
	if err != nil {
		t.Fatalf("windowed FetchEntries returned error: %v (the below-half shortfall is a full-mode guard)", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1", len(entries))
	}
	// The catalogue-scale diagnostics are skipped too, not merely downgraded: a
	// count-mismatch WARN on every tick would poison the signal for the full
	// walk that actually means it.
	for _, msg := range recorder.Messages() {
		if strings.Contains(msg, "shrunk") || strings.Contains(msg, "fewer entries") {
			t.Errorf("windowed fetch emitted a catalogue-scale diagnostic: %q", msg)
		}
	}

	if _, err := client.FetchEntries(context.Background(), Options{}); err == nil {
		t.Error("full FetchEntries returned nil error on a below-half shortfall, want the truncated-catalogue refusal")
	}
}

// TestFetchWindowRejectsZeroSince pins the one thing a FetchWindow must never
// do silently: fall back to a full fetch. The zero time is also what a failed
// timestamp parse yields, so accepting it would turn a malformed value into a
// whole-catalogue walk with the completeness guards switched off - the worst of
// both modes. No request is made at all.
func TestFetchWindowRejectsZeroSince(t *testing.T) {
	var reqs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs++
		fmt.Fprintf(w, `{"totalItems":1,"totalPages":1,"items":[%s]}`, keysetRecords(1, 1))
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).
		FetchEntries(context.Background(), Options{Mode: FetchWindow})
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want a zero-Since error", len(entries))
	}
	if entries != nil {
		t.Errorf("entries = %d items, want nil", len(entries))
	}
	if !strings.Contains(err.Error(), "non-zero Since") {
		t.Errorf("error = %q, want it to name the missing Since", err.Error())
	}
	if reqs != 0 {
		t.Errorf("requests = %d, want 0 (the refusal is before any wire call)", reqs)
	}
}

// TestFetchWindowKeepsStructuralGuards pins the other half of the completeness
// split: every guard that judges the WIRE RESPONSE rather than the catalogue
// still refuses a window. A window is a smaller result, not a less trustworthy
// one, so an entry with no usable AniList ID (which the matcher would read as
// an ordinary unmapped entry) and a record repeated across chunks (which can
// stand in for one that was dropped) must fail a tick exactly as they fail a
// reconcile.
func TestFetchWindowKeepsStructuralGuards(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		// handler serves the chunk for request number n (1-based).
		handler func(n int) string
		want    string
	}{
		"non-positive AniList ID": {
			handler: func(int) string {
				return `{"totalItems":1,"totalPages":1,"items":[{"alID":0,"expand":{"trs":[]}}]}`
			},
			want: "page 1 item 1 has invalid AniList ID 0",
		},
		"negative AniList ID": {
			handler: func(int) string {
				return `{"totalItems":1,"totalPages":1,"items":[{"alID":-7,"expand":{"trs":[]}}]}`
			},
			want: "page 1 item 1 has invalid AniList ID -7",
		},
		"AniList ID repeated across pages": {
			handler: func(n int) string {
				if n == 1 {
					// A FULL chunk (ids 1..perPage) keeps the walk going.
					return fmt.Sprintf(`{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, fullKeysetChunk(1))
				}
				return fmt.Sprintf(`{"totalItems":%d,"totalPages":2,"items":[%s]}`, perPage+1, keysetRecords(1, 1))
			},
			want: "page 2 repeats AniList ID 1",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var reqs int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reqs++
				fmt.Fprint(w, tc.handler(reqs))
			}))
			defer server.Close()

			logger, _ := capture.New()
			entries, err := NewClient(server.Client(), server.URL, 0, logger).
				FetchEntries(context.Background(), windowOptions())
			if err == nil {
				t.Fatalf("FetchEntries = %d entries, want a structural error in window mode", len(entries))
			}
			if entries != nil {
				t.Errorf("entries = %d items, want nil on a structural error", len(entries))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestFetchWindowKeepsMetadataConsistencyGuard pins the one catalogue-metadata
// guard window mode KEEPS: totalItems cannot exceed what the reported pages can
// hold, whatever the filter. It is kept because it catches the upstream
// contradicting ITSELF, which is not a statement about catalogue size.
func TestFetchWindowKeepsMetadataConsistencyGuard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"totalItems":%d,"totalPages":1,"items":[%s]}`, perPage+1, keysetRecords(1, 1))
	}))
	defer server.Close()

	entries, err := NewClient(server.Client(), server.URL, 0, nil).
		FetchEntries(context.Background(), windowOptions())
	if err == nil {
		t.Fatalf("FetchEntries = %d entries, want the metadata-consistency refusal", len(entries))
	}
	if !strings.Contains(err.Error(), "cannot fit the reported") {
		t.Errorf("error = %q, want the reported-total-versus-pages refusal", err.Error())
	}
}

// TestCountWindowRequestAndTotal pins the probe's whole contract: it asks for
// ONE record of ids only (that is what makes it ~88 bytes and therefore
// affordable every tick), carries the same changed-since conjunct the fetch
// would, and answers the response's totalItems rather than counting items.
func TestCountWindowRequestAndTotal(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		fmt.Fprint(w, `{"page":1,"perPage":1,"totalItems":37,"totalPages":37,"items":[{"id":"rec000001"}]}`)
	}))
	defer server.Close()

	n, err := NewClient(server.Client(), server.URL, 0, nil).CountWindow(context.Background(), windowSince)
	if err != nil {
		t.Fatalf("CountWindow returned error: %v", err)
	}
	if n != 37 {
		t.Errorf("CountWindow = %d, want the reported totalItems 37 (not the 1 item served)", n)
	}
	if got == nil {
		t.Fatal("CountWindow made no request")
	}
	if got.URL.Path != entriesPath {
		t.Errorf("path = %q, want %q", got.URL.Path, entriesPath)
	}
	q := got.URL.Query()
	for _, tc := range []struct{ key, want string }{
		{"perPage", "1"},
		{"page", "1"},
		{"fields", "id"},
		{"filter", wantWindowFilter},
	} {
		if v := q.Get(tc.key); v != tc.want {
			t.Errorf("%s query = %q, want %q", tc.key, v, tc.want)
		}
	}
	// The probe must not pull the torrents relation: expanding it is what makes
	// a real page megabytes, and it is the entire cost the probe exists to skip.
	if v := q.Get("expand"); v != "" {
		t.Errorf("expand query = %q, want empty (a probe that expands trs is not a probe)", v)
	}
}

// TestCountWindowRejectsNegativeTotal pins the degenerate-response arm.
// PocketBase answers totalItems: -1 when a caller asks it to skip the count, so
// reading a negative as "nothing changed" would report a clean empty window
// from a response that counted nothing at all - and the tick would go on
// skipping work forever while looking healthy.
func TestCountWindowRejectsNegativeTotal(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		body    string
		wantErr string
	}{
		"skip-count sentinel": {`{"totalItems":-1,"items":[]}`, "negative total"},
		"arbitrary negative":  {`{"totalItems":-500,"items":[]}`, "negative total"},
		"undecodable body":    {`{"totalItems":`, "decode window count"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			n, err := NewClient(server.Client(), server.URL, 0, nil).CountWindow(context.Background(), windowSince)
			if err == nil {
				t.Fatalf("CountWindow = %d, want an error", n)
			}
			if n != 0 {
				t.Errorf("CountWindow = %d, want 0 alongside the error", n)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestCountWindowZeroTotalIsNotAnError is the boundary the guard above must not
// swallow: zero IS the ordinary quiet-upstream answer, and refusing it would
// turn most ticks into failures.
func TestCountWindowZeroTotalIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"totalItems":0,"totalPages":0,"items":[]}`)
	}))
	defer server.Close()

	n, err := NewClient(server.Client(), server.URL, 0, nil).CountWindow(context.Background(), windowSince)
	if err != nil {
		t.Fatalf("CountWindow returned error: %v (a quiet upstream is not a fault)", err)
	}
	if n != 0 {
		t.Errorf("CountWindow = %d, want 0", n)
	}
}

// TestCountWindowRejectsZeroSince mirrors FetchEntries' refusal at the probe.
// A zero since would count the whole collection, so the tick would read every
// window as oversize and freeze the fast path permanently - with no request
// made, the refusal is unambiguous.
func TestCountWindowRejectsZeroSince(t *testing.T) {
	var reqs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs++
		fmt.Fprint(w, `{"totalItems":5,"items":[]}`)
	}))
	defer server.Close()

	n, err := NewClient(server.Client(), server.URL, 0, nil).CountWindow(context.Background(), time.Time{})
	if err == nil {
		t.Fatalf("CountWindow = %d, want a zero-since error", n)
	}
	if n != 0 {
		t.Errorf("CountWindow = %d, want 0 alongside the error", n)
	}
	if !strings.Contains(err.Error(), "non-zero since") {
		t.Errorf("error = %q, want it to name the missing since", err.Error())
	}
	if reqs != 0 {
		t.Errorf("requests = %d, want 0 (the refusal is before any wire call)", reqs)
	}
}

// TestCountWindowNormalizesSinceToUTC pins the probe's timestamp rendering
// against the one input shape that would silently shift the window: a Since in
// a non-UTC location. PocketBase compares the literal, so a filter rendered in
// local time would ask for a window hours away from the one the caller meant -
// wider (re-fetching settled records) or narrower (missing changes entirely),
// depending on the sign of the offset.
func TestCountWindowNormalizesSinceToUTC(t *testing.T) {
	var filter string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter = r.URL.Query().Get("filter")
		fmt.Fprint(w, `{"totalItems":0,"items":[]}`)
	}))
	defer server.Close()

	// The same instant as windowSince, carried in a +05:00 zone.
	zoned := windowSince.In(time.FixedZone("test", 5*60*60))
	if _, err := NewClient(server.Client(), server.URL, 0, nil).CountWindow(context.Background(), zoned); err != nil {
		t.Fatalf("CountWindow returned error: %v", err)
	}
	if filter != wantWindowFilter {
		t.Errorf("filter = %q, want %q (the same instant rendered in UTC)", filter, wantWindowFilter)
	}
}

// TestMaxWindowEntriesIsOnePage pins the bound's DEFINITION, not its value: the
// tick's oversize check exists to keep a window to ONE request, and the reason a
// caller over the bound must defer to a full pass rather than fetch a prefix is
// that the walk sorts on `created`, so page 1 of an oversized window holds the
// OLDEST records - the opposite of what a freshness pass wants. If the constant
// ever drifted off perPage, that whole argument would quietly stop holding.
func TestMaxWindowEntriesIsOnePage(t *testing.T) {
	if MaxWindowEntries != perPage {
		t.Errorf("MaxWindowEntries = %d, want perPage %d (the bound is one request, by construction)",
			MaxWindowEntries, perPage)
	}
}

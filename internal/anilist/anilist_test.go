package anilist

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/cplieger/jsoncap/v2"
)

func TestParseMedia(t *testing.T) {
	raw := []byte(`{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"Sousou no Frieren","english":"Frieren","native":"x"}}}}`)
	m, err := parseMedia(raw)
	if err != nil {
		t.Fatalf("parseMedia: %v", err)
	}
	if m.Format != "TV" || m.Year != 2023 {
		t.Errorf("format/year = %q/%d, want TV/2023", m.Format, m.Year)
	}
	want := []string{"Sousou no Frieren", "Frieren", "x"}
	if !slices.Equal(m.Titles, want) {
		t.Errorf("titles = %v, want %v", m.Titles, want)
	}
}

func TestParseMediaYearFallsBackToStartDate(t *testing.T) {
	raw := []byte(`{"data":{"Media":{"format":"MOVIE","startDate":{"year":2020},"title":{"romaji":"A"}}}}`)
	m, err := parseMedia(raw)
	if err != nil {
		t.Fatalf("parseMedia: %v", err)
	}
	if m.Year != 2020 {
		t.Errorf("year = %d, want startDate fallback 2020", m.Year)
	}
}

// TestParseMediaImplausibleYearFallsBack pins toMedia's year gate: an
// impossible untrusted wire year carries no evidence, so it must never become
// match.findByTitle's hard constraint - it falls back to a plausible startDate
// year, then to the unknown sentinel 0.
func TestParseMediaImplausibleYearFallsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want int
	}{
		"negative seasonYear falls back to startDate":     {raw: `{"data":{"Media":{"format":"TV","seasonYear":-2020,"startDate":{"year":2020},"title":{"romaji":"A"}}}}`, want: 2020},
		"over-four-digit seasonYear falls back":           {raw: `{"data":{"Media":{"format":"TV","seasonYear":20200,"startDate":{"year":2020},"title":{"romaji":"A"}}}}`, want: 2020},
		"three-digit seasonYear falls back to startDate":  {raw: `{"data":{"Media":{"format":"TV","seasonYear":999,"startDate":{"year":2020},"title":{"romaji":"A"}}}}`, want: 2020},
		"both implausible yields unknown":                 {raw: `{"data":{"Media":{"format":"TV","seasonYear":20200,"startDate":{"year":-5},"title":{"romaji":"A"}}}}`, want: 0},
		"implausible seasonYear with over-range fallback": {raw: `{"data":{"Media":{"format":"TV","seasonYear":-1,"startDate":{"year":10000},"title":{"romaji":"A"}}}}`, want: 0},
		"plausible seasonYear is kept":                    {raw: `{"data":{"Media":{"format":"TV","seasonYear":2023,"startDate":{"year":2020},"title":{"romaji":"A"}}}}`, want: 2023},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := parseMedia([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseMedia: %v", err)
			}
			if m.Year != tc.want {
				t.Errorf("Year = %d, want %d", m.Year, tc.want)
			}
		})
	}
}

func TestParseMediaNotFoundCarriesMessage(t *testing.T) {
	raw := []byte(`{"data":{"Media":null},"errors":[{"message":"Not Found."}]}`)
	_, err := parseMedia(raw)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := err.Error(); got != "anilist: media not found: Not Found." {
		t.Errorf("err.Error() = %q, want upstream message preserved", got)
	}
}

// TestParseMediaNotFoundClassification pins the negative-memoization boundary:
// only an explicit Media null with no error, or AniList's verified not-found
// error shape (status 404 / message "Not Found."), may satisfy
// errors.Is(err, ErrNotFound). An HTTP-200 GraphQL failure or a malformed
// envelope must NOT — the matcher persists ErrNotFound as NotFound:true, so
// misclassifying a transient failure would silently suppress the id forever.
func TestParseMediaNotFoundClassification(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantErr      bool
		wantNotFound bool
	}{
		{name: "empty envelope", raw: `{}`, wantErr: true, wantNotFound: false},
		{name: "missing Media field", raw: `{"data":{}}`, wantErr: true, wantNotFound: false},
		{name: "null Media with non-not-found error", raw: `{"data":{"Media":null},"errors":[{"message":"Internal Server Error"}]}`, wantErr: true, wantNotFound: false},
		{name: "missing data with error", raw: `{"errors":[{"message":"bad request"}]}`, wantErr: true, wantNotFound: false},
		{name: "explicit null no error", raw: `{"data":{"Media":null}}`, wantErr: true, wantNotFound: true},
		{name: "null Media with status 404", raw: `{"data":{"Media":null},"errors":[{"message":"Something went wrong","status":404}]}`, wantErr: true, wantNotFound: true},
		{name: "null Media with Not Found message", raw: `{"data":{"Media":null},"errors":[{"message":"Not Found."}]}`, wantErr: true, wantNotFound: true},
		{name: "embedded control cannot launder into Not Found", raw: `{"data":{"Media":null},"errors":[{"message":"Not\nFound."}]}`, wantErr: true, wantNotFound: false},
		{name: "leading newline cannot launder into Not Found", raw: `{"data":{"Media":null},"errors":[{"message":"\nNot Found."}]}`, wantErr: true, wantNotFound: false},
		{name: "trailing newline cannot launder into Not Found", raw: `{"data":{"Media":null},"errors":[{"message":"Not Found.\n"}]}`, wantErr: true, wantNotFound: false},
		{name: "carriage return cannot launder into Not Found", raw: `{"data":{"Media":null},"errors":[{"message":"\rNot Found."}]}`, wantErr: true, wantNotFound: false},
		{name: "tab cannot launder into Not Found", raw: `{"data":{"Media":null},"errors":[{"message":"\tNot Found."}]}`, wantErr: true, wantNotFound: false},
		{name: "tab and carriage-return wrapping cannot launder into Not Found", raw: `{"data":{"Media":null},"errors":[{"message":"\tNot Found.\r"}]}`, wantErr: true, wantNotFound: false},
		{name: "null Media with Not Found plus second error", raw: `{"data":{"Media":null},"errors":[{"message":"Not Found."},{"message":"Internal Server Error"}]}`, wantErr: true, wantNotFound: false},
		{name: "non-object Media fails decode", raw: `{"data":{"Media":123}}`, wantErr: true, wantNotFound: false},
		{name: "null Media beside a type-mismatched errors field fails decode", raw: `{"data":{"Media":null},"errors":{"message":"boom"}}`, wantErr: true, wantNotFound: false},
		{name: "type-mismatched data field fails decode", raw: `{"data":"nope"}`, wantErr: true, wantNotFound: false},
		{name: "partial response with non-null Media and errors", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"A"}}},"errors":[{"message":"field resolution failed"}]}`, wantErr: true, wantNotFound: false},
		{name: "empty Media object has no usable title", raw: `{"data":{"Media":{}}}`, wantErr: true, wantNotFound: false},
		{name: "whitespace-only titles are not usable", raw: `{"data":{"Media":{"title":{"romaji":" ","english":"\t"}}}}`, wantErr: true, wantNotFound: false},
		{name: "punctuation-only title normalizes to no match key", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"!!!"}}}}`, wantErr: true, wantNotFound: false},
		{name: "decorated title keeps a match key", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"(A)"}}}}`, wantErr: false, wantNotFound: false},
		{name: "invalid UTF-8 in title rejected before decode", raw: "{\"data\":{\"Media\":{\"format\":\"TV\",\"title\":{\"romaji\":\"A\xff\"}}}}", wantErr: true, wantNotFound: false},
		{name: "lone surrogate escape decoding to U+FFFD rejected", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"A\ud800"}}}}`, wantErr: true, wantNotFound: false},
		{name: "explicit null errors field is not an envelope error", raw: `{"data":{"Media":{"format":"TV","title":{"romaji":"A"}}},"errors":null}`, wantErr: false, wantNotFound: false},
		{name: "media present", raw: `{"data":{"Media":{"format":"TV","seasonYear":2023,"title":{"romaji":"A"}}}}`, wantErr: false, wantNotFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMedia([]byte(tt.raw))
			if tt.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("parseMedia: %v", err)
			}
			if got := errors.Is(err, ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(err, ErrNotFound) = %v (err = %v), want %v", got, err, tt.wantNotFound)
			}
		})
	}
}

func TestParseMediaPage(t *testing.T) {
	raw := []byte(`{"data":{"Page":{"media":[` +
		`{"id":1,"format":"TV","seasonYear":2023,"title":{"romaji":"A"}},` +
		`{"id":2,"format":"MOVIE","startDate":{"year":2019},"title":{"romaji":"B","english":"B"}},` +
		`{"id":3,"format":"TV","seasonYear":20210,"startDate":{"year":2021},"title":{"romaji":"C"}},` +
		`{"id":4,"format":"TV","seasonYear":-2021,"startDate":{"year":-5},"title":{"romaji":"D"}}` +
		`]}}}`)
	out, err := parseMediaPage(raw)
	if err != nil {
		t.Fatalf("parseMediaPage: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	if out[1].Year != 2023 {
		t.Errorf("id 1 year = %d, want 2023", out[1].Year)
	}
	if out[2].Year != 2019 {
		t.Errorf("id 2 year = %d, want startDate fallback 2019", out[2].Year)
	}
	// The batch parser routes through the same toMedia year gate as the single
	// parser: an implausible seasonYear falls back to startDate, and implausible
	// evidence on both fields yields the unknown sentinel 0.
	if out[3].Year != 2021 {
		t.Errorf("id 3 year = %d, want implausible seasonYear to fall back to 2021", out[3].Year)
	}
	if out[4].Year != 0 {
		t.Errorf("id 4 year = %d, want unknown sentinel 0", out[4].Year)
	}
	if !slices.Equal(out[2].Titles, []string{"B"}) {
		t.Errorf("id 2 titles = %v, want deduped [B]", out[2].Titles)
	}
}

func TestParseMediaPageErrorFailsBatch(t *testing.T) {
	raw := []byte(`{"errors":[{"message":"bad request"}]}`)
	if _, err := parseMediaPage(raw); err == nil {
		t.Error("a GraphQL-level error must fail the batch")
	}
}

// TestParseMediaFieldLimits pins the per-field wire rules on the untrusted
// AniList boundary in BOTH the single and batch parsers: a boundary-sized title
// is accepted while a max+1 title is rejected outright (never truncated, which
// could forge a normalized-title match), so a hostile near-body-cap payload
// cannot inflate the memo or state.json.
//
// A defective FORMAT is deliberately NOT a rejection (l-f140): knownFormat
// republishes the field as a canonical mediatype token, so an over-long,
// unrecognized, or unsafe wire value costs the record only its arr hint - the
// bound on Media.Format is the vocabulary itself, not a byte cap, and the
// record keeps the usable titles it would otherwise have lost to a permanent
// negative memo.
func TestParseMediaFieldLimits(t *testing.T) {
	okTitle := strings.Repeat("a", maxTitleBytes)
	bigTitle := strings.Repeat("a", maxTitleBytes+1)
	bigFormat := strings.Repeat("F", 65)

	tests := []struct {
		name       string
		fields     string // media object body, without the enclosing braces
		wantErr    bool
		wantFormat string   // expected Media.Format when the record is accepted
		wantTitles []string // expected Media.Titles when the record is accepted (nil = unchecked)
	}{
		{name: "boundary-sized romaji accepted", fields: `"title":{"romaji":"` + okTitle + `"}`, wantErr: false},
		{name: "over-limit romaji rejected", fields: `"title":{"romaji":"` + bigTitle + `"}`, wantErr: true},
		// An over-limit SIBLING costs the record that title and nothing else
		// (h-f1): the three titles are independent facts, each memoized and
		// republished on its own, so one over-cap alias must not take the usable
		// ones with it into a permanent negative memo.
		{name: "over-limit english drops only that title", fields: `"title":{"romaji":"A","english":"` + bigTitle + `"}`, wantTitles: []string{"A"}},
		{name: "over-limit native drops only that title", fields: `"title":{"romaji":"A","native":"` + bigTitle + `"}`, wantTitles: []string{"A"}},
		{name: "known format canonical", fields: `"format":"MOVIE","title":{"romaji":"A"}`, wantFormat: "MOVIE"},
		{name: "lower-cased format canonicalized", fields: `"format":"tv","title":{"romaji":"A"}`, wantFormat: "TV"},
		{name: "over-long format collapses to unknown", fields: `"format":"` + bigFormat + `","title":{"romaji":"A"}`, wantFormat: ""},
		{name: "surrounding whitespace format canonicalized", fields: `"format":"TV\n","title":{"romaji":"A"}`, wantFormat: "TV"},
		{name: "bidi override in format collapses to unknown", fields: `"format":"TV\u202e","title":{"romaji":"A"}`, wantFormat: ""},
		{name: "unrecognized format collapses to unknown", fields: `"format":"NOT_A_FORMAT","title":{"romaji":"A"}`, wantFormat: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			single := []byte(`{"data":{"Media":{` + tt.fields + `}}}`)
			media, err := parseMedia(single)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMedia err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && media.Format != tt.wantFormat {
				t.Errorf("parseMedia format = %q, want %q", media.Format, tt.wantFormat)
			}
			if err == nil && tt.wantTitles != nil && !slices.Equal(media.Titles, tt.wantTitles) {
				t.Errorf("parseMedia titles = %v, want %v (the defective title dropped, its siblings kept)",
					media.Titles, tt.wantTitles)
			}
			batch := []byte(`{"data":{"Page":{"media":[{"id":1,` + tt.fields + `}]}}}`)
			page, err := parseMediaPage(batch)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMediaPage err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && page[1].Format != tt.wantFormat {
				t.Errorf("parseMediaPage format = %q, want %q", page[1].Format, tt.wantFormat)
			}
			if err == nil && tt.wantTitles != nil && !slices.Equal(page[1].Titles, tt.wantTitles) {
				t.Errorf("parseMediaPage titles = %v, want %v", page[1].Titles, tt.wantTitles)
			}
		})
	}
}

// TestParseMediaKeepsTitlesWhenOnlyFormatIsDefective pins l-f140's whole point
// at the consumer-visible level: a record whose ONLY defect is its format field
// must still yield its usable titles, because ErrRecordUnusable is a definitive
// answer the matcher negative-memoizes - rejecting the record would have cost it
// every future title match until the memo expired, with an overrides.json entry
// as the operator's only remedy.
func TestParseMediaKeepsTitlesWhenOnlyFormatIsDefective(t *testing.T) {
	defective := map[string]string{
		"over-long":      strings.Repeat("F", 4096),
		"control rune":   "TV\\u0007",
		"bidi override":  "TV\\u202e",
		"lone surrogate": "\\ud800",
	}
	for name, format := range defective {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{"data":{"Media":{"format":"` + format + `","title":{"romaji":"Keeper"}}}}`)
			media, err := parseMedia(raw)
			if err != nil {
				t.Fatalf("parseMedia: %v, want the record accepted with its titles", err)
			}
			if !slices.Equal(media.Titles, []string{"Keeper"}) {
				t.Errorf("titles = %v, want [Keeper]", media.Titles)
			}
			if media.Format != "" {
				t.Errorf("format = %q, want the unknown sentinel", media.Format)
			}
		})
	}
}

// TestParseMediaDropsUnsafeTitleTextKeepingSiblings pins the title single-line
// guard on its own: each payload carries ONE unsafe title field plus a safe
// sibling, so the ONLY thing that can produce the expected result is the
// runesafe.SanitizeSingleLine check in toMedia running and dropping exactly that
// member.
//
// This assertion is STRONGER than the "record rejected" one it replaces (h-f1
// stopped a defective alias from killing its siblings, which made rejection the
// wrong expectation). Rejection could be produced by any record-wide failure;
// "the unsafe member is absent AND the safe member is present" can only be
// produced by the per-title guard. So dropping the guard still fails here, which
// is what this test exists for.
func TestParseMediaDropsUnsafeTitleTextKeepingSiblings(t *testing.T) {
	tests := map[string]struct {
		raw    string
		unsafe string
	}{
		"romaji newline with safe sibling":        {`{"data":{"Media":{"title":{"romaji":"A\nB","english":"Safe"}}}}`, "A\nB"},
		"english C1 control with safe sibling":    {`{"data":{"Media":{"title":{"romaji":"Safe","english":"A\u009bB"}}}}`, "A\u009bB"},
		"native line separator with safe sibling": {`{"data":{"Media":{"title":{"romaji":"Safe","native":"A\u2028B"}}}}`, "A\u2028B"},
		"romaji bidi override with safe sibling":  {`{"data":{"Media":{"title":{"romaji":"A\u202eB","english":"Safe"}}}}`, "A\u202eB"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			media, err := parseMedia([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseMedia(%s) = %v, want the record accepted with its safe sibling", tc.raw, err)
			}
			if slices.Contains(media.Titles, tc.unsafe) {
				t.Errorf("titles = %v, want the unsafe member %q DROPPED - it must never reach the memo",
					media.Titles, tc.unsafe)
			}
			if !slices.Contains(media.Titles, "Safe") {
				t.Errorf("titles = %v, want the safe sibling kept", media.Titles)
			}
		})
	}
}

// TestParseMediaRejectsARecordWhoseEveryTitleIsUnsafe pins the other side of the
// same guard: dropping members is not a licence to accept a record with nothing
// left. An all-unsafe title set has no survivor, so it is still the permanent
// ErrRecordUnusable a definitive negative memo is built on.
func TestParseMediaRejectsARecordWhoseEveryTitleIsUnsafe(t *testing.T) {
	raw := `{"data":{"Media":{"title":{"romaji":"A\nB","english":"C\u009bD","native":"E\u2028F"}}}}`
	if _, err := parseMedia([]byte(raw)); !errors.Is(err, ErrRecordUnusable) {
		t.Errorf("parseMedia = %v, want ErrRecordUnusable when no title survives the per-title guard", err)
	}
}

func TestParseMediaPageNullableEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "missing data", raw: `{}`, wantErr: true},
		{name: "null Page", raw: `{"data":{"Page":null}}`, wantErr: true},
		{name: "missing Page", raw: `{"data":{}}`, wantErr: true},
		{name: "missing media", raw: `{"data":{"Page":{}}}`, wantErr: true},
		{name: "null media", raw: `{"data":{"Page":{"media":null}}}`, wantErr: true},
		{name: "non-array media (string) rejected", raw: `{"data":{"Page":{"media":"nope"}}}`, wantErr: true},
		{name: "non-array media (object) rejected", raw: `{"data":{"Page":{"media":{}}}}`, wantErr: true},
		{name: "type-mismatched element fails batch", raw: `{"data":{"Page":{"media":[{"id":"x","title":{"romaji":"A"}}]}}}`, wantErr: true},
		{name: "valid page beside a type-mismatched errors field fails batch", raw: `{"data":{"Page":{"media":[]}},"errors":{"message":"boom"}}`, wantErr: true},
		{name: "duplicate media ending in null", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A"}}],"media":null}}}`, wantErr: true},
		{name: "record with whitespace-only title fails batch", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":" "}}]}}}`, wantErr: true},
		{name: "record with punctuation-only title fails batch", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"!!!"}}]}}}`, wantErr: true},
		{name: "record with no title fails batch", raw: `{"data":{"Page":{"media":[{"id":1}]}}}`, wantErr: true},
		{name: "invalid UTF-8 in title rejected before decode", raw: "{\"data\":{\"Page\":{\"media\":[{\"id\":1,\"title\":{\"romaji\":\"A\xff\"}}]}}}", wantErr: true},
		{name: "lone surrogate escape decoding to U+FFFD fails batch", raw: `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A\ud800"}}]}}}`, wantErr: true},
		{name: "explicit null errors field is not an envelope error", raw: `{"data":{"Page":{"media":[]}},"errors":null}`, wantErr: false},
		{name: "empty media array", raw: `{"data":{"Page":{"media":[]}}}`, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := parseMediaPage([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatal("a malformed envelope must fail the batch, got nil error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMediaPage: %v", err)
			}
			if len(out) != 0 {
				t.Errorf("len = %d, want empty map for an explicit empty media array", len(out))
			}
		})
	}
}

// TestParseMediaPageBoundsMediaCardinality pins the wire-cardinality bound on
// the untrusted batch decode: exactly batchSize compact valid records parse,
// while batchSize+1 records fail the whole batch as an envelope error (never
// errBatchRecord) rejected before the extra element is decoded — so a hostile
// endpoint cannot expand an attacker-sized media array into []gqlMedia under
// the 1 MiB body cap.
func TestParseMediaPageBoundsMediaCardinality(t *testing.T) {
	page := func(n int) []byte {
		records := make([]string, n)
		for i := range records {
			id := strconv.Itoa(i + 1)
			records[i] = `{"id":` + id + `,"title":{"romaji":"T` + id + `"}}`
		}
		return []byte(`{"data":{"Page":{"media":[` + strings.Join(records, ",") + `]}}}`)
	}

	out, err := parseMediaPage(page(batchSize))
	if err != nil {
		t.Fatalf("parseMediaPage with %d records: %v", batchSize, err)
	}
	if len(out) != batchSize {
		t.Errorf("len = %d, want %d", len(out), batchSize)
	}

	_, err = parseMediaPage(page(batchSize + 1))
	if err == nil {
		t.Fatalf("parseMediaPage with %d records must be rejected", batchSize+1)
	}
	if errors.Is(err, errBatchRecord) {
		t.Errorf("over-cardinality must be an envelope error, got record-local %v", err)
	}
}

// TestObserveRateHeadersCapsResetWindow pins the proactive (pre-429) path's
// ceiling. It is the POLITENESS ceiling, not the per-attempt one: this path
// penalizes the shared throttle without any retry loop involved, so
// maxRetryAfter would be the wrong bound here (l-f7).
func TestObserveRateHeadersCapsResetWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(http.DefaultClient, "https://example.invalid/graphql", WithRate(30))
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("X-RateLimit-Remaining", "1")
		resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))

		client.observeRateHeaders(resp)

		if wait := client.throttle.reserve(); wait != maxThrottlePenalty {
			t.Errorf("low-budget reset wait = %v, want exactly the %v politeness ceiling", wait, maxThrottlePenalty)
		}
	})
}

// TestObserveRateHeadersHonoursARealWindowBeyondAMinute is the case the single
// ceiling got wrong: a window AniList actually states, longer than a minute but
// nowhere near absurd, must be honoured in FULL rather than truncated to 60s and
// then discarded - which is what left the client probing a rate-limited upstream
// once a minute for the rest of the real window.
func TestObserveRateHeadersHonoursARealWindowBeyondAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const stated = 3 * time.Minute // > maxRetryAfter, < maxThrottlePenalty
		client := NewClient(http.DefaultClient, "https://example.invalid/graphql", WithRate(30))
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("X-RateLimit-Remaining", "1")
		resp.Header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(stated).Unix(), 10))

		client.observeRateHeaders(resp)

		wait := client.throttle.reserve()
		if wait <= maxRetryAfter {
			t.Errorf("wait = %v, want the full stated window (~%v); truncating at the per-attempt ceiling %v is the defect",
				wait, stated, maxRetryAfter)
		}
		if wait > stated {
			t.Errorf("wait = %v, want no more than the stated %v", wait, stated)
		}
	})
}

// parseMedia is parseMediaForID without the identity invariant — a test-local
// shorthand for exercising the envelope contract (production always binds the
// requested id via Fetch -> parseMediaForID).
func parseMedia(raw []byte) (Media, error) {
	return parseMediaForID(raw, 0)
}

// reserve claims the next slot and returns how long to wait before using it.
// Test-only observation helper for throttle state; production code reserves
// via wait/reserveSlot.
func (t *throttle) reserve() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	return t.reserveSlotLocked(now).Sub(now)
}

// TestThrottleReserveSpacesRequests pins the spacing math: the first slot is
// immediate, and each subsequent reserve is spaced one interval after the
// previous slot (not after the call), so N requests spread across (N-1)
// intervals. synctest's fake clock makes the assertions exact.
func TestThrottleReserveSpacesRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := &throttle{interval: 100 * time.Millisecond}
		if got := th.reserve(); got != 0 {
			t.Errorf("first reserve wait = %v, want 0", got)
		}
		if got := th.reserve(); got != 100*time.Millisecond {
			t.Errorf("second reserve wait = %v, want 100ms", got)
		}
		if got := th.reserve(); got != 200*time.Millisecond {
			t.Errorf("third reserve wait = %v, want 200ms", got)
		}
	})
}

// TestThrottlePenalizeNeverShortensSchedule pins penalize's monotonicity: a
// penalty pushes the next slot out, and a later smaller penalty can never pull
// an already-scheduled slot back in (a 429 backoff must not be cancelled by a
// subsequent low-budget hint with a nearer reset).
func TestThrottlePenalizeNeverShortensSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := &throttle{interval: time.Millisecond}
		th.penalize(500 * time.Millisecond)
		th.penalize(time.Millisecond) // smaller penalty must not shorten the schedule
		if got := th.reserve(); got != 500*time.Millisecond {
			t.Errorf("reserve after penalties = %v, want 500ms", got)
		}
	})
}

// TestThrottleWaitRevalidatesReservationAfterPenalty pins the penalty-epoch
// revalidation: a waiter already holding a reserved slot when penalize fires
// must NOT wake and issue its request inside the penalty window on the stale
// pre-penalty slot - it re-reserves at the end of the penalized schedule, and
// a subsequent reservation stays interval-spaced behind it.
func TestThrottleWaitRevalidatesReservationAfterPenalty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		th := &throttle{interval: time.Second}
		start := time.Now()
		if err := th.wait(t.Context()); err != nil {
			t.Fatalf("first wait: %v", err)
		}
		// Second waiter holds a reservation one interval out (start+1s).
		done := make(chan time.Time, 1)
		go func() {
			if err := th.wait(t.Context()); err != nil {
				t.Errorf("penalized wait: %v", err)
			}
			done <- time.Now()
		}()
		synctest.Wait() // the waiter has reserved its slot and is sleeping
		// A 429 penalty lands before the outstanding slot matures.
		th.penalize(5 * time.Second)
		woke := <-done
		if got := woke.Sub(start); got != 5*time.Second {
			t.Errorf("penalized waiter proceeded after %v, want exactly the 5s penalty epoch", got)
		}
		// The re-reserved slot consumed start+5s, so the next reservation is
		// interval-spaced behind it at start+6s.
		if got := th.reserve(); got != time.Second {
			t.Errorf("reserve after penalized waiter = %v, want one interval (1s)", got)
		}
	})
}

// TestNewClientCoercesNonPositiveRate pins the documented constructor
// contract that rate values <= 0 are treated as 1 request per minute, so a
// zero rate cannot divide by zero and a negative rate cannot disable the
// throttle spacing.
func TestNewClientCoercesNonPositiveRate(t *testing.T) {
	tests := []struct {
		name string
		rate int
	}{
		{name: "zero rate", rate: 0},
		{name: "negative rate", rate: -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := NewClient(http.DefaultClient, "https://example.invalid/graphql", WithRate(tt.rate))
				if got := c.throttle.reserve(); got != 0 {
					t.Errorf("first reserve wait = %v, want 0", got)
				}
				if got := c.throttle.reserve(); got != time.Minute {
					t.Errorf("second reserve wait = %v, want %v (rate coerced to 1/min)", got, time.Minute)
				}
			})
		})
	}
}

func TestObserveRateHeadersMissingResetDefaultsToMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(http.DefaultClient, "https://example.invalid/graphql", WithRate(30))
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("X-RateLimit-Remaining", "1")

		client.observeRateHeaders(resp)

		if wait := client.throttle.reserve(); wait != time.Minute {
			t.Errorf("low-budget wait with no reset header = %v, want exactly the %v default", wait, time.Minute)
		}
		if got := client.Stats().RateLimitWaits; got != 1 {
			t.Errorf("Stats().RateLimitWaits = %d, want 1", got)
		}
	})
}

func TestObserveRateHeadersMalformedResetDefaultsToMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(http.DefaultClient, "https://example.invalid/graphql", WithRate(30))
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("X-RateLimit-Remaining", "0")
		resp.Header.Set("X-RateLimit-Reset", "not-a-timestamp")

		client.observeRateHeaders(resp)

		if wait := client.throttle.reserve(); wait != time.Minute {
			t.Errorf("low-budget wait with malformed reset = %v, want exactly the %v default", wait, time.Minute)
		}
	})
}

// TestSanitizeUpstreamMessage pins the log-forging boundary on untrusted
// upstream error messages: short clean text passes unchanged; C0/C1 controls,
// DEL, line/paragraph separators, and bidi override/isolate runes become
// spaces; and the 200-byte cap cuts on a rune boundary so the result stays
// valid UTF-8 even when the boundary lands inside a multibyte rune.
func TestSanitizeUpstreamMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short clean text unchanged", "Media not found.", "Media not found."},
		{"C0 newline and DEL cleaned", "line1\nline2\x7f", "line1 line2 "},
		{"C1 CSI and OSC cleaned", "a\u009bb\u009dc", "a b c"},
		{"line and paragraph separators cleaned", "a\u2028b\u2029c", "a b c"},
		{"bidi overrides and isolates cleaned", "a\u202eb\u2066c\u2069d", "a b c d"},
		{"bidi ALM LRM RLM marks cleaned", "a\u061cb\u200ec\u200fd", "a b c d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeUpstreamMessage(tt.in); got != tt.want {
				t.Errorf("sanitizeUpstreamMessage(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitizeUpstreamMessageRuneBoundaryCut pins the cap's UTF-8 safety: a
// clean message whose 200-byte retained-message boundary falls inside a
// multibyte rune is cut back to the rune start, stays valid UTF-8, and remains
// bounded by the 200-byte retained cap plus the three-byte "..." ellipsis
// (203 bytes total).
func TestSanitizeUpstreamMessageRuneBoundaryCut(t *testing.T) {
	// 199 ASCII bytes then a 3-byte rune: the 200-byte boundary lands inside it.
	in := strings.Repeat("a", 199) + "\u4e16\u754c"
	got := sanitizeUpstreamMessage(in)
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeUpstreamMessage() = %q is not valid UTF-8", got)
	}
	if want := strings.Repeat("a", 199) + "..."; got != want {
		t.Errorf("sanitizeUpstreamMessage() = %q, want the cut moved back to the rune start (%q)", got, want)
	}
	if len(got) > 200+len("...") {
		t.Errorf("len = %d, want bounded by 203", len(got))
	}
}

// TestObserveRateHeadersThresholdBoundary pins the lowRemaining gate on both
// sides: a remaining budget AT the threshold (2) backs off for the default
// minute window, while a budget just above it (3), a missing header, and a
// malformed header leave the throttle untouched.
func TestObserveRateHeadersThresholdBoundary(t *testing.T) {
	tests := []struct {
		name        string
		remaining   string
		wantBackoff bool
	}{
		{name: "at threshold backs off", remaining: "2", wantBackoff: true},
		{name: "just above threshold does not back off", remaining: "3", wantBackoff: false},
		{name: "missing header does not back off", remaining: "", wantBackoff: false},
		{name: "malformed header does not back off", remaining: "many", wantBackoff: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := NewClient(http.DefaultClient, "https://example.invalid/graphql", WithRate(100000))
				resp := &http.Response{Header: make(http.Header)}
				if tt.remaining != "" {
					resp.Header.Set("X-RateLimit-Remaining", tt.remaining)
				}
				client.observeRateHeaders(resp)
				wait := client.throttle.reserve()
				if tt.wantBackoff {
					if wait != time.Minute {
						t.Errorf("wait = %v, want exactly the %v default backoff at the lowRemaining threshold", wait, time.Minute)
					}
					if got := client.Stats().RateLimitWaits; got != 1 {
						t.Errorf("Stats().RateLimitWaits = %d, want 1", got)
					}
				} else {
					if wait != 0 {
						t.Errorf("wait = %v, want 0 (no backoff above the threshold)", wait)
					}
					if got := client.Stats().RateLimitWaits; got != 0 {
						t.Errorf("Stats().RateLimitWaits = %d, want 0", got)
					}
				}
			})
		})
	}
}

// TestBoundedMediaListUnmarshalTruncatedData pins the json.Unmarshaler
// contract of boundedMediaList against inputs the outer decoder never
// produces (encoding/json hands UnmarshalJSON syntax-valid values only, so
// these EOF branches are unreachable through parseMediaPage): a truncated
// value must error and leave the list unset, never a silent empty decode.
func TestBoundedMediaListUnmarshalTruncatedData(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty input", data: ""},
		{name: "unclosed array", data: "["},
		{name: "truncated element", data: `[{"id":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l boundedMediaList
			if err := l.UnmarshalJSON([]byte(tt.data)); err == nil {
				t.Fatal("UnmarshalJSON on truncated data = nil error, want error")
			}
			if l.set {
				t.Error("l.set = true after a failed decode, want unset")
			}
			if l.records != nil {
				t.Errorf("l.records = %v after a failed decode, want nil", l.records)
			}
		})
	}
}

// TestParseRejectsDuplicateJSONKeys pins the structural preflight: encoding/json
// applies the LAST occurrence of a duplicate object key and discards the earlier
// value unseen, so a single body carrying a valid Media plus a later null Media
// would otherwise reach classifyNullMedia as a genuine not-found and be
// negative-memoized (and a batch could have Page.media swapped for an empty
// array). Every ambiguous body must fail plainly — never ErrNotFound, never a
// usable result — so the id is retried next cycle. Key matching is
// case-insensitive because encoding/json matches struct fields that way too.
func TestParseRejectsDuplicateJSONKeys(t *testing.T) {
	single := map[string]string{
		"duplicate data ending in null Media":    `{"data":{"Media":{"id":1,"title":{"romaji":"A"}}},"data":{"Media":null}}`,
		"duplicate Media ending in null":         `{"data":{"Media":{"id":1,"title":{"romaji":"A"}},"Media":null}}`,
		"case-insensitive duplicate Media/media": `{"data":{"Media":{"id":1,"title":{"romaji":"A"}},"media":null}}`,
		"duplicate key inside title":             `{"data":{"Media":{"id":1,"title":{"romaji":"A","romaji":"B"}}}}`,
	}
	for name, raw := range single {
		t.Run("single/"+name, func(t *testing.T) {
			got, err := parseMediaForID([]byte(raw), 1)
			if err == nil {
				t.Fatalf("parseMediaForID(%s) = %+v, nil error; want ambiguous JSON rejected", raw, got)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want a plain retryable error (never ErrNotFound, which is negative-memoized)", err)
			}
		})
	}

	batch := map[string]string{
		"duplicate Page ending in null":   `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A"}}]},"Page":null}}`,
		"duplicate media ending in empty": `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A"}}],"media":[]}}}`,
		"duplicate data":                  `{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A"}}]}},"data":{"Page":{"media":[]}}}`,
	}
	for name, raw := range batch {
		t.Run("batch/"+name, func(t *testing.T) {
			got, err := parseMediaPage([]byte(raw))
			if err == nil {
				t.Fatalf("parseMediaPage(%s) = %v, nil error; want ambiguous JSON rejected", raw, got)
			}
			if len(got) != 0 {
				t.Errorf("got = %v, want no usable records from an ambiguous batch", got)
			}
		})
	}
}

// TestParseAcceptsRepeatedKeysAcrossSiblingObjects guards against the
// duplicate-key preflight over-rejecting: the same key name in DIFFERENT
// objects (each batch record has its own id/title) is normal, unambiguous JSON.
func TestParseAcceptsRepeatedKeysAcrossSiblingObjects(t *testing.T) {
	raw := []byte(`{"data":{"Page":{"media":[{"id":1,"title":{"romaji":"A"}},{"id":2,"title":{"romaji":"B"}}]}}}`)
	got, err := parseMediaPage(raw)
	if err != nil {
		t.Fatalf("parseMediaPage: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 records", len(got))
	}
}

// TestValidateResponseBoundsThePreflightWalk is the cross-library acceptance
// test for the structural preflight this package delegates to
// (jsoncap.Preflight): an all-opens body must be rejected by a depth ceiling
// rather than recursing once per byte of a 1 MiB '[' body, and a key-dense
// object must still validate (the library tracks per-object keys in a
// fold-canonicalized set, so the cost is O(keys) rather than a rescan of
// every prior key on an upstream-controlled key count).
//
// The ceiling is the JSON TOKENIZER's, not the library's, and that is the
// contract worth pinning here. Since Go 1.27 encoding/json is backed by
// encoding/json/v2 and json.Decoder.Token enforces jsontext's own
// 10000-container nesting limit - at exactly jsoncap.MaxDepth, and one call
// BELOW the library's depth check, which therefore never sees the token that
// would trip it. jsoncap.ErrMaxDepth is structurally unreachable through
// Preflight, so asserting on it would pin a sentinel that can no longer fire.
// What replaces it is stronger: the refusal arrives as encoding/json's own
// *json.SyntaxError, and Preflight's depth acceptance set is now exactly
// json.Unmarshal's, so the preflight cannot admit a body the decode step would
// reject on depth. Both boundary levels are pinned because that parity is the
// claim; the error TEXT deliberately is not, since jsontext exports no depth
// sentinel and documents its syntactic-error contents as unstable.
func TestValidateResponseBoundsThePreflightWalk(t *testing.T) {
	// A WELL-FORMED body one level over the ceiling, so the refusal is depth and
	// nothing else - an all-opens body is also truncated, which muddies the
	// diagnosis.
	overDeep := []byte(strings.Repeat("[", jsoncap.MaxDepth+1) + strings.Repeat("]", jsoncap.MaxDepth+1))
	var syntaxErr *json.SyntaxError
	if err := validateResponse(overDeep); !errors.As(err, &syntaxErr) {
		t.Fatalf("validateResponse(%d nested arrays) = %v (%T), want the tokenizer's *json.SyntaxError", jsoncap.MaxDepth+1, err, err)
	}
	var sink any
	if json.Unmarshal(overDeep, &sink) == nil {
		t.Errorf("json.Unmarshal accepted %d nested arrays; the preflight is now stricter on depth than the decoder it guards", jsoncap.MaxDepth+1)
	}

	// At the ceiling exactly both accept, so the preflight adds no depth
	// strictness of its own.
	atCeiling := []byte(strings.Repeat("[", jsoncap.MaxDepth) + strings.Repeat("]", jsoncap.MaxDepth))
	if err := validateResponse(atCeiling); err != nil {
		t.Errorf("validateResponse(%d nested arrays) = %v, want it accepted (json.Unmarshal accepts it)", jsoncap.MaxDepth, err)
	}

	// The original hostile shape: 1 MiB of nothing but '['. Still refused, and
	// refused by the ceiling rather than by walking the body, which is what keeps
	// the cost bounded by MaxDepth instead of by the input length.
	if err := validateResponse([]byte(strings.Repeat("[", 1<<20))); err == nil {
		t.Error("validateResponse(1 MiB of open brackets) = nil, want it rejected")
	}

	var wide strings.Builder
	wide.WriteByte('{')
	for i := range 20000 {
		if i > 0 {
			wide.WriteByte(',')
		}
		wide.WriteString(`"k`)
		wide.WriteString(strconv.Itoa(i))
		wide.WriteString(`":0`)
	}
	wide.WriteByte('}')
	if err := validateResponse([]byte(wide.String())); err != nil {
		t.Errorf("validateResponse(key-dense object) = %v, want it accepted", err)
	}
}

// TestParseMediaRejectsUnknownFormatAsTypeEvidence pins l-f12: the format is
// the only arr-routing evidence the AniList fallback carries, and
// match.formatArr routes it by EXCLUSION (MOVIE to Radarr, everything else to
// Sonarr). An unrecognized non-empty token therefore did not read as "unknown",
// it read as "not a movie" - so a garbled or hostile value supplied false Sonarr
// evidence for an unmapped entry, removed the Radarr candidate a title+year
// match would have left ambiguous, and persisted the wrong match in state.json
// for the memo's life. An unknown token must collapse to "", which every
// consumer already reads as type-unknown. The record itself stays usable: only
// the TYPE claim is discarded, never the titles.
func TestParseMediaRejectsUnknownFormatAsTypeEvidence(t *testing.T) {
	tests := map[string]struct {
		wire string
		want string
	}{
		"a real format is preserved verbatim":    {wire: "MOVIE", want: "MOVIE"},
		"lowercase real format is canonicalized": {wire: "movie", want: "MOVIE"},
		"TV_SHORT is a real AniList format":      {wire: "TV_SHORT", want: "TV_SHORT"},
		"an invented token is discarded":         {wire: "NOT_A_FORMAT", want: ""},
		"a plausible-but-wrong token is dropped": {wire: "FILM", want: ""},
		"an empty format stays empty":            {wire: "", want: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"data":{"Media":{"format":"` + tc.wire + `","title":{"romaji":"A Show"}}}}`)
			got, err := parseMedia(body)
			if err != nil {
				t.Fatalf("parseMedia = %v, want the record accepted (only its type claim is at stake)", err)
			}
			if got.Format != tc.want {
				t.Errorf("Media.Format = %q, want %q", got.Format, tc.want)
			}
			if len(got.Titles) == 0 {
				t.Error("titles were dropped; an unknown format must not cost the record its usable titles")
			}
		})
	}
}

// TestParseMediaRejectionsWrapErrRecordUnusable pins the PRODUCER half of the
// permanent-vs-transient classification contract. Every toMedia rejection is a
// function of the record's own content, so match.lookup memoizes it negatively
// and resets the degradation streak (errors.Is(err, anilist.ErrRecordUnusable));
// a rejection that surfaced as a plain error would instead be re-fetched every
// cycle forever, keep Result.Degraded true, and escalate to a standing ERROR
// whose remediation text points at graphql.anilist.co reachability that is
// healthy. The consumer's own test constructs the sentinel by hand, so nothing
// else in the tree fails if this package stops wrapping it.
func TestParseMediaRejectionsWrapErrRecordUnusable(t *testing.T) {
	tests := map[string]string{
		"over-limit title":             `{"data":{"Media":{"format":"TV","title":{"romaji":"` + strings.Repeat("a", maxTitleBytes+1) + `"}}}}`,
		"every title unsafe":           `{"data":{"Media":{"format":"TV","title":{"romaji":"A\nB","english":"C\u009bD"}}}}`,
		"no usable title":              `{"data":{"Media":{"format":"TV","title":{"romaji":" ","english":""}}}}`,
		"no matchable title key":       `{"data":{"Media":{"format":"TV","title":{"romaji":"!!!"}}}}`,
		"native-script-only title set": `{"data":{"Media":{"format":"TV","title":{"native":"\u4e16\u754c"}}}}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseMedia([]byte(raw))
			if !errors.Is(err, ErrRecordUnusable) {
				t.Fatalf("parseMedia = %v, want ErrRecordUnusable (a plain error re-fetches the record every cycle forever)", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("parseMedia = %v, must not also classify as ErrNotFound", err)
			}
		})
	}
}

// TestGqlErrorsBoundsEnvelopeCardinality pins the untrusted errors[] bound
// documented on maxEnvelopeErrors: the 1 MiB body cap alone admits ~350k empty
// objects, so this cap is what stops json.Unmarshal expanding them into
// []gqlError before any consumer reads errs[0] (CWE-400, the amplification
// boundedMediaList exists to close on the media array). An at-cap array must
// still decode fully, an over-cap array must fail with the library's
// ErrArrayCap and leave the list nil.
func TestGqlErrorsBoundsEnvelopeCardinality(t *testing.T) {
	array := func(n int) []byte {
		return []byte("[" + strings.TrimSuffix(strings.Repeat(`{"message":"e","status":500},`, n), ",") + "]")
	}

	var atCap gqlErrors
	if err := atCap.UnmarshalJSON(array(maxEnvelopeErrors)); err != nil {
		t.Fatalf("UnmarshalJSON at the cap = %v, want %d errors accepted", err, maxEnvelopeErrors)
	}
	if len(atCap) != maxEnvelopeErrors {
		t.Errorf("len = %d, want %d", len(atCap), maxEnvelopeErrors)
	}

	var over gqlErrors
	err := over.UnmarshalJSON(array(maxEnvelopeErrors + 1))
	if !errors.Is(err, jsoncap.ErrArrayCap) {
		t.Fatalf("UnmarshalJSON over the cap = %v, want jsoncap.ErrArrayCap", err)
	}
	if over != nil {
		t.Errorf("list = %v after a failed decode, want nil", over)
	}
}

// TestRetainRequestedReportsUnsolicitedMagnitude pins the magnitude signal on
// the identity-set violation, the sibling of the one
// TestParseMediaPageDuplicateIDExcluded already pins for parsePageRecords ("3 of
// 4 records rejected"). retainRequested names only the FIRST unsolicited id, so
// without the count an operator cannot tell one stray id apart from an upstream
// that answered with a wholesale foreign identity set - and the existing
// coverage exercises the count without asserting it, so deleting the branch
// leaves the suite green.
func TestRetainRequestedReportsUnsolicitedMagnitude(t *testing.T) {
	chunk := []int{1, 2}

	single := map[int]Media{1: {Titles: []string{"t1"}}, 99: {Titles: []string{"injected"}}}
	err := retainRequested(single, chunk)
	if err == nil {
		t.Fatal("retainRequested with one unsolicited id = nil error, want a record-local error")
	}
	if !strings.Contains(err.Error(), "unexpected media id 99") {
		t.Errorf("error = %q, want the offending id named", err)
	}
	if strings.Contains(err.Error(), "unsolicited ids dropped") {
		t.Errorf("error = %q, want no magnitude suffix for a single stray id", err)
	}

	many := map[int]Media{1: {Titles: []string{"t1"}}, 97: {}, 98: {}, 99: {}}
	err = retainRequested(many, chunk)
	if err == nil {
		t.Fatal("retainRequested with three unsolicited ids = nil error, want a record-local error")
	}
	if want := "(3 unsolicited ids dropped)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want magnitude %q so a wholesale identity-set violation reads differently from one stray id", err, want)
	}
	if len(many) != 1 {
		t.Errorf("page retained %d records, want only the requested id 1", len(many))
	}
}

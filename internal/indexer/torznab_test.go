package indexer

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestRenderFeed_usesStableGUIDFallback pins the documented GUID fallback
// order (explicit GUID -> info hash -> download URL) independently of the
// production guid() helper, which the round-trip fuzz oracle also calls.
func TestRenderFeed_usesStableGUIDFallback(t *testing.T) {
	const hash = "143ed15e5e3df072ae91adaeb149973a887590dd"
	tests := map[string]struct {
		want string
		item item
	}{
		"explicit GUID wins": {
			item: item{GUID: "explicit", InfoHash: hash, DownloadURL: "https://prowlarr.test/download/1"},
			want: "explicit",
		},
		"info hash is the first fallback": {
			item: item{InfoHash: hash, DownloadURL: "https://prowlarr.test/download/1"},
			want: hash,
		},
		"download URL is the final fallback": {
			item: item{DownloadURL: "https://prowlarr.test/download/1"},
			want: "https://prowlarr.test/download/1",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			parsed, err := parseTorznab([]byte(renderFeed([]item{tc.item})))
			if err != nil {
				t.Fatalf("parseTorznab(renderFeed(item)): %v", err)
			}
			if len(parsed) != 1 {
				t.Fatalf("parsed item count = %d, want 1", len(parsed))
			}
			if got := parsed[0].GUID; got != tc.want {
				t.Errorf("rendered GUID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteItemSaturatesPeerCount pins writeItem's overflow guard: attrInt
// accepts counts through math.MaxInt, so a malformed-but-valid upstream item
// with seeders and leechers both at math.MaxInt must render a peers attr
// saturated at math.MaxInt - never a wrapped negative value, which would
// contradict toItem's non-negative normalization.
func TestWriteItemSaturatesPeerCount(t *testing.T) {
	tests := map[string]struct {
		wantPeers         int
		seeders, leechers int
	}{
		"both at MaxInt saturate":        {seeders: math.MaxInt, leechers: math.MaxInt, wantPeers: math.MaxInt},
		"sum just over MaxInt saturates": {seeders: math.MaxInt - 1, leechers: 2, wantPeers: math.MaxInt},
		"ordinary counts sum exactly":    {seeders: 146, leechers: 3, wantPeers: 149},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			it := item{Title: "x", Seeders: tc.seeders, Leechers: tc.leechers}
			writeItem(&b, &it)
			out := b.String()
			want := `<torznab:attr name="peers" value="` + strconv.Itoa(tc.wantPeers) + `"/>`
			if !strings.Contains(out, want) {
				t.Errorf("rendered item missing %s:\n%s", want, out)
			}
			if strings.Contains(out, `value="-`) {
				t.Errorf("rendered a negative attribute value:\n%s", out)
			}
		})
	}
}

// TestUpstreamErrorDocMessageNamesCodeAndDescription pins the operator-facing
// message of the Torznab <error>-document failure: the error string carries
// both the upstream code and its description, since it is what fetchRaw's
// "upstream query failed" WARN renders for the operator diagnosing a bad
// Prowlarr credential or a named indexer failure.
func TestUpstreamErrorDocMessageNamesCodeAndDescription(t *testing.T) {
	_, err := parseTorznab([]byte(`<?xml version="1.0"?><error code="100" description="Incorrect user credentials"/>`))
	if err == nil {
		t.Fatal("parseTorznab on an <error> document returned nil error")
	}
	var doc *upstreamDocError
	if !errors.As(err, &doc) {
		t.Fatalf("error = %T, want *upstreamDocError", err)
	}
	got := err.Error()
	if !strings.Contains(got, "code=100") || !strings.Contains(got, "Incorrect user credentials") {
		t.Errorf("Error() = %q, want it to carry the upstream code and description", got)
	}
}

// TestSanitizeUpstreamText_cleansAndBounds pins the emit-boundary policy on
// untrusted Torznab <error> text: control characters (a newline that could
// spoof a level=ERROR log line) are replaced with spaces, and the output is
// capped at exactly 200 bytes plus the "..." truncation marker, so a multi-MB
// <error> body can never flood a log line.
func TestSanitizeUpstreamText_cleansAndBounds(t *testing.T) {
	if got, want := sanitizeUpstreamText("ok\nlevel=ERROR fake"), "ok level=ERROR fake"; got != want {
		t.Errorf("sanitizeUpstreamText(control text) = %q, want %q", got, want)
	}
	input := strings.Repeat("x", 201)
	want := strings.Repeat("x", 200) + "..."
	if got := sanitizeUpstreamText(input); got != want {
		t.Errorf("sanitizeUpstreamText(201 bytes) = %q, want %q", got, want)
	}
}

// TestParseTorznabDecodeLimits pins the fail-closed decode limits on an
// untrusted upstream response: the transport byte cap alone cannot stop a
// compromised Prowlarr from packing millions of tiny item/attr elements (or
// one escape-heavy multi-megabyte field) into the budget, so the parser must
// reject cardinality, per-field, and cumulative-text overflows with a
// torznabLimitError - which fetchAndParse then retries as a malformed body -
// while responses at or under the limits keep parsing.
func TestParseTorznabDecodeLimits(t *testing.T) {
	feedOf := func(inner string) []byte {
		return []byte(`<?xml version="1.0"?><rss><channel>` + inner + `</channel></rss>`)
	}
	// An escape-heavy field: every source byte is a 5-byte entity, so the
	// wire form is ~5x the decoded length the limit is measured against.
	escapeHeavy := strings.Repeat("&amp;", maxUpstreamFieldBytes+1)

	tests := map[string]struct {
		inner    string
		wantErr  bool
		wantItem int
	}{
		"item count at the cap parses": {
			inner:    strings.Repeat("<item><title>x</title></item>", maxUpstreamItems),
			wantItem: maxUpstreamItems,
		},
		"item count over the cap rejected": {
			inner:   strings.Repeat("<item/>", maxUpstreamItems+1),
			wantErr: true,
		},
		"attr count over the cap rejected": {
			inner:   "<item>" + strings.Repeat(`<torznab:attr name="a" value="b"/>`, maxUpstreamAttrs+1) + "</item>",
			wantErr: true,
		},
		"tiny attr flood rejected": {
			// A body-sized run of tiny <attr/> elements: the per-item cap
			// must reject DURING decoding (itemXML.decodeChild refuses the
			// 65th attr before decoding it), so the flood never materializes
			// an attr slice proportional to the wire bytes.
			inner:   "<item>" + strings.Repeat(`<torznab:attr name="a" value="b"/>`, 100000) + "</item>",
			wantErr: true,
		},
		"escape-heavy field over the cap rejected": {
			inner:   "<item><title>" + escapeHeavy + "</title></item>",
			wantErr: true,
		},
		"attr value over the cap rejected": {
			inner:   `<item><torznab:attr name="a" value="` + strings.Repeat("v", maxUpstreamFieldBytes+1) + `"/></item>`,
			wantErr: true,
		},
		"attr name over the cap rejected": {
			inner:   `<item><torznab:attr name="` + strings.Repeat("n", maxUpstreamFieldBytes+1) + `" value="b"/></item>`,
			wantErr: true,
		},
		"repeated fields in one item over the budget rejected": {
			// decodeField accounts EVERY occurrence of a repeated element, so
			// 1025 x 4096-byte titles in ONE item cross the 4 MiB response budget
			// even though each field and the item count stay under their own caps.
			inner: "<item>" + strings.Repeat(
				"<title>"+strings.Repeat("t", maxUpstreamFieldBytes)+"</title>",
				maxUpstreamTextBytes/maxUpstreamFieldBytes+1) + "</item>",
			wantErr: true,
		},
		"cumulative text over the budget rejected": {
			// Each item stays under the per-field cap and the item count
			// stays under maxUpstreamItems (513 < 1000), so ONLY the
			// cumulative budget can reject: 513 items x (title+guid) 8192
			// bytes = 4,202,496 > maxUpstreamTextBytes.
			inner: strings.Repeat(
				"<item><title>"+strings.Repeat("t", maxUpstreamFieldBytes)+"</title>"+
					"<guid>"+strings.Repeat("g", maxUpstreamFieldBytes)+"</guid></item>",
				maxUpstreamTextBytes/(2*maxUpstreamFieldBytes)+1),
			wantErr: true,
		},
		"cumulative text across two channels rejected": {
			// Each <channel> stays individually under the budget (~2 MiB of
			// decoded text) but their aggregate crosses it. encoding/xml
			// re-invokes channelXML.UnmarshalXML on the same value for each
			// sibling, so the budget must persist across invocations with
			// the accumulated Items - a per-invocation budget would accept
			// this response.
			inner: strings.Repeat(
				"<item><title>"+strings.Repeat("t", maxUpstreamFieldBytes)+"</title>"+
					"<guid>"+strings.Repeat("g", maxUpstreamFieldBytes)+"</guid></item>",
				257) +
				"</channel><channel>" +
				strings.Repeat(
					"<item><title>"+strings.Repeat("t", maxUpstreamFieldBytes)+"</title>"+
						"<guid>"+strings.Repeat("g", maxUpstreamFieldBytes)+"</guid></item>",
					257),
			wantErr: true,
		},
		"maximum-length field parses": {
			inner:    "<item><title>" + strings.Repeat("t", maxUpstreamFieldBytes) + "</title></item>",
			wantItem: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			items, err := parseTorznab(feedOf(tc.inner))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTorznab accepted an over-limit response (%d items)", len(items))
				}
				var limitErr *torznabLimitError
				if !errors.As(err, &limitErr) {
					t.Errorf("error = %T (%v), want *torznabLimitError", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTorznab: %v", err)
			}
			if len(items) != tc.wantItem {
				t.Errorf("parsed item count = %d, want %d", len(items), tc.wantItem)
			}
		})
	}
}

// TestTorznabLimitErrorMessageNamesLimit pins the operator-facing message of a
// decode-limit rejection (what fetchAndParse's retry logging + the harvest WARN
// render), so it must name the decode limit that fired.
func TestTorznabLimitErrorMessageNamesLimit(t *testing.T) {
	_, err := parseTorznab([]byte(`<?xml version="1.0"?><rss><channel>` +
		strings.Repeat("<item/>", maxUpstreamItems+1) + `</channel></rss>`))
	if err == nil {
		t.Fatal("parseTorznab accepted an over-limit response")
	}
	got := err.Error()
	if !strings.Contains(got, "torznab response exceeds decode limit") || !strings.Contains(got, "more than 1000 items") {
		t.Errorf("Error() = %q, want it to name the decode limit that fired", got)
	}
}

// TestParseTorznabRejectsTruncatedResponses pins error propagation at every
// decode nesting level of the hand-rolled UnmarshalXML loops.
func TestParseTorznabRejectsTruncatedResponses(t *testing.T) {
	tests := map[string]string{
		"EOF inside channel":        `<?xml version="1.0"?><rss><channel>`,
		"EOF inside item":           `<?xml version="1.0"?><rss><channel><item>`,
		"EOF after complete child":  `<?xml version="1.0"?><rss><channel><item><title>x</title>`,
		"EOF inside open enclosure": `<?xml version="1.0"?><rss><channel><item><enclosure url="http://x/1">`,
		"EOF inside open attr":      `<?xml version="1.0"?><rss><channel><item><torznab:attr name="seeders">`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			items, err := parseTorznab([]byte(body))
			if err == nil {
				t.Errorf("parseTorznab accepted a truncated response (%d items); partial data must fail so the fetch retries", len(items))
			}
		})
	}
}

// TestParseTorznabSkipsUnknownItemChildren pins the default d.Skip() arm of
// itemXML.decodeChild: real Prowlarr responses carry item children the feed
// does not consume, and the parser must skip them whole.
func TestParseTorznabSkipsUnknownItemChildren(t *testing.T) {
	body := `<?xml version="1.0"?><rss><channel><item>` +
		`<title>x</title>` +
		`<description>rendered by Prowlarr, ignored by the feed</description>` +
		`<jackettindexer id="1">Nyaa</jackettindexer>` +
		`<guid>https://nyaa.si/view/1</guid>` +
		`</item></channel></rss>`
	items, err := parseTorznab([]byte(body))
	if err != nil {
		t.Fatalf("parseTorznab with unknown item children: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("parsed item count = %d, want 1", len(items))
	}
	if items[0].Title != "x" || items[0].GUID != "https://nyaa.si/view/1" {
		t.Errorf("recognized fields around unknown children = %q/%q, want x/https://nyaa.si/view/1", items[0].Title, items[0].GUID)
	}
}

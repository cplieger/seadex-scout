package indexer

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/runesafe"
)

// Torznab category ids. SeaDex is anime, so series map to the Anime subcategory
// (5070) under TV (5000) and movies to Movies (2000) - matching the Nyaa and
// AnimeBytes indexer definitions the feed proxies.
const (
	catTV     = 5000
	catAnime  = 5070
	catMovies = 2000
)

// torznabNS is the Torznab feed namespace the arrs key their attr parsing on.
const torznabNS = "http://torznab.com/schemas/2015/feed"

// errCodeIncorrectCredentials is the Newznab/Torznab error code for missing or
// incorrect credentials (100), the closest fit for a required-but-unset secret.
// Prowlarr surfaces the <error> element's description on the indexer's test, so
// the operator sees why the save failed.
const errCodeIncorrectCredentials = 100

// errCodeUnknown is the Newznab/Torznab "unknown error" code (900), used for an
// unexpected internal failure such as a recovered handler panic.
const errCodeUnknown = 900

// item is one Torznab feed release in the WIRE vocabulary - the real fields
// parsed from a Prowlarr Torznab result, plus the SeaDex
// download-volume-factor marker this feed adds; writeItem renders exactly
// these fields back out. It carries no journal bookkeeping: the persisted
// RSS journal wraps this type as journalItem (journal.go), so a change to
// the volatile upstream parse shape and a change to the on-disk snapshot
// contract can never silently be the same edit. The json tags pin the
// persisted feed.json snapshot contract (journalItem embeds item, and
// encoding/json flattens the embed, so these names ARE the historical
// on-disk field names an upgraded binary must keep reading).
type item struct {
	PubDate              time.Time `json:"PubDate"`
	Title                string    `json:"Title"`
	GUID                 string    `json:"GUID"`
	InfoURL              string    `json:"InfoURL"`
	DownloadURL          string    `json:"DownloadURL"`
	InfoHash             string    `json:"InfoHash"`
	DownloadVolumeFactor string    `json:"DownloadVolumeFactor"`
	Categories           []int     `json:"Categories"`
	Size                 int64     `json:"Size"`
	Seeders              int       `json:"Seeders"`
	Leechers             int       `json:"Leechers"`
}

// guid returns a stable unique id for the item.
func (it *item) guid() string {
	switch {
	case it.GUID != "":
		return it.GUID
	case it.InfoHash != "":
		return it.InfoHash
	default:
		return it.DownloadURL
	}
}

// renderCaps returns the t=caps response. The categories and search modes match
// the Nyaa + AnimeBytes indexer definitions this feed proxies (q-based search
// with season/ep for TV; no id search, since neither tracker supports it), so
// the arrs query the feed exactly as they would those indexers.
func renderCaps() string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<caps>")
	b.WriteString(`<server title="seadex-scout"/>`)
	fmt.Fprintf(&b, `<limits max="%d" default="%d"/>`, maxItems, defaultCapsLimit)
	b.WriteString("<searching>")
	b.WriteString(`<search available="yes" supportedParams="q"/>`)
	b.WriteString(`<tv-search available="yes" supportedParams="q,season,ep"/>`)
	b.WriteString(`<movie-search available="yes" supportedParams="q"/>`)
	b.WriteString("</searching>")
	b.WriteString("<categories>")
	fmt.Fprintf(&b, `<category id="%d" name="TV"><subcat id="%d" name="Anime"/></category>`, catTV, catAnime)
	fmt.Fprintf(&b, `<category id="%d" name="Movies"/>`, catMovies)
	b.WriteString("</categories>")
	b.WriteString("</caps>")
	return b.String()
}

// renderError returns a Newznab/Torznab <error> document. The arrs and Prowlarr
// treat a response carrying this element as a failed request and show the
// description, so it is how the feed reports a misconfiguration (a required
// secret not set) on the indexer's save-test rather than returning empty.
func renderError(code int, description string) string {
	var b strings.Builder
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, `<error code="%d" description="%s"/>`, code, esc(description))
	return b.String()
}

// renderFeed returns the Torznab RSS feed for items. It is written by hand so
// the `torznab:` prefixed attribute elements come out exactly as the arrs
// expect, without the namespace rewriting encoding/xml would apply on output.
func renderFeed(items []item) string {
	var b strings.Builder
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, `<rss version="2.0" xmlns:torznab="%s">`, torznabNS)
	b.WriteString("<channel>")
	b.WriteString("<title>seadex-scout</title>")
	for i := range items {
		if b.Len() > maxRenderedFeedBytes {
			// A pathological feed (every field at maxPersistedFieldBytes,
			// escape-amplified ~5x) must degrade to a truncated-but-valid
			// document instead of OOMing the container; a realistic feed
			// never comes near the budget.
			break
		}
		writeItem(&b, &items[i])
	}
	b.WriteString("</channel></rss>")
	return b.String()
}

// writeItem renders one release as an <item>: its title, size, seeders, and
// download URL (Prowlarr's proxy link for a search, a directly-built tracker
// link for a synthesized RSS item), plus the SeaDex marker. The enclosure is
// omitted when there is no download URL, so a link-less item never renders an
// empty enclosure. Seeders are floored to 1 (never 0, so the arrs' minimum-
// seeders check cannot reject a curated release when the swarm count is
// momentarily 0/unknown or synthesized).
func writeItem(b *strings.Builder, it *item) {
	b.WriteString("<item>")
	writeText(b, "title", it.Title)
	writeText(b, "guid", it.guid())
	if it.InfoURL != "" {
		writeText(b, "comments", it.InfoURL)
	}
	if !it.PubDate.IsZero() {
		writeText(b, "pubDate", it.PubDate.UTC().Format(time.RFC1123Z))
	}
	// Clamp like the peer counts below: toItem and totalSize normalize at
	// their own ingresses, and validPersistedItem rejects persisted negative
	// size/peer counts, but it does not validate category positivity, and
	// search-path items rendered directly never pass the persistence gate -
	// so an unclamped value here could still render a negative enclosure
	// length/size attr or a non-positive category id, contradicting toItem's
	// normalization.
	size := max(it.Size, 0)
	if it.DownloadURL != "" {
		b.WriteString(`<enclosure url="`)
		escTo(b, it.DownloadURL)
		fmt.Fprintf(b, `" length="%d" type="application/x-bittorrent"/>`, size)
	}

	cats := it.Categories
	if len(cats) == 0 {
		cats = []int{catAnime}
	}
	for _, c := range cats {
		if c <= 0 {
			continue
		}
		writeAttr(b, "category", strconv.Itoa(c))
	}
	writeAttr(b, "size", strconv.FormatInt(size, 10))
	if it.InfoHash != "" {
		writeAttr(b, "infohash", it.InfoHash)
	}
	// The marker: best -> downloadvolumefactor 0.75 (Freeleech25), alt -> 0.25
	// (Freeleech75). uploadvolumefactor 1 keeps it from also flagging DoubleUpload.
	// Search results and synthesized RSS items that were matched to SeaDex carry
	// this marker. When DownloadVolumeFactor is empty, omit both factor attrs so
	// the arr treats the item as normal (factor 1).
	if it.DownloadVolumeFactor != "" {
		writeAttr(b, "downloadvolumefactor", it.DownloadVolumeFactor)
		writeAttr(b, "uploadvolumefactor", "1")
	}

	seeders := max(it.Seeders, 1)
	leechers := max(it.Leechers, 0)
	// Saturate instead of wrapping: attrInt accepts counts through
	// math.MaxInt, so a malformed-but-valid upstream item with huge counts
	// would otherwise overflow seeders+leechers negative and render an
	// invalid negative peers attr, contradicting toItem's non-negative
	// normalization.
	peers := seeders + min(leechers, math.MaxInt-seeders)
	writeAttr(b, "seeders", strconv.Itoa(seeders))
	writeAttr(b, "peers", strconv.Itoa(peers))
	b.WriteString("</item>")
}

// writeText writes a simple escaped <tag>value</tag> element.
func writeText(b *strings.Builder, tag, value string) {
	b.WriteString("<" + tag + ">")
	escTo(b, value)
	b.WriteString("</" + tag + ">")
}

// writeAttr writes a <torznab:attr name=".." value=".."/> element.
func writeAttr(b *strings.Builder, name, value string) {
	b.WriteString(`<torznab:attr name="`)
	escTo(b, name)
	b.WriteString(`" value="`)
	escTo(b, value)
	b.WriteString(`"/>`)
}

// escTo escapes s for use in XML text or attribute values, writing directly
// into b. Escaping in place keeps renderFeed from holding a second escaped
// copy of every field beside the document builder: XML escaping can expand an
// ampersand-heavy value ~5x, and the temporary copies esc-per-field rendering
// retained were one leg of the snapshot memory-amplification path (the other
// is the shared persisted-item limits in writer.go).
//
// The escape composes the shared rune policy: xml.EscapeText
// covers XML's own metacharacters and the C0 controls, but passes C1
// controls (U+0080-U+009F), Unicode bidi controls, and U+2028/U+2029 through
// RAW - and every text value on this feed is upstream-controlled (tracker
// titles via Prowlarr, SeaDex file names synthesized into titles), consumed
// by arr web UIs and operator terminals. Sanitizing at the one emit boundary
// (the runesafe adoption rule) keeps the raw bytes intact everywhere they
// are computed on - the persisted snapshot, matching, dedupe keys - while no
// rendered document can carry the unsafe classes, wherever the value came
// from (a live search passthrough, the persisted journal, or a legacy
// snapshot written before this policy).
func escTo(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(runesafe.Sanitize(s)))
}

// esc escapes a string for use in XML text or attribute values, returning it
// as a new string. Rendering paths that already own a strings.Builder should
// prefer escTo; esc remains for call sites that interpolate (renderError).
func esc(s string) string {
	var b strings.Builder
	escTo(&b, s)
	return b.String()
}

// --- Parsing Prowlarr's per-indexer Torznab responses ---

// feedXML / channelXML / itemXML / attrXML mirror the Torznab RSS a Prowlarr
// indexer endpoint returns. `xml:"attr"` matches the torznab:attr elements by
// local name regardless of the declared namespace prefix.
type feedXML struct {
	XMLName xml.Name   `xml:"rss"`
	Channel channelXML `xml:"channel"`
}

type channelXML struct {
	Items []itemXML `xml:"item"`
	// textBytes is the cumulative decoded text across every <channel>
	// occurrence in the response. It lives on the struct (not a per-
	// UnmarshalXML local) because encoding/xml re-invokes UnmarshalXML on
	// the same channelXML value for each <channel> sibling, accumulating
	// Items across invocations - a per-call budget would reset while the
	// retained items kept growing.
	textBytes int
}

// Decode limits on an untrusted upstream Torznab response. The transport cap
// (prowlarr.go's upstreamMaxBytes) bounds wire bytes only: a compromised
// Prowlarr could pack millions of tiny item/attr elements into that byte
// budget, or one multi-megabyte field, amplifying allocations in the decoded
// object graph and again in renderFeed (CWE-400). These constants bound the
// decoded representation independently; any overflow fails the whole parse
// closed with a torznabLimitError, which fetchAndParse wraps as a transient
// malformed-body failure inside the existing bounded retry budget.
const (
	// maxUpstreamItems caps item elements per response. It reuses the render
	// cap (query.go's maxItems): the served feed never renders more than
	// maxItems items, so accepting more from one upstream has no value. Real
	// responses are far smaller (a live AB series search returns ~145).
	maxUpstreamItems = maxItems
	// maxUpstreamAttrs caps torznab:attr elements per item. Prowlarr emits
	// roughly a dozen (size, seeders, categories, flags); 64 is generous.
	maxUpstreamAttrs = 64
	// maxUpstreamFieldBytes caps each decoded text field (title, guid,
	// comments, link, pubDate, enclosure URL, attr name/value). Real titles
	// and Prowlarr proxy URLs are well under 1 KiB.
	maxUpstreamFieldBytes = 4096
	// maxUpstreamTextBytes caps the cumulative decoded text across all items
	// in one response, bounding total retained memory even when every field
	// stays under its individual cap.
	maxUpstreamTextBytes = 4 << 20
)

// maxRenderedFeedBytes bounds one rendered feed document, the render-side
// twin of maxUpstreamTextBytes: the search path is aggregate-bounded at
// decode, but the persisted journal path is bounded only per field and per
// snapshot, and XML escaping can expand an ampersand-heavy field ~5x.
// Overshoot past the check is at most one item (~120 KiB).
const maxRenderedFeedBytes = 8 << 20

// torznabLimitError is parseTorznab's fail-closed error for a syntactically
// valid response that exceeds the decode limits above. fetchAndParse treats
// it like any other 2xx decode failure: transient with the malformedBody
// marker, so it retries within the bounded budget and, after exhaustion, the
// harvest scopes the failure to the one result set rather than the upstream.
type torznabLimitError struct {
	limit string
}

func (e *torznabLimitError) Error() string {
	return "torznab response exceeds decode limit: " + e.limit
}

// UnmarshalXML decodes <channel> one <item> at a time so the item-count cap
// rejects an oversized response before its object graph is built, instead of
// after encoding/xml has already allocated an unbounded Items slice. Each
// item bounds itself during decoding (see itemXML.UnmarshalXML) and its
// decoded text is folded into the response-wide budget before it is
// retained. Non-item children are skipped. The decoder this runs under is
// xml.Unmarshal's, which keeps Strict enabled.
func (c *channelXML) UnmarshalXML(d *xml.Decoder, _ xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := c.decodeChild(d, t); err != nil {
				return err
			}
		case xml.EndElement:
			// The first end element at this nesting level is </channel>:
			// DecodeElement and Skip consume every nested element whole.
			return nil
		}
	}
}

// decodeChild skips a non-item child and decodes one <item>, appending it
// under the item cap and folding its decoded text into the response-wide
// budget. A nil return on a skipped non-item child lets the caller's token
// loop continue exactly as before.
func (c *channelXML) decodeChild(d *xml.Decoder, t xml.StartElement) error {
	if t.Name.Local != "item" {
		return d.Skip()
	}
	if len(c.Items) >= maxUpstreamItems {
		return &torznabLimitError{limit: fmt.Sprintf("more than %d items", maxUpstreamItems)}
	}
	var it itemXML
	if err := d.DecodeElement(&it, &t); err != nil {
		return err
	}
	c.textBytes += it.textBytes
	if c.textBytes > maxUpstreamTextBytes {
		return &torznabLimitError{limit: fmt.Sprintf("cumulative decoded text over %d bytes", maxUpstreamTextBytes)}
	}
	c.Items = append(c.Items, it)
	return nil
}

type itemXML struct {
	// Title is the tracker-controlled release title. It is tagged
	// runesafe.Untrusted at this decode boundary — the one place Prowlarr
	// titles enter the program — so the trust decision is recorded on the
	// wire struct and any emission of the wire form (a bare slog attr, an
	// fmt.Errorf) is sanitized automatically. toItem unwraps via Raw() into
	// the plain persisted/compute form (runesafe's machine-read persistence
	// rule: feed.json stores raw bytes in plain string fields), and the
	// human-facing sinks keep their own layers: the XML render escapes over
	// the rune belt (escTo), capped log lines use capLogText.
	Title     runesafe.Untrusted `xml:"title"`
	GUID      string             `xml:"guid"`
	Comments  string             `xml:"comments"`
	Link      string             `xml:"link"`
	PubDate   string             `xml:"pubDate"`
	Enclosure enclosureXML       `xml:"enclosure"`
	Attrs     []attrXML          `xml:"attr"`
	Size      int64              `xml:"size"`
	// textBytes accumulates the decoded text of every field occurrence in
	// this item (unexported: invisible to encoding/xml). channelXML's
	// decodeChild folds it into the response-wide budget.
	textBytes int
}

// UnmarshalXML decodes one <item> child-by-child so the attr-count and
// per-field caps reject an oversized item DURING decoding - before
// encoding/xml materializes an unbounded []attrXML from a run of tiny
// <attr/> elements or retains a multi-megabyte field - instead of
// validating the fully-built object graph after the fact. Unknown children
// are skipped whole; the decoder stays xml.Unmarshal's Strict one.
func (x *itemXML) UnmarshalXML(d *xml.Decoder, _ xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := x.decodeChild(d, t); err != nil {
				return err
			}
		case xml.EndElement:
			// The first end element at this nesting level is </item>.
			return nil
		}
	}
}

// decodeChild decodes and validates one child element of an <item>: the
// plain string scalars route through stringField's destination lookup, the
// remaining recognized children keep their dedicated decoders, and unknown
// children are skipped. Each recognized field is bounded as it decodes; an
// <attr> is rejected BEFORE decoding once the per-item attr cap is reached,
// so the cap bounds the allocation instead of merely reporting it.
func (x *itemXML) decodeChild(d *xml.Decoder, t xml.StartElement) error {
	if dst := x.stringField(t.Name.Local); dst != nil {
		return x.decodeField(d, t, dst)
	}
	switch t.Name.Local {
	case "title":
		return x.decodeUntrustedField(d, t, &x.Title)
	case "size":
		return x.decodeSizeField(d, t)
	case "enclosure":
		return x.decodeEnclosure(d, t)
	case "attr":
		if len(x.Attrs) >= maxUpstreamAttrs {
			return &torznabLimitError{limit: fmt.Sprintf("more than %d attrs on one item", maxUpstreamAttrs)}
		}
		return x.decodeAttr(d, t)
	default:
		return d.Skip()
	}
}

// stringField maps a recognized plain string child element to its
// destination field, or nil for anything needing more than decodeField's
// bounded string decode (the untrusted title, the numeric size, the two
// structured children).
func (x *itemXML) stringField(name string) *string {
	switch name {
	case "guid":
		return &x.GUID
	case "comments":
		return &x.Comments
	case "link":
		return &x.Link
	case "pubDate":
		return &x.PubDate
	default:
		return nil
	}
}

// decodeSizeField decodes the <size> child: bounded text first, numeric
// conversion second - decoding straight into the int64 would let a
// multi-megabyte <size> text bypass the per-field cap and the cumulative
// budget entirely (the conversion error, when it came, arrived only after
// the allocation).
func (x *itemXML) decodeSizeField(d *xml.Decoder, t xml.StartElement) error {
	var s string
	if err := d.DecodeElement(&s, &t); err != nil {
		return err
	}
	n, err := x.boundedInt64(s)
	if err != nil {
		return err
	}
	x.Size = n
	return nil
}

// decodeEnclosure reads an <enclosure>'s recognized attributes off its start
// element, bounding and accounting each retained value BEFORE it is parsed or
// stored. The struct decode it replaces materialized the attributes first and
// accounted only the URL afterwards, leaving the length text outside the
// budget; here the same accounting helper covers every recognized field. The
// element body is skipped whole (Torznab enclosures are attribute-only).
func (x *itemXML) decodeEnclosure(d *xml.Decoder, t xml.StartElement) error {
	var enc enclosureXML
	for _, a := range t.Attr {
		switch a.Name.Local {
		case "url":
			if err := x.account(a.Value); err != nil {
				return err
			}
			enc.URL = a.Value
		case "length":
			n, err := x.boundedInt64(a.Value)
			if err != nil {
				return err
			}
			enc.Length = n
		}
	}
	x.Enclosure = enc
	return d.Skip()
}

// decodeAttr reads one <torznab:attr>'s name/value off its start element,
// bounding and accounting both retained fields before they are stored (the
// struct decode it replaces materialized them first and accounted after).
// The element body is skipped whole (attr elements are attribute-only).
func (x *itemXML) decodeAttr(d *xml.Decoder, t xml.StartElement) error {
	var a attrXML
	for _, at := range t.Attr {
		switch at.Name.Local {
		case "name":
			if err := x.account(at.Value); err != nil {
				return err
			}
			a.Name = at.Value
		case "value":
			if err := x.account(at.Value); err != nil {
				return err
			}
			a.Value = at.Value
		}
	}
	x.Attrs = append(x.Attrs, a)
	return d.Skip()
}

// boundedInt64 bounds and accounts one numeric text value through the same
// accounting helper as the string fields, then parses it, so an oversized
// numeric field is charged against the item's budget (and capped) before
// strconv ever sees it. TrimSpace mirrors encoding/xml's own numeric
// conversion.
func (x *itemXML) boundedInt64(s string) (int64, error) {
	if err := x.account(s); err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// decodeField decodes one text child into dst, bounding and accounting it.
// Every decoded occurrence is accounted (a repeated <title> overwrites dst
// but still consumes budget), so duplicate elements cannot amplify past the
// cumulative cap.
func (x *itemXML) decodeField(d *xml.Decoder, t xml.StartElement, dst *string) error {
	var s string
	if err := d.DecodeElement(&s, &t); err != nil {
		return err
	}
	if err := x.account(s); err != nil {
		return err
	}
	*dst = s
	return nil
}

// decodeUntrustedField decodes one text element into an Untrusted-tagged
// destination: the same accounting as decodeField, with the provenance tag
// applied at the decode boundary (raw bytes preserved — Untrusted has no
// UnmarshalText, and the explicit conversion here keeps the manual decoder's
// *string plumbing out of the tagged field).
func (x *itemXML) decodeUntrustedField(d *xml.Decoder, t xml.StartElement, dst *runesafe.Untrusted) error {
	var s string
	if err := x.decodeField(d, t, &s); err != nil {
		return err
	}
	*dst = runesafe.Untrusted(s)
	return nil
}

// chargeText enforces the per-field cap on one decoded string and folds it
// into a cumulative decoded-text counter, failing fast on either breach. It
// is the single home of the decode-time budget arithmetic, shared by the
// item decoder (itemXML.account) and the <error>-document decoder
// (errorXML.UnmarshalXML), so the cap policy cannot drift between them.
func chargeText(total *int, s string) error {
	if len(s) > maxUpstreamFieldBytes {
		return &torznabLimitError{limit: fmt.Sprintf("field longer than %d bytes", maxUpstreamFieldBytes)}
	}
	*total += len(s)
	if *total > maxUpstreamTextBytes {
		return &torznabLimitError{limit: fmt.Sprintf("cumulative decoded text over %d bytes", maxUpstreamTextBytes)}
	}
	return nil
}

// account enforces the per-field cap on one decoded string and accumulates
// it into the item's text counter, failing fast when this single item
// already exceeds the whole response budget.
func (x *itemXML) account(s string) error { return chargeText(&x.textBytes, s) }

type enclosureXML struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
}

type attrXML struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// errorXML mirrors a Newznab/Torznab <error> document an upstream can return
// in place of an RSS feed (bad credentials, a named indexer failure) - the
// same shape renderError emits on the serving side. Decoding is custom (see
// UnmarshalXML) so every attribute is bounded BEFORE assignment.
type errorXML struct {
	Code        string
	Description string
}

// UnmarshalXML decodes the <error> document under the same decode-time
// budget itemXML.account enforces on feed items: only an <error> root is
// accepted, and every attribute value is charged against
// maxUpstreamFieldBytes and the cumulative maxUpstreamTextBytes BEFORE it is
// assigned, returning a *torznabLimitError on breach - the plain struct
// unmarshal this replaces copied up to the transport cap into the fields and
// only len()-checked them afterwards.
func (e *errorXML) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "error" {
		return fmt.Errorf("expected torznab error element, got %s", start.Name.Local)
	}
	total := 0
	for _, attr := range start.Attr {
		if err := chargeText(&total, attr.Value); err != nil {
			return err
		}
		switch attr.Name.Local {
		case "code":
			e.Code = attr.Value
		case "description":
			e.Description = attr.Value
		}
	}
	return d.Skip()
}

// upstreamDocError is parseTorznab's error for a syntactically VALID Torznab
// <error> document delivered in place of an RSS feed (the errorXML shape: bad
// credentials, a named indexer failure). It is a deliberate upstream-scoped
// answer, not a garbled body: fetchAndParse still wraps it transient (the
// bounded retry budget is unchanged) but never marks it malformedBody, so
// after retry exhaustion the harvest latches the failed scope instead of
// treating an upstream-wide auth/config failure as one show's poison result
// set. The string fields hold the RAW upstream text (so fetchAndParse's
// exact-substring API-key redaction sees the untruncated key); Error() is the
// sanitizing emit boundary.
type upstreamDocError struct {
	code        string
	description string
	// codeNum is the document code parsed ONCE at construction from the raw
	// upstream text (-1 when non-numeric), before fetchAndParse's API-key
	// redaction can rewrite the code string. Both classification consumers -
	// terminalTorznabCode's retry decision and requestScopedHarvestError's
	// show-vs-scope decision - read this field, so a short all-digit
	// Prowlarr key occurring inside a valid code (key "2" turning "201"
	// into "REDACTED01") can no longer corrupt control flow: redaction
	// rewrites only the display strings.
	codeNum int
}

// newUpstreamDocError builds the error from the document's raw code and
// description, parsing codeNum from the untouched code text (see the field
// comment for why classification must never re-parse the string).
func newUpstreamDocError(code, description string) *upstreamDocError {
	return &upstreamDocError{code: code, description: description, codeNum: torznabCodeNum(code)}
}

// torznabCodeNum parses a Torznab <error> document code, returning -1 for
// anything non-numeric (an unknown shape classifies as neither terminal nor
// request-scoped, the conservative default).
func torznabCodeNum(code string) int {
	n, err := strconv.Atoi(code)
	if err != nil {
		return -1
	}
	return n
}

// parseErrorDocument strictly parses a Newznab/Torznab <error> document
// (errorXML.UnmarshalXML only accepts an <error> root), bounding its code and
// description AT DECODE TIME before an upstreamDocError retains them: the
// previous unrestricted unmarshal let a compromised upstream park up to the
// 16 MiB transport cap in the retained error strings, which the retry loop
// then redacted and logged on every attempt. The bound REJECTS an over-cap
// document (the decoder's *torznabLimitError, a definitive verdict
// parseTorznab propagates) rather than truncating it, which preserves the
// redact-before-sanitize ordering: the retained fields stay RAW and
// untruncated so fetchAndParse's exact-substring API-key redaction always
// sees the intact key, and Error() remains the sanitizing emit boundary.
func parseErrorDocument(body []byte) (*upstreamDocError, error) {
	var e errorXML
	if err := xml.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	return newUpstreamDocError(e.Code, e.Description), nil
}

func (e *upstreamDocError) Error() string {
	return fmt.Sprintf("upstream torznab error code=%s: %s",
		sanitizeUpstreamText(e.code), sanitizeUpstreamText(e.description))
}

// capLogText bounds and cleans an untrusted string before it reaches a log
// line, delegating to runesafe.SanitizeSingleLineBounded (single-line rune
// safety, then a rune-boundary byte cap with the "..." truncation marker).
// It is the shared emit-boundary policy behind sanitizeUpstreamText and
// logParam; the composition itself now lives in the library, so the policy
// cannot drift per consumer.
func capLogText(s string, maxLen int) string {
	return runesafe.SanitizeSingleLineBounded(s, maxLen)
}

// sanitizeUpstreamText bounds and cleans an untrusted Torznab <error>
// code/description before it is carried into an error that reaches slog
// (fetchRaw's Warn, httpx.Do's retry logs) - the same emit-boundary policy
// report.go/anilist.go apply to untrusted upstream text, mirroring anilist's
// sanitizeUpstreamMessage: single-line rune safety, then a 200-byte cap on a
// rune boundary (truncated output appends "...", for a 203-byte maximum) so
// a multi-MB or control-laden <error> body can never spoof or flood a log
// line.
func sanitizeUpstreamText(s string) string { return capLogText(s, 200) }

// parseTorznab decodes a Prowlarr Torznab response into feed items.
func parseTorznab(body []byte) ([]item, error) {
	var feed feedXML
	if err := xml.Unmarshal(body, &feed); err != nil {
		// A decode-limit overflow is already a definitive verdict on a
		// well-formed feed document; skip the <error>-document re-parse of
		// the (up to 16 MiB) body it could never match.
		if limitErr, ok := errors.AsType[*torznabLimitError](err); ok {
			return nil, limitErr
		}
		docErr, docParseErr := parseErrorDocument(body)
		if docParseErr == nil {
			return nil, docErr
		}
		// An over-cap <error> attribute is a definitive decode-limit
		// verdict on the fallback parse too; propagate it over the generic
		// RSS parse failure so it classifies like every other limit breach.
		if limitErr, ok := errors.AsType[*torznabLimitError](docParseErr); ok {
			return nil, limitErr
		}
		return nil, fmt.Errorf("parse torznab feed: %w", err)
	}
	items := make([]item, 0, len(feed.Channel.Items))
	for i := range feed.Channel.Items {
		items = append(items, feed.Channel.Items[i].toItem())
	}
	return items, nil
}

// toItem converts a decoded Torznab item into an Item, reading size, info hash,
// seeders/peers, and categories from the torznab:attr elements.
func (x *itemXML) toItem() item {
	attrs := make(map[string]string, len(x.Attrs))
	var cats []int
	for _, a := range x.Attrs {
		if a.Name == "category" {
			// Categories are tracker-controlled numerics rendered back into the
			// served feed; only positive ids are meaningful Torznab categories,
			// so a negative/zero value is dropped like the other count fields
			// are clamped below.
			if n, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil && n > 0 {
				cats = append(cats, n)
			}
			continue
		}
		attrs[a.Name] = a.Value
	}

	dl := x.Enclosure.URL
	if dl == "" {
		dl = x.Link
	}
	size := x.Enclosure.Length
	if size <= 0 {
		size = x.Size
	}
	if size <= 0 {
		size, _ = strconv.ParseInt(strings.TrimSpace(attrs["size"]), 10, 64)
	}

	// The decoded numeric fields are tracker-controlled: normalize every count
	// to the feed's zero-as-unknown domain so a malformed-but-valid response
	// cannot render a negative enclosure length/size attr or an inflated peer
	// count derived from an unbounded negative seeders value.
	size = max(size, 0)
	seeders := max(attrInt(attrs, "seeders"), 0)
	leechers := max(attrInt(attrs, "leechers"), 0)
	if leechers == 0 {
		if peers := max(attrInt(attrs, "peers"), 0); peers > seeders {
			leechers = peers - seeders
		}
	}

	return item{
		// Raw() by design: item rides journalItem into feed.json, and
		// runesafe's machine-read persistence rule stores raw bytes in
		// plain fields (a tagged field would round-trip sanitized). The
		// emit boundaries own the policy: escTo for the XML render,
		// capLogText for log lines.
		Title:       strings.TrimSpace(x.Title.Raw()),
		GUID:        strings.TrimSpace(x.GUID),
		InfoURL:     strings.TrimSpace(x.Comments),
		DownloadURL: strings.TrimSpace(dl),
		InfoHash:    validInfoHash(attrs["infohash"]),
		Categories:  cats,
		PubDate:     parsePubDate(x.PubDate),
		Size:        size,
		Seeders:     seeders,
		Leechers:    leechers,
	}
}

// attrInt reads a named torznab:attr as an int, defaulting to 0.
func attrInt(attrs map[string]string, name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(attrs[name]))
	return n
}

// pubDateLayouts are the date formats seen on Torznab <pubDate> elements.
var pubDateLayouts = []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339}

// parsePubDate parses a Torznab pubDate, returning the zero time on failure.
func parsePubDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

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
	"github.com/cplieger/xmlx"
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
// incorrect credentials (100). Prowlarr surfaces the <error> description on the
// indexer's test, so the operator sees why the save failed.
const errCodeIncorrectCredentials = 100

// errCodeUnknown is the Newznab/Torznab "unknown error" code (900), used for an
// unexpected internal failure such as a recovered handler panic.
const errCodeUnknown = 900

// item is one Torznab feed release in the WIRE vocabulary - the fields parsed from
// a Prowlarr result plus the SeaDex download-volume-factor marker this feed adds.
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

// renderCaps returns the t=caps response. The categories and search modes match the
// Nyaa + AnimeBytes indexer definitions this feed proxies (q-based search with
// season/ep for TV; no id search), so the arrs query the feed as they would those.
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
// description, so it is how a misconfiguration surfaces on the save-test.
func renderError(code int, description string) string {
	var b strings.Builder
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, `<error code="%d" description="%s"/>`, code, esc(description))
	return b.String()
}

// renderFeed returns the Torznab RSS feed for items plus how many it actually
// emitted: rendered < len(items) means the byte budget truncated the document, and
// the returned count is what the request log and the truncation WARN report, so a
// truncated feed is never logged as complete. It is written by hand so the
// `torznab:` prefixed attributes come out exactly as the arrs expect.
func renderFeed(items []item) (doc string, rendered int) {
	var b strings.Builder
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, `<rss version="2.0" xmlns:torznab="%s">`, torznabNS)
	b.WriteString("<channel>")
	b.WriteString("<title>seadex-scout</title>")
	for i := range items {
		if b.Len() > maxRenderedFeedBytes {
			// A pathological feed (every field at the cap, escape-amplified ~5x) must
			// degrade to a truncated-but-valid document instead of OOMing the container.
			break
		}
		writeItem(&b, &items[i])
		rendered++
	}
	b.WriteString("</channel></rss>")
	return b.String(), rendered
}

// writeItem renders one release as an <item>: title, size, seeders and download URL,
// plus the SeaDex marker. The enclosure is omitted when there is no download URL.
// Seeders are floored to 1, so the arrs' minimum-seeders check cannot reject a
// curated release whose swarm count is momentarily 0 or synthesized.
func writeItem(b *strings.Builder, it *item) {
	b.WriteString("<item>")
	writeText(b, "title", it.Title)
	writeText(b, "guid", it.guid())
	if it.InfoURL != "" {
		writeText(b, "comments", it.InfoURL)
	}
	// Always render pubDate: Sonarr's RSS parser rejects the WHOLE response when any
	// item lacks the element, so omitting it turns one unparseable upstream date into
	// an empty search answer.
	pub := it.PubDate
	if pub.IsZero() {
		pub = time.Unix(0, 0)
	}
	writeText(b, "pubDate", pub.UTC().Format(time.RFC1123Z))
	// Clamp like the peer counts below: render-side validation is the final totality
	// guard, independent of which ingress produced the item. Each producer normalizes
	// its own domain, but no single gate covers both paths, so an item reaching the
	// renderer with a negative size must not render an invalid enclosure length.
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
	if it.DownloadVolumeFactor != "" {
		writeAttr(b, "downloadvolumefactor", it.DownloadVolumeFactor)
		writeAttr(b, "uploadvolumefactor", "1")
	}

	seeders := max(it.Seeders, 1)
	leechers := max(it.Leechers, 0)
	// Saturate instead of wrapping: attrInt accepts counts through math.MaxInt, so a
	// malformed-but-valid item with huge counts would otherwise overflow negative and
	// render an invalid peers attr.
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

// escTo escapes s for use in XML text or attribute values, writing directly into b.
// Escaping in place keeps renderFeed from holding a second escaped copy of every
// field: XML escaping can expand an ampersand-heavy value ~5x, and those temporary
// copies were one leg of the snapshot memory-amplification path.
func escTo(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(runesafe.Sanitize(s)))
}

// esc escapes a string for XML text or attribute values, returning a new string.
// Paths that already own a strings.Builder should prefer escTo.
func esc(s string) string {
	var b strings.Builder
	escTo(&b, s)
	return b.String()
}

// feedXML / channelXML / itemXML / attrXML mirror the Torznab RSS a Prowlarr
// indexer endpoint returns. The manual decoders switch on each element's LOCAL
// name, so a torznab:attr element matches regardless of the declared prefix.
type feedXML struct {
	XMLName xml.Name   `xml:"rss"`
	Channel channelXML `xml:"channel"`
}

type channelXML struct {
	// budget is the response-wide decoded-text allowance, created on the first
	// UnmarshalXML call and handed to each item as it decodes. It lives on the struct
	// because encoding/xml re-invokes UnmarshalXML on the same channelXML value for
	// each <channel> sibling, so a per-call budget would reset while items grew.
	budget *xmlx.Budget
	Items  []itemXML
}

// Decode limits on an untrusted upstream Torznab response. The transport cap bounds
// wire bytes only: a compromised Prowlarr could pack millions of tiny elements into
// that budget, or one multi-megabyte field, amplifying allocations in the decoded
// graph and again in renderFeed (CWE-400). These constants bound the decoded
// representation independently; any overflow fails the parse closed.
const (
	// maxUpstreamItems caps item elements per response. It reuses the render cap:
	// the served feed never renders more than maxItems, so accepting more has no
	// value. A live AB series search returns ~145.
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

// Lexical preflight limits: the values this app gives xmlx.Preflight, the allocation
// gate over the RAW response bytes enforced BEFORE encoding/xml constructs any
// token. The decode-time caps above bound the RETAINED graph, but one text node or
// start tag can force a transient allocation up to the transport cap before either
// sees it, and concurrent searches can stack several such transients past the
// container memory budget. The library owns the scan; these numbers are the Torznab
// contract, chosen to reject a response already far outside it.
const (
	// maxUpstreamTextRunBytes caps one contiguous raw text or CDATA run. Entity
	// escaping only EXPANDS raw text relative to its decoded form, so 64 KiB is far
	// past any field that could still decode legally under the 4 KiB field cap.
	maxUpstreamTextRunBytes = 64 << 10
	// maxUpstreamTokenBytes caps one markup token. A start tag holding
	// maxUpstreamTagAttrs attributes of maxUpstreamFieldBytes each stays under it.
	maxUpstreamTokenBytes = 128 << 10
	// maxUpstreamTagAttrs caps XML attributes on ONE start tag - the lexical twin of
	// maxUpstreamAttrs, which counts <torznab:attr> ELEMENTS per item. Torznab
	// elements carry at most a handful; 16 is generous.
	maxUpstreamTagAttrs = 16
	// maxUpstreamDepth caps element nesting depth. The decoder pushes one
	// heap-allocated stack entry per open element and its Skip path has no depth
	// bound, so an all-opens body of tiny tags could grow that stack by ~2.7M entries
	// from one 8 MiB response. A Torznab document is depth ~4.
	maxUpstreamDepth = 64
	// maxUpstreamElements caps total elements in one response, the bound for
	// amplification by COUNT rather than size: millions of tiny empty elements pass
	// every per-token bound and charge nothing to the text budget.
	maxUpstreamElements = 8 * maxUpstreamItems * 10
)

// upstreamLimits is the xmlx.Limits value the preflight runs under, assembled
// from the Torznab contract constants above. The library owns the scan; this app
// owns the numbers.
var upstreamLimits = xmlx.Limits{
	MaxTextRunBytes: maxUpstreamTextRunBytes,
	MaxTokenBytes:   maxUpstreamTokenBytes,
	MaxTagAttrs:     maxUpstreamTagAttrs,
	MaxDepth:        maxUpstreamDepth,
	MaxElements:     maxUpstreamElements,
}

// preflightTorznab is the lexical gate over the raw response bytes, converting an
// xmlx bound rejection into this package's *torznabLimitError so a lexical breach
// classifies exactly like a decode-time one.
func preflightTorznab(body []byte) error {
	return asLimitError(xmlx.Preflight(body, upstreamLimits))
}

// newUpstreamBudget returns the decode-time text budget for ONE response: the
// per-field cap and the response-wide cumulative cap, shared by the feed item
// decoders and the <error>-document decoder so the policy cannot drift.
func newUpstreamBudget() *xmlx.Budget {
	b, err := xmlx.NewBudget(maxUpstreamFieldBytes, maxUpstreamTextBytes)
	if err != nil {
		// Unreachable: both caps are positive constants. A panic here would be a
		// build-time mistake, so fail loudly rather than serving unbounded.
		panic("indexer: invalid upstream decode budget: " + err.Error())
	}
	return b
}

// asLimitError wraps an xmlx bound rejection in this package's *torznabLimitError,
// so every decode-limit breach presents as one error type - which is what
// fetchAndParse classifies on and what parseTorznab checks before re-parsing the
// body as an <error> document. Any other error passes through untouched.
func asLimitError(err error) error {
	var le *xmlx.LimitError
	if errors.As(err, &le) {
		return &torznabLimitError{err: le}
	}
	return err
}

// maxRenderedFeedBytes bounds one rendered feed document, the render-side twin of
// maxUpstreamTextBytes: the search path is aggregate-bounded at decode, but the
// persisted journal path is bounded only per field, and escaping can expand a field
// ~5x. Overshoot past the check is at most one item.
const maxRenderedFeedBytes = 8 << 20

// torznabLimitError is parseTorznab's fail-closed error for a syntactically valid
// response that exceeds the decode limits above. fetchAndParse treats it like any
// other 2xx decode failure: transient with the malformedBody marker.
type torznabLimitError struct {
	// err is the library's *xmlx.LimitError when the bound was xmlx's, nil for an
	// app-side cap, so a consumer can also match errors.Is(err, xmlx.ErrLimit).
	err error
	// limit describes an app-side cardinality cap (item count, attr count).
	limit string
}

func (e *torznabLimitError) Error() string {
	detail := e.limit
	if e.err != nil {
		detail = e.err.Error()
	}
	return "torznab response exceeds decode limit: " + detail
}

// Unwrap exposes the library rejection, so errors.Is(err, xmlx.ErrLimit) holds
// for a bound xmlx enforced.
func (e *torznabLimitError) Unwrap() error { return e.err }

// UnmarshalXML decodes <channel> one <item> at a time so the item-count cap rejects
// an oversized response before its object graph is built, instead of after
// encoding/xml has allocated an unbounded Items slice. Each item bounds itself and
// its decoded text is folded into the response-wide budget. Non-item children are
// skipped, under xml.Unmarshal's Strict decoder.
func (c *channelXML) UnmarshalXML(d *xml.Decoder, _ xml.StartElement) error {
	if c.budget == nil {
		// First invocation owns the response-wide allowance. Setting it here means no
		// construction path can leave it nil, and the field persists across sibling
		// <channel> invocations rather than resetting per channel.
		c.budget = newUpstreamBudget()
	}
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

// decodeChild skips a non-item child and decodes one <item>, appending it under the
// item cap and folding its decoded text into the response-wide budget. A nil return
// on a skipped child lets the caller's token loop continue.
func (c *channelXML) decodeChild(d *xml.Decoder, t xml.StartElement) error {
	if t.Name.Local != "item" {
		return d.Skip()
	}
	if len(c.Items) >= maxUpstreamItems {
		return &torznabLimitError{limit: fmt.Sprintf("more than %d items", maxUpstreamItems)}
	}
	it := itemXML{budget: c.budget}
	if err := d.DecodeElement(&it, &t); err != nil {
		return err
	}
	c.Items = append(c.Items, it)
	return nil
}

// The element names this type decodes are NOT struct tags: itemXML implements
// UnmarshalXML, so encoding/xml never reads them. decodeChild owns the
// child-element vocabulary and decodeEnclosure/decodeAttr the attribute one, so a
// field added here decodes nothing until it is wired into one of those.
type itemXML struct {
	// Title is the tracker-controlled release title, tagged runesafe.Untrusted at this
	// decode boundary - the one place Prowlarr titles enter the program - so any
	// emission of the wire form is sanitized automatically.
	budget    *xmlx.Budget
	Attrs     []attrXML
	Title     runesafe.Untrusted
	GUID      string
	Comments  string
	Link      string
	PubDate   string
	Enclosure enclosureXML
	Size      int64
}

// UnmarshalXML decodes one <item> child-by-child so the attr-count and per-field
// caps reject an oversized item DURING decoding - before encoding/xml materializes
// an unbounded []attrXML or retains a multi-megabyte field. Unknown children are
// skipped whole, under xml.Unmarshal's Strict decoder.
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

// decodeChild decodes and validates one child element of an <item>: each recognized
// child decodes into its own destination through a bounded text decode, and unknown
// children are skipped. An <attr> is rejected BEFORE decoding once the per-item
// attr cap is reached, so the cap bounds the allocation rather than reporting it.
func (x *itemXML) decodeChild(d *xml.Decoder, t xml.StartElement) error {
	switch t.Name.Local {
	case "guid":
		return x.decodeField(d, &x.GUID)
	case "comments":
		return x.decodeField(d, &x.Comments)
	case "link":
		return x.decodeField(d, &x.Link)
	case "pubDate":
		return x.decodeField(d, &x.PubDate)
	case "title":
		return x.decodeUntrustedField(d, &x.Title)
	case "size":
		return x.decodeSizeField(d)
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

// decodeSizeField decodes the <size> child: bounded text first, numeric conversion
// second - decoding straight into the int64 would let a multi-megabyte <size> text
// bypass the per-field cap and the cumulative budget entirely.
func (x *itemXML) decodeSizeField(d *xml.Decoder) error {
	s, err := x.budget.DecodeText(d)
	if err != nil {
		return asLimitError(err)
	}
	if s == "" {
		// Mirror encoding/xml's own numeric conversion, which treats an EMPTY value as
		// zero: an <size></size> element must degrade into the zero-as-unknown domain
		// rather than fail the whole response and cost every other curated item in it.
		x.Size = 0
		return nil
	}
	text := strings.TrimSpace(s)
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return err
	}
	x.Size = n
	return nil
}

// decodeEnclosure reads an <enclosure>'s recognized attributes off its start
// element, bounding and accounting each retained value BEFORE it is parsed or
// stored - where the struct decode it replaces materialized the attributes first
// and left the length text outside the budget. The body is skipped whole.
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

// decodeAttr reads one <torznab:attr>'s name/value off its start element, bounding
// and accounting both retained fields before they are stored. The element body is
// skipped whole (attr elements are attribute-only).
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

// boundedInt64 bounds and accounts one numeric text value through the same helper as
// the string fields, then parses it, so an oversized numeric field is charged and
// capped before strconv sees it. TrimSpace mirrors encoding/xml's own conversion.
func (x *itemXML) boundedInt64(s string) (int64, error) {
	if err := x.account(s); err != nil {
		return 0, err
	}
	if s == "" {
		// Same empty-is-zero mirror as decodeSizeField, keyed on the RAW value: an
		// <enclosure length=""/> decoded as zero before this manual decoder, and
		// itemSize treats a zero length as unknown. A whitespace-only length fails.
		return 0, nil
	}
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// decodeField decodes one text child into dst, bounded and charged as it
// accumulates: DecodeText stops at the CharData token that would cross either cap,
// so a value split across CDATA seams is rejected during token iteration. Every
// decoded occurrence is charged, so duplicate elements cannot amplify past the cap.
func (x *itemXML) decodeField(d *xml.Decoder, dst *string) error {
	s, err := x.budget.DecodeText(d)
	if err != nil {
		return asLimitError(err)
	}
	*dst = s
	return nil
}

// decodeUntrustedField decodes one text element into an Untrusted-tagged
// destination: the same accounting as decodeField, with the provenance tag applied
// at the decode boundary and the raw bytes preserved.
func (x *itemXML) decodeUntrustedField(d *xml.Decoder, dst *runesafe.Untrusted) error {
	var s string
	if err := x.decodeField(d, &s); err != nil {
		return err
	}
	*dst = runesafe.Untrusted(s)
	return nil
}

// account charges one decoded string against the response-wide budget, which
// enforces the per-field and cumulative caps and mutates nothing on rejection.
func (x *itemXML) account(s string) error { return asLimitError(x.budget.Charge(s)) }

// enclosureXML and attrXML are decoded-value carriers, not XML schema:
// decodeEnclosure and decodeAttr read the attributes off the start element by hand
// so each retained value is charged to the budget before it is stored.
type enclosureXML struct {
	URL    string
	Length int64
}

type attrXML struct {
	Name  string
	Value string
}

// errorXML mirrors a Newznab/Torznab <error> document an upstream can return in
// place of an RSS feed - the same shape renderError emits. Decoding is custom so
// every attribute is bounded BEFORE assignment.
type errorXML struct {
	Code        string
	Description string
}

// UnmarshalXML decodes the <error> document under the same decode-time budget feed
// items charge: only an <error> root is accepted, and every attribute value is
// charged against the per-field and cumulative caps BEFORE assignment, returning a
// *torznabLimitError on breach.
func (e *errorXML) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "error" {
		return fmt.Errorf("expected torznab error element, got %s", start.Name.Local)
	}
	// One allowance per document, scoped to this call: xml.Unmarshal decodes only a
	// document's FIRST element, so unlike channelXML this decoder runs exactly once
	// and needs no field to carry the budget across invocations. The <error> fallback
	// re-parses the SAME body, so it gets a fresh allowance either way.
	budget := newUpstreamBudget()
	for _, attr := range start.Attr {
		if err := asLimitError(budget.Charge(attr.Value)); err != nil {
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

// upstreamDocError is parseTorznab's error for a syntactically VALID Torznab <error>
// document delivered in place of an RSS feed. It is a deliberate upstream-scoped
// answer, not a garbled body: fetchAndParse still wraps it transient but never marks
// it malformedBody, so after retry exhaustion the harvest latches the failed scope
// instead of treating an upstream-wide failure as one show's poison result set. The
// string fields hold RAW upstream text, so the exact-substring API-key redaction
// sees the untruncated key; Error() is the sanitizing emit boundary.
type upstreamDocError struct {
	code        string
	description string
	// codeNum is the document code parsed ONCE at construction from the raw upstream
	// text (-1 when non-numeric), before fetchAndParse's API-key redaction can rewrite
	// the code string.
	codeNum int
}

// newUpstreamDocError builds the error from the document's raw code and description,
// parsing codeNum from the untouched code text (see the field comment for why).
func newUpstreamDocError(code, description string) *upstreamDocError {
	return &upstreamDocError{code: code, description: description, codeNum: torznabCodeNum(code)}
}

// torznabCodeNum parses a Torznab <error> document code, returning -1 for anything
// non-numeric (an unknown shape classifies as neither terminal nor request-scoped).
func torznabCodeNum(code string) int {
	// TrimSpace like every other untrusted numeric parse in this file: XML preserves
	// attribute whitespace, and a padded code must not degrade to unknown-shape.
	n, err := strconv.Atoi(strings.TrimSpace(code))
	if err != nil {
		return -1
	}
	return n
}

// parseErrorDocument strictly parses a Newznab/Torznab <error> document, bounding
// its code and description AT DECODE TIME before an upstreamDocError retains them:
// the previous unrestricted unmarshal let a compromised upstream park up to the
// transport cap in the retained strings, which the retry loop then logged on every
// attempt. The bound REJECTS an over-cap document rather than truncating it, which
// keeps the retained fields raw so the API-key redaction sees the intact key.
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

// capLogText bounds and cleans an untrusted string before it reaches a log line,
// delegating to runesafe.SanitizeSingleLineBounded. It is the shared emit-boundary
// policy behind sanitizeUpstreamText and logParam.
func capLogText(s string, maxLen int) string {
	return runesafe.SanitizeSingleLineBounded(s, maxLen)
}

// upstreamTextMaxBytes bounds one untrusted upstream text value on its way to a log
// line or an error message. It is named because TWO compositions must agree on it:
// sanitizeUpstreamText below (upstreamDocError's emit boundary) and the upstream's
// own redactAndBound (prowlarr.go), which bounds the same text after redacting it on
// both sides of the sanitizer. A drift between the two would earn a second
// truncation marker at the emit boundary.
const upstreamTextMaxBytes = 200

// sanitizeUpstreamText bounds and cleans an untrusted Torznab <error>
// code/description before it is carried into an error that reaches slog: single-line
// rune safety, then an upstreamTextMaxBytes cap on a rune boundary, so a multi-MB or
// control-laden <error> body can never spoof or flood a log line.
func sanitizeUpstreamText(s string) string { return capLogText(s, upstreamTextMaxBytes) }

// parseTorznab decodes a Prowlarr Torznab response into feed items. The lexical
// preflight runs over the raw bytes BEFORE either xml.Unmarshal, so an
// attacker-shaped response cannot force transient token allocations past the
// decode caps on either path.
func parseTorznab(body []byte) ([]item, error) {
	if err := preflightTorznab(body); err != nil {
		return nil, err
	}
	var feed feedXML
	if err := xml.Unmarshal(body, &feed); err != nil {
		// A decode-limit overflow is already a definitive verdict on a well-formed
		// feed document; skip the <error>-document re-parse it could never match.
		if limitErr, ok := errors.AsType[*torznabLimitError](err); ok {
			return nil, limitErr
		}
		docErr, docParseErr := parseErrorDocument(body)
		if docParseErr == nil {
			return nil, docErr
		}
		// An over-cap <error> attribute is a definitive limit verdict on the fallback
		// parse too; propagate it over the generic RSS parse failure.
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
	attrs, cats := splitItemAttrs(x.Attrs)

	dl := x.Enclosure.URL
	if dl == "" {
		dl = x.Link
	}
	seeders, leechers := itemPeers(attrs)

	return item{
		// Raw() by design: item rides journalItem into feed.json, and runesafe's
		// machine-read persistence rule stores raw bytes in plain fields. The emit
		// boundaries own the policy: escTo for the render, capLogText for logs.
		Title:       strings.TrimSpace(x.Title.Raw()),
		GUID:        strings.TrimSpace(x.GUID),
		InfoURL:     strings.TrimSpace(x.Comments),
		DownloadURL: strings.TrimSpace(dl),
		InfoHash:    validInfoHash(attrs["infohash"]),
		Categories:  cats,
		PubDate:     parsePubDate(x.PubDate),
		Size:        itemSize(x, attrs),
		Seeders:     seeders,
		Leechers:    leechers,
	}
}

// splitItemAttrs partitions a decoded item's torznab:attr elements into the
// named-attribute map (last value wins for a duplicated name) and the
// positive category ids in document order.
func splitItemAttrs(in []attrXML) (attrs map[string]string, cats []int) {
	attrs = make(map[string]string, len(in))
	for _, a := range in {
		if a.Name == "category" {
			// Categories are tracker-controlled numerics rendered back into the served
			// feed; only positive ids are meaningful Torznab categories.
			if n, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil && n > 0 {
				cats = append(cats, n)
			}
			continue
		}
		attrs[a.Name] = a.Value
	}
	return attrs, cats
}

// itemSize resolves a decoded item's byte size from the enclosure length, the <size>
// element, then the size torznab:attr, in that order. The decoded numerics are
// tracker-controlled, so the result is normalized to the feed's zero-as-unknown
// domain: a malformed-but-valid response cannot render a negative length.
func itemSize(x *itemXML, attrs map[string]string) int64 {
	size := x.Enclosure.Length
	if size <= 0 {
		size = x.Size
	}
	if size <= 0 {
		size, _ = strconv.ParseInt(strings.TrimSpace(attrs["size"]), 10, 64)
	}
	return max(size, 0)
}

// itemPeers normalizes the tracker-controlled peer counts to the feed's
// zero-as-unknown domain, deriving leechers from a peers attr only when it exceeds
// the seeders, so an inflated count cannot come from a negative seeders value.
func itemPeers(attrs map[string]string) (seeders, leechers int) {
	seeders = max(attrInt(attrs, "seeders"), 0)
	leechers = max(attrInt(attrs, "leechers"), 0)
	if leechers == 0 {
		if peers := max(attrInt(attrs, "peers"), 0); peers > seeders {
			leechers = peers - seeders
		}
	}
	return seeders, leechers
}

// attrInt reads a named torznab:attr as an int, defaulting to 0.
func attrInt(attrs map[string]string, name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(attrs[name]))
	return n
}

// pubDateLayouts are the date formats seen on Torznab <pubDate> elements.
var pubDateLayouts = []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339}

// parsePubDate parses a Torznab pubDate, returning the zero time on failure
// (an empty or unparseable value is the same outcome: no layout accepts one).
func parsePubDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range pubDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

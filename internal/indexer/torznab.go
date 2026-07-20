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

// item is one feed release: the real fields parsed from a Prowlarr Torznab
// result, plus the SeaDex download-volume-factor marker this feed adds. The
// json tags pin the persisted feed.json snapshot contract (see writer.go's
// snapshot): they mirror the historical field names, so renaming a Go field
// never silently changes the on-disk format a resident daemon or an upgraded
// binary reads back.
//
// FirstSeen, Key, and AniListID are journal bookkeeping carried only by
// synthesized RSS items (see journal.go): when the release entered the
// journal (PubDate mirrors it; the prune clock keys on it), the torrent's
// stable tracker identity (nyaa:{id} / ab:{id} - the harvested-title cache
// key), and the SeaDex entry's AniList id (the harvest query group). They are
// zero on proxied search results, which are never persisted, and writeItem
// does not render them.
type item struct {
	PubDate              time.Time `json:"PubDate"`
	FirstSeen            time.Time `json:"FirstSeen,omitzero"`
	Title                string    `json:"Title"`
	GUID                 string    `json:"GUID"`
	InfoURL              string    `json:"InfoURL"`
	DownloadURL          string    `json:"DownloadURL"`
	InfoHash             string    `json:"InfoHash"`
	DownloadVolumeFactor string    `json:"DownloadVolumeFactor"`
	Key                  string    `json:"Key,omitempty"`
	Categories           []int     `json:"Categories"`
	Size                 int64     `json:"Size"`
	AniListID            int       `json:"AniListID,omitempty"`
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
	if it.DownloadURL != "" {
		b.WriteString(`<enclosure url="`)
		escTo(b, it.DownloadURL)
		fmt.Fprintf(b, `" length="%d" type="application/x-bittorrent"/>`, it.Size)
	}

	cats := it.Categories
	if len(cats) == 0 {
		cats = []int{catAnime}
	}
	for _, c := range cats {
		writeAttr(b, "category", strconv.Itoa(c))
	}
	writeAttr(b, "size", strconv.FormatInt(it.Size, 10))
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
func escTo(b *strings.Builder, s string) {
	_ = xml.EscapeText(b, []byte(s))
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
	Title     string       `xml:"title"`
	GUID      string       `xml:"guid"`
	Comments  string       `xml:"comments"`
	Link      string       `xml:"link"`
	PubDate   string       `xml:"pubDate"`
	Enclosure enclosureXML `xml:"enclosure"`
	Attrs     []attrXML    `xml:"attr"`
	Size      int64        `xml:"size"`
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

// decodeChild decodes and validates one child element of an <item>. Each
// recognized scalar field is bounded as it decodes; an <attr> is rejected
// BEFORE decoding once the per-item attr cap is reached, so the cap bounds
// the allocation instead of merely reporting it; unknown children are
// skipped.
func (x *itemXML) decodeChild(d *xml.Decoder, t xml.StartElement) error {
	switch t.Name.Local {
	case "title":
		return x.decodeField(d, t, &x.Title)
	case "guid":
		return x.decodeField(d, t, &x.GUID)
	case "comments":
		return x.decodeField(d, t, &x.Comments)
	case "link":
		return x.decodeField(d, t, &x.Link)
	case "pubDate":
		return x.decodeField(d, t, &x.PubDate)
	case "size":
		return d.DecodeElement(&x.Size, &t)
	case "enclosure":
		if err := d.DecodeElement(&x.Enclosure, &t); err != nil {
			return err
		}
		return x.account(x.Enclosure.URL)
	case "attr":
		if len(x.Attrs) >= maxUpstreamAttrs {
			return &torznabLimitError{limit: fmt.Sprintf("more than %d attrs on one item", maxUpstreamAttrs)}
		}
		var a attrXML
		if err := d.DecodeElement(&a, &t); err != nil {
			return err
		}
		if err := x.account(a.Name); err != nil {
			return err
		}
		if err := x.account(a.Value); err != nil {
			return err
		}
		x.Attrs = append(x.Attrs, a)
		return nil
	default:
		return d.Skip()
	}
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

// account enforces the per-field cap on one decoded string and accumulates
// it into the item's text counter, failing fast when this single item
// already exceeds the whole response budget.
func (x *itemXML) account(s string) error {
	if len(s) > maxUpstreamFieldBytes {
		return &torznabLimitError{limit: fmt.Sprintf("field longer than %d bytes", maxUpstreamFieldBytes)}
	}
	x.textBytes += len(s)
	if x.textBytes > maxUpstreamTextBytes {
		return &torznabLimitError{limit: fmt.Sprintf("cumulative decoded text over %d bytes", maxUpstreamTextBytes)}
	}
	return nil
}

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
// same shape renderError emits on the serving side.
type errorXML struct {
	XMLName     xml.Name `xml:"error"`
	Code        string   `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}

// upstreamDocError is parseTorznab's error for a syntactically VALID Torznab
// <error> document delivered in place of an RSS feed (the errorXML shape: bad
// credentials, a named indexer failure). It is a deliberate upstream-scoped
// answer, not a garbled body: fetchAndParse still wraps it transient (the
// bounded retry budget is unchanged) but never marks it malformedBody, so
// after retry exhaustion the harvest latches the failed scope instead of
// treating an upstream-wide auth/config failure as one show's poison result
// set. The fields hold the RAW upstream text (so fetchAndParse's exact-
// substring API-key redaction sees the untruncated key); Error() is the
// sanitizing emit boundary.
type upstreamDocError struct {
	code        string
	description string
}

func (e *upstreamDocError) Error() string {
	return fmt.Sprintf("upstream torznab error code=%s: %s",
		sanitizeUpstreamText(e.code), sanitizeUpstreamText(e.description))
}

// capLogText bounds and cleans an untrusted string before it reaches a log
// line: single-line rune safety (runesafe.SanitizeSingleLine), then a byte cap
// on a rune boundary (truncated output appends "..."). It is the shared
// emit-boundary policy behind sanitizeUpstreamText and logParam, so a policy
// change (truncation marker, control-char class) lands once.
func capLogText(s string, maxLen int) string {
	s = runesafe.SanitizeSingleLine(s)
	if len(s) > maxLen {
		s = runesafe.CapBytes(s, maxLen) + "..."
	}
	return s
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
		var e errorXML
		if xml.Unmarshal(body, &e) == nil {
			// Raw fields; Error() sanitizes at the emit boundary so the key
			// redaction in fetchAndParse sees the untruncated text (cap-before-
			// redact would let a boundary-straddling key prefix escape the
			// exact-substring replacement).
			return nil, &upstreamDocError{code: e.Code, description: e.Description}
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
		Title:       strings.TrimSpace(x.Title),
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

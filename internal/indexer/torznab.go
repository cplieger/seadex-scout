package indexer

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
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
// result, plus the SeaDex download-volume-factor marker this feed adds.
type item struct {
	PubDate              time.Time
	Title                string
	GUID                 string
	InfoURL              string
	DownloadURL          string
	InfoHash             string
	DownloadVolumeFactor string
	Categories           []int
	Size                 int64
	Seeders              int
	Leechers             int
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
	b.WriteString(`<limits max="1000" default="100"/>`)
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
		fmt.Fprintf(b, `<enclosure url="%s" length="%d" type="application/x-bittorrent"/>`, esc(it.DownloadURL), it.Size)
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
	// Pass-through (empty-q/RSS) items are not curated and carry no marker, so
	// the attrs are omitted and the arr treats them as normal (factor 1).
	if it.DownloadVolumeFactor != "" {
		writeAttr(b, "downloadvolumefactor", it.DownloadVolumeFactor)
		writeAttr(b, "uploadvolumefactor", "1")
	}

	seeders := max(it.Seeders, 1)
	leechers := max(it.Leechers, 0)
	writeAttr(b, "seeders", strconv.Itoa(seeders))
	writeAttr(b, "peers", strconv.Itoa(seeders+leechers))
	b.WriteString("</item>")
}

// writeText writes a simple escaped <tag>value</tag> element.
func writeText(b *strings.Builder, tag, value string) {
	b.WriteString("<" + tag + ">")
	b.WriteString(esc(value))
	b.WriteString("</" + tag + ">")
}

// writeAttr writes a <torznab:attr name=".." value=".."/> element.
func writeAttr(b *strings.Builder, name, value string) {
	fmt.Fprintf(b, `<torznab:attr name="%s" value="%s"/>`, name, esc(value))
}

// esc escapes a string for use in XML text or attribute values.
func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
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
}

type enclosureXML struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
}

type attrXML struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// parseTorznab decodes a Prowlarr Torznab response into feed items.
func parseTorznab(body []byte) ([]item, error) {
	var feed feedXML
	if err := xml.Unmarshal(body, &feed); err != nil {
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
			if n, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil {
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
	if size == 0 {
		size = x.Size
	}
	if size == 0 {
		size, _ = strconv.ParseInt(strings.TrimSpace(attrs["size"]), 10, 64)
	}

	seeders := attrInt(attrs, "seeders")
	leechers := attrInt(attrs, "leechers")
	if leechers == 0 {
		if peers := attrInt(attrs, "peers"); peers > seeders {
			leechers = peers - seeders
		}
	}

	return item{
		Title:       strings.TrimSpace(x.Title),
		GUID:        strings.TrimSpace(x.GUID),
		InfoURL:     strings.TrimSpace(x.Comments),
		DownloadURL: strings.TrimSpace(dl),
		InfoHash:    strings.ToLower(strings.TrimSpace(attrs["infohash"])),
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

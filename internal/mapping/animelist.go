package mapping

import (
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/xmlx"
)

// Decode limits for the Anime-Lists anime-list-master.xml body, sized off the
// real file (3.5 MB, 37,837 elements, depth 5, 9 attributes on an <anime> tag,
// longest line 669 bytes) with the headroom a weekly community-edited document
// needs and no more: the body is untrusted upstream XML and every bound here is
// what stops a hostile one from amplifying past its wire size (CWE-400).
const (
	// listMaxTextRunBytes caps one raw text or CDATA run; the longest real row
	// text is under 1 KiB.
	listMaxTextRunBytes = 64 << 10
	// listMaxTokenBytes caps one markup token whole.
	listMaxTokenBytes = 64 << 10
	// listMaxTagAttrs caps XML attributes on one start tag; <anime> carries 9.
	listMaxTagAttrs = 32
	// listMaxDepth caps element nesting as the preflight counts it, root
	// included; the document is depth 5, and every unmodeled child goes through
	// Decoder.Skip, which has no depth bound of its own.
	listMaxDepth = 16
	// listMaxElements caps total elements, the bound on amplification by COUNT.
	listMaxElements = 1 << 20
	// listMaxFieldBytes caps one decoded value (an attribute or a row's text).
	listMaxFieldBytes = 4 << 10
	// listMaxTextBytes caps the cumulative decoded text retained from one body.
	listMaxTextBytes = 8 << 20
)

// listLimits is the xmlx.Limits value the raw-byte preflight runs under.
var listLimits = xmlx.Limits{
	MaxTextRunBytes: listMaxTextRunBytes,
	MaxTokenBytes:   listMaxTokenBytes,
	MaxTagAttrs:     listMaxTagAttrs,
	MaxDepth:        listMaxDepth,
	MaxElements:     listMaxElements,
}

// errListNoNodes rejects a well-formed body carrying no <anime> node: the list
// is never empty, so an empty one is a wrong or truncated document, not a
// mapping with nothing in it.
var errListNoNodes = errors.New("mapping: mapping-list carries no anime nodes")

// errListRoot rejects a document whose root element is not <anime-list>.
var errListRoot = errors.New("mapping: mapping-list root is not anime-list")

// parseMappingList decodes an anime-list-master.xml body into the non-empty
// Mappings it carries, keyed by AniDB id. The raw bytes pass xmlx.Preflight
// first and every retained value is charged to one xmlx.Budget during the
// decode, so a bound breach surfaces as an error wrapping *xmlx.LimitError
// (errors.Is(err, xmlx.ErrLimit)); any other error is a decode error. A node
// whose anidbid is not a positive integer is skipped, and a node the two readers
// (filmEpisode, seasonRanges) find nothing in is not retained.
func parseMappingList(body []byte) (map[int]Mapping, error) {
	if err := xmlx.Preflight(body, listLimits); err != nil {
		return nil, asListLimitError(err)
	}
	list := listXML{mappings: make(map[int]Mapping)}
	if err := xml.Unmarshal(body, &list); err != nil {
		return nil, asListLimitError(err)
	}
	if list.nodes == 0 {
		return nil, errListNoNodes
	}
	return list.mappings, nil
}

// asListLimitError names the mapping-list as the document an xmlx bound refused,
// keeping the *xmlx.LimitError reachable through the chain. Any other error
// passes through untouched.
func asListLimitError(err error) error {
	if le, ok := errors.AsType[*xmlx.LimitError](err); ok {
		return fmt.Errorf("mapping: mapping-list exceeds decode limit: %w", le)
	}
	return err
}

// newListBudget returns the decode-time text budget for ONE body.
func newListBudget() *xmlx.Budget {
	b, err := xmlx.NewBudget(listMaxFieldBytes, listMaxTextBytes)
	if err != nil {
		// Unreachable: both caps are positive constants. A panic here would be a
		// build-time mistake, so fail loudly rather than decoding unbounded.
		panic("mapping: invalid mapping-list decode budget: " + err.Error())
	}
	return b
}

// listXML decodes the <anime-list> root one <anime> at a time, folding each node
// into mappings as it is read so the retained graph is the answer rather than the
// document. The budget is created on the first UnmarshalXML call, so no
// construction path can leave it nil.
type listXML struct {
	budget   *xmlx.Budget
	mappings map[int]Mapping
	// nodes counts every <anime> element decoded, whatever it yielded; the
	// element count the loader logs and the emptiness check reads.
	nodes int
}

// UnmarshalXML walks the root's children: an <anime> is decoded and folded,
// anything else is skipped whole.
func (l *listXML) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local != "anime-list" {
		return errListRoot
	}
	if l.budget == nil {
		l.budget = newListBudget()
	}
	return walkChildren(d, func(t xml.StartElement) error {
		if t.Name.Local != "anime" {
			return d.Skip()
		}
		node := animeXML{budget: l.budget}
		if err := d.DecodeElement(&node, &t); err != nil {
			return err
		}
		l.nodes++
		l.fold(&node)
		return nil
	})
}

// walkChildren iterates the children of the element the decoder is inside,
// handing each start element to onStart (which must consume the element whole,
// through DecodeElement, Skip or DecodeText) and returning at the parent's end
// tag. It is the one token loop the three levels of the document share.
func walkChildren(d *xml.Decoder, onStart func(t xml.StartElement) error) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := onStart(t); err != nil {
				return err
			}
		case xml.EndElement:
			// The first end element at this nesting level closes the parent: every
			// consumer above swallows its nested elements whole.
			return nil
		}
	}
}

// fold reads the two facts off one decoded node and retains them under its
// AniDB id when either is present.
func (l *listXML) fold(node *animeXML) {
	id, err := strconv.Atoi(node.AniDBID)
	if err != nil || id <= 0 {
		return
	}
	m := Mapping{
		SpecialEpisode: filmEpisode(node.DefaultTvdbSeason, node.Rows),
		Seasons:        seasonRanges(node.Rows),
	}
	if m.SpecialEpisode == 0 && len(m.Seasons) == 0 {
		return
	}
	l.mappings[id] = m
}

// animeXML is one <anime> node's decoded facts: the two attributes the readers
// key on and its <mapping-list> rows. The attribute and element names it reads
// are NOT struct tags: it implements UnmarshalXML, so decodeChild owns the child
// vocabulary and the attribute switch below the attribute one.
type animeXML struct {
	budget            *xmlx.Budget
	AniDBID           string
	DefaultTvdbSeason string
	Rows              []rowXML
}

// rowXML is one <mapping> row: its four routing attributes and its text, all as
// the wire strings, so the readers own every parse and its failure mode.
type rowXML struct {
	AniDBSeason string
	TVDBSeason  string
	Start       string
	End         string
	Text        string
}

// UnmarshalXML reads the node's attributes off the start element and walks its
// children: <mapping-list> is decoded row by row, every other child (<name>,
// <supplemental-info>, <before>) is skipped whole.
func (a *animeXML) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for _, at := range start.Attr {
		var dst *string
		switch at.Name.Local {
		case "anidbid":
			dst = &a.AniDBID
		case "defaulttvdbseason":
			dst = &a.DefaultTvdbSeason
		default:
			continue
		}
		if err := a.account(at.Value); err != nil {
			return err
		}
		*dst = at.Value
	}
	return walkChildren(d, func(t xml.StartElement) error {
		if t.Name.Local != "mapping-list" {
			return d.Skip()
		}
		return a.decodeRows(d)
	})
}

// decodeRows decodes a <mapping-list>'s <mapping> children row by row and skips
// any other child.
func (a *animeXML) decodeRows(d *xml.Decoder) error {
	return walkChildren(d, func(t xml.StartElement) error {
		if t.Name.Local != "mapping" {
			return d.Skip()
		}
		return a.decodeRow(d, t)
	})
}

// decodeRow reads one <mapping>'s routing attributes off its start element and
// its text through the bounded decode, charging every retained value first.
func (a *animeXML) decodeRow(d *xml.Decoder, t xml.StartElement) error {
	var row rowXML
	for _, at := range t.Attr {
		var dst *string
		switch at.Name.Local {
		case "anidbseason":
			dst = &row.AniDBSeason
		case "tvdbseason":
			dst = &row.TVDBSeason
		case "start":
			dst = &row.Start
		case "end":
			dst = &row.End
		default:
			continue
		}
		if err := a.account(at.Value); err != nil {
			return err
		}
		*dst = at.Value
	}
	text, err := a.budget.DecodeText(d)
	if err != nil {
		return err
	}
	row.Text = text
	a.Rows = append(a.Rows, row)
	return nil
}

// account charges one retained attribute value against the body-wide budget.
func (a *animeXML) account(s string) error { return a.budget.Charge(s) }

// filmEpisode reads which TVDB season-0 episode a film filed in a series'
// specials IS, and 0 when the node names none. The node gate is load-bearing: a
// series node can carry an identically shaped season-0 row for one of its OWN
// specials, so only a node filing its own title under the parent's specials
// (defaulttvdbseason "0") is read, or a false episode lands on a TV series. The
// rows are the film's own pair-text rows carrying no start (a start marks a range
// row), and every pair must name the same positive episode.
func filmEpisode(defaultSeason string, rows []rowXML) int {
	if defaultSeason != "0" {
		return 0
	}
	episode := 0
	for i := range rows {
		row := &rows[i]
		if row.AniDBSeason != "1" || row.TVDBSeason != "0" || row.Start != "" || strings.TrimSpace(row.Text) == "" {
			continue
		}
		for pair := range strings.SplitSeq(row.Text, ";") {
			if strings.TrimSpace(pair) == "" {
				continue
			}
			target, ok := pairTarget(pair)
			if !ok || (episode != 0 && target != episode) {
				return 0
			}
			episode = target
		}
	}
	return episode
}

// pairTarget parses one "a-b" mapping pair and returns b when both halves are
// plain positive integers. A "+" in the target (one AniDB episode spanning two
// TVDB episodes) and a 0 target are refused along with anything unparseable.
func pairTarget(pair string) (int, bool) {
	source, target, found := strings.Cut(strings.TrimSpace(pair), "-")
	if !found {
		return 0, false
	}
	if _, err := strconv.Atoi(source); err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(target)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// seasonRanges reads which TVDB seasons an absolute-numbered run's episodes fall
// into: every row rangeOf admits, sorted by first episode. When the lowest range
// starts above episode 1 the uncovered leading run is prepended as the season
// below the lowest one named (floor 1): a node whose rows start at season 2 is
// stating that its earlier episodes are TVDB season 1.
func seasonRanges(rows []rowXML) []SeasonRange {
	var out []SeasonRange
	for i := range rows {
		if r, ok := rangeOf(&rows[i]); ok {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.SortFunc(out, func(a, b SeasonRange) int {
		if c := a.First - b.First; c != 0 {
			return c
		}
		return a.Season - b.Season
	})
	lowestSeason := out[0].Season
	for _, r := range out[1:] {
		lowestSeason = min(lowestSeason, r.Season)
	}
	if first := out[0].First; first > 1 {
		out = slices.Insert(out, 0, SeasonRange{Season: max(1, lowestSeason-1), First: 1, Last: first - 1})
	}
	return out
}

// rangeOf reads one row as a TVDB season range, false for every row that is not
// one (a pair-text row, a tmdbseason row, a season-0 row, an unparseable bound).
func rangeOf(row *rowXML) (SeasonRange, bool) {
	if row.AniDBSeason != "1" || row.Start == "" {
		return SeasonRange{}, false
	}
	season, err := strconv.Atoi(row.TVDBSeason)
	if err != nil || season < 1 {
		return SeasonRange{}, false
	}
	first, err := strconv.Atoi(row.Start)
	if err != nil || first < 1 {
		return SeasonRange{}, false
	}
	last := 0
	if row.End != "" {
		if last, err = strconv.Atoi(row.End); err != nil {
			return SeasonRange{}, false
		}
	}
	return SeasonRange{Season: season, First: first, Last: last}, true
}

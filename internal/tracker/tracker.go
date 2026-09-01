// Package tracker is the single home of the canonical SeaDex tracker
// vocabulary: canonical names and accepted aliases, the obtainability class
// (public/private/unknown), the site base URLs, and the fail-closed
// name/host/relative-URL identity gates every consumer keys on - including the
// composed rules over them, tracker.CanonicalName (the label to render a link
// with) and tracker.ClassifyAB (how strongly an untrusted (label, URL) pair
// identifies AnimeBytes; the operator's toggle POLICY over that grade stays in
// internal/filter).
package tracker

import (
	"net/url"
	"strings"

	"github.com/cplieger/urlform"
)

// Canonical tracker names: the Tracker.Name values of the table entries.
// Consumers that branch on a specific tracker compare Lookup's canonical Name
// against these instead of re-spelling alias sets.
const (
	// NameNyaa is the canonical name of the Nyaa tracker.
	NameNyaa = "Nyaa"
	// NameAnimeBytes is the canonical name of the AnimeBytes tracker.
	NameAnimeBytes = "AnimeBytes"
	// NameAnimeTosho is the canonical name of the AnimeTosho tracker.
	NameAnimeTosho = "AnimeTosho"
	// NameRuTracker is the canonical name of the RuTracker tracker.
	NameRuTracker = "RuTracker"
)

// Type is the obtainability class of a release's tracker.
type Type string

const (
	// Public is an openly accessible tracker (Nyaa).
	Public Type = "public"
	// Private is a private tracker requiring membership (AnimeBytes).
	Private Type = "private"
	// Unknown is an unrecognized tracker.
	Unknown Type = "unknown"
)

// Tracker is one entry of the canonical SeaDex tracker table: the single home
// of the tracker vocabulary (canonical name, accepted aliases, public/private
// class, and site base URL) that classification, link building, and feed
// routing all consume, so a tracker addition cannot land in one consumer's
// map and silently miss the others.
type Tracker struct {
	// Name is the canonical tracker name, as SeaDex spells it.
	Name string
	// BaseURL is the tracker's site base URL, used to prefix the relative
	// torrent paths private trackers return into usable links.
	BaseURL string
	// Type is the tracker's obtainability class.
	Type Type
	// aliases are additional accepted spellings; the canonical Name is
	// always accepted case-insensitively and is not repeated here.
	aliases []string
}

// table is the canonical table, limited to the trackers SeaDex actually
// uses (verified against the live API: Nyaa and AB carry ~all entries;
// AnimeTosho and RuTracker are a negligible tail).
var table = []Tracker{
	{Name: NameNyaa, Type: Public, BaseURL: "https://nyaa.si"},
	{Name: NameAnimeBytes, aliases: []string{"ab"}, Type: Private, BaseURL: "https://animebytes.tv"},
	{Name: NameAnimeTosho, Type: Public, BaseURL: "https://animetosho.org"},
	{Name: NameRuTracker, Type: Public, BaseURL: "https://rutracker.org"},
}

// byAlias indexes the table by lowercased canonical name and alias for
// Lookup.
var byAlias = func() map[string]Tracker {
	m := make(map[string]Tracker, len(table)*2)
	for _, t := range table {
		m[urlform.FoldHostASCII(t.Name)] = t
		for _, a := range t.aliases {
			m[urlform.FoldHostASCII(a)] = t
		}
	}
	return m
}()

// Lookup resolves a tracker name or alias (case- and
// whitespace-insensitively) to its canonical table entry, reporting whether
// the tracker is known. An empty or unrecognized name is not found.
func Lookup(name string) (Tracker, bool) {
	// The fold is urlform.FoldHostASCII, not strings.ToLower: full Unicode simple folding has
	// ASCII-PRODUCING mappings (U+0130 -> 'i', U+212A -> 'k'), so a pre-lookup ToLower launders a
	// homograph label into a canonical alias key - the same class LookupByHost's ASCII gate exists
	// to stop, on the axis that reads the same untrusted record. It is the same fold byAlias is
	// keyed by, so every key is its own fold image.
	t, known := byAlias[urlform.FoldHostASCII(strings.TrimSpace(name))]
	return t, known
}

// CanonicalName resolves the name to LABEL a link with, for a human-facing
// surface that renders a tracker page URL (the daemon's alert attributes, the
// season report's links cell).
func CanonicalName(label, rawURL string) string {
	host := urlform.Classify(rawURL).Host
	if t, known := LookupByHost(host); known {
		return t.Name
	}
	if t, known := Lookup(label); known {
		return t.Name
	}
	return host
}

// CanonicalSourceURL returns the canonical ABSOLUTE value to parse a tracker
// URL's own components out of - the torrent-page route and the numeric id in
// it - for a form the caller has ALREADY vouched as the tracker's own.
//
// SeaDex spells the same tracker page two ways.
func CanonicalSourceURL(f *urlform.Form) string {
	if f.Class == urlform.ClassSchemelessHost {
		return "https://" + f.Trimmed
	}
	return f.Trimmed
}

// TypeOf maps a tracker name to its obtainability class via the canonical
// tracker table (Lookup). An unrecognized name is Unknown, never silently
// treated as obtainable.
func TypeOf(name string) Type {
	t, ok := Lookup(name)
	if !ok {
		return Unknown
	}
	return t.Type
}

// IsAnimeBytes reports whether the tracker name is AnimeBytes (SeaDex uses
// the literal "AB"; the alias set is the table's, so a spelling accepted for
// the obtainability class is accepted here too). It is the ONE home of that
// question for every consumer of the AnimeBytes toggle.
func IsAnimeBytes(name string) bool {
	t, ok := Lookup(name)
	return ok && t.Name == NameAnimeBytes
}

// IsAnimeBytesHost reports whether a URL host (case-insensitively; one
// DNS-root trailing dot tolerated) is the AnimeBytes site host or a real
// dot-delimited subdomain of it, via the canonical table (LookupByHost). It
// is the URL-host twin of IsAnimeBytes (the tracker-label question), for
// consumers that must key on the URL rather than the untrusted label.
func IsAnimeBytesHost(host string) bool {
	t, ok := LookupByHost(host)
	return ok && t.Name == NameAnimeBytes
}

// Host returns the tracker's canonical lowercased site hostname, derived
// from BaseURL. It is "" when BaseURL does not parse to a hostname, so a
// malformed table entry fails closed for every consumer.
func (t Tracker) Host() string {
	u, err := url.Parse(t.BaseURL)
	if err != nil {
		return ""
	}
	return urlform.FoldHostASCII(u.Hostname())
}

// byHost indexes the table by canonical lowercased site hostname
// (derived from BaseURL, so the table stays the single home of the hosts).
// An entry whose BaseURL does not parse to a hostname is omitted, so a
// malformed table entry fails closed instead of matching arbitrary hosts.
var byHost = func() map[string]Tracker {
	m := make(map[string]Tracker, len(table))
	for _, t := range table {
		if h := t.Host(); h != "" {
			m[h] = t
		}
	}
	return m
}()

// LookupByHost resolves a URL hostname (case-insensitively; one
// DNS-root trailing dot tolerated) to the tracker whose canonical site host
// it equals or is a real dot-delimited subdomain of, reporting whether one
// matched.
func LookupByHost(host string) (Tracker, bool) {
	// Gate on the RAW UNTRIMMED host before any Unicode transform: ToLower and
	// TrimSpace are both full-Unicode and can launder a homograph (U+0130,
	// U+212A) or NBSP/ideographic-space padding past this byte-wise ASCII
	// check. Incidental ASCII padding still passes and is trimmed after.
	if !urlform.IsASCIIHost(host) {
		return Tracker{}, false
	}
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return Tracker{}, false
	}
	// FoldHostASCII rather than ToLower: agrees with the gate above today, but
	// cannot launder if the ordering ever moves.
	host = urlform.FoldHostASCII(host)
	// Most specific match wins, so the result cannot depend on Go's
	// randomized map iteration order once the table holds a host that is
	// a subdomain of another (sukebei.nyaa.si beside nyaa.si).
	var best Tracker
	bestLen := 0
	for canonical, t := range byHost {
		if len(canonical) > bestLen && urlform.HostMatchesDomain(host, canonical) {
			best, bestLen = t, len(canonical)
		}
	}
	return best, bestLen > 0
}

// LookupByRelativeURL resolves tracker-specific relative page shapes
// to their owning tracker.
func LookupByRelativeURL(raw string) (Tracker, bool) {
	f := urlform.Classify(raw)
	rooted, ok := hrefPath(&f)
	if !ok {
		return Tracker{}, false
	}
	u, err := url.Parse(rooted)
	if err != nil || !urlform.EqualASCIIFold(u.Path, "/torrents.php") || !rawQueryHasKeyFold(u.RawQuery, "torrentid") {
		return Tracker{}, false
	}
	return Lookup(NameAnimeBytes)
}

// hrefPath returns the rooted path an href-context consumer resolves a
// host-less value against, reporting whether the form has one at all.
func hrefPath(f *urlform.Form) (string, bool) {
	switch f.Class {
	case urlform.ClassRelative:
		return hrefSlashes(f.Trimmed), true
	case urlform.ClassSchemelessHost:
		return "/" + hrefSlashes(f.Trimmed), true
	default:
		return "", false
	}
}

// hrefSlashes applies the WHATWG backslash-is-a-slash reading to a host-less
// value's path, the same canonicalization urlform.Classify used to decide the
// Class.
func hrefSlashes(trimmed string) string {
	stop := strings.IndexAny(trimmed, "?#")
	if stop < 0 {
		stop = len(trimmed)
	}
	if !strings.Contains(trimmed[:stop], `\`) {
		return trimmed
	}
	return strings.ReplaceAll(trimmed[:stop], `\`, "/") + trimmed[stop:]
}

// rawQueryHasKeyFold reports whether the RAW query carries key under ASCII
// case folding. The raw reading (urlform.RawQueryNames: split on both '&' and
// ';', percent-decode each name) is a strict superset of the parsed u.Query()
// view, which drops a malformed pair wholesale - so a semicolon-smuggled pair
// ("?torrentid=1;x") cannot evade the AB torrent-page shape check.
func rawQueryHasKeyFold(rawQuery, key string) bool {
	for name := range urlform.RawQueryNames(rawQuery) {
		if urlform.EqualASCIIFold(name, key) {
			return true
		}
	}
	return false
}

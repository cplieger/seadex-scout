// Package tracker is the single home of the canonical SeaDex tracker
// vocabulary: canonical names and accepted aliases, the obtainability class
// (public/private/unknown), the site base URLs, and the fail-closed
// name/host/relative-URL identity gates every consumer keys on. A tracker
// addition lands here and nowhere else, so it cannot reach one consumer's map
// and silently miss the others.
//
// It is a leaf over urlform: it knows tracker identity and the URL SHAPES that
// carry it, and nothing about release names, findings, or links. Its consumers
// split cleanly along that line — internal/release reads only the obtainability
// class for a Release fingerprint, internal/trackerlink applies the publish
// policy over the table's base URLs, and internal/filter, internal/notify,
// internal/compare and internal/indexer read the identity gates. Keeping the
// vocabulary out of internal/release is what lets the pure classifier stay free
// of URL parsing and lets trackerlink reach the table without importing the
// whole classification engine.
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
		m[strings.ToLower(t.Name)] = t
		for _, a := range t.aliases {
			m[strings.ToLower(a)] = t
		}
	}
	return m
}()

// Lookup resolves a tracker name or alias (case- and
// whitespace-insensitively) to its canonical table entry, reporting whether
// the tracker is known. An empty or unrecognized name is not found.
func Lookup(name string) (Tracker, bool) {
	t, ok := byAlias[strings.ToLower(strings.TrimSpace(name))]
	return t, ok
}

// CanonicalName resolves the name to LABEL a link with, for a human-facing
// surface that renders a tracker page URL (the daemon's alert attributes, the
// season report's links cell). It is the composed rule over the two identity
// gates this package already owns, and it lives here for the same reason they
// do: a table edit (an added alias, a rename, a fifth tracker) must reach
// every renderer at once.
//
// The URL host is the primary evidence because it is what the reader will
// actually navigate to; the untrusted SeaDex tracker label (an alias, oddly
// cased, or empty) is the fallback for a host-less or unrecognized-host link,
// and the bare host labels a link no table entry claims. Only a link with
// neither a known host, a known label, nor any host at all yields "" - a
// caller that must always print something supplies its own last-resort word.
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

// IsNyaaHost reports whether a URL host (case-insensitively; one DNS-root
// trailing dot tolerated) is the Nyaa site host or a real dot-delimited
// subdomain of it, via the canonical table (LookupByHost), mirroring
// IsAnimeBytesHost so the tracker-identity questions share one home.
func IsNyaaHost(host string) bool {
	t, ok := LookupByHost(host)
	return ok && t.Name == NameNyaa
}

// Host returns the tracker's canonical lowercased site hostname, derived
// from BaseURL. It is "" when BaseURL does not parse to a hostname, so a
// malformed table entry fails closed for every consumer.
func (t Tracker) Host() string {
	u, err := url.Parse(t.BaseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
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
// matched. The tracker label is untrusted upstream data, so consumers that
// validate an absolute URL's host key on this evidence instead; an empty or
// unknown host matches nothing, and neither a suffix-confusion host
// ("evilnyaa.si") nor a parent-domain spoof ("nyaa.si.evil.example")
// survives the dot-delimited comparison. Two further fail-closed rules live
// here so every consumer inherits them: a non-ASCII host never matches (see
// urlform.IsASCIIHost - homograph territory), and an empty-labeled host (".nyaa.si",
// "a..nyaa.si") is not a subdomain - no DNS name has an empty label, so only
// a non-empty label chain counts (see urlform.HostMatchesDomain). When two
// canonical hosts both match (a table entry that is a subdomain of another),
// the most specific one wins.
func LookupByHost(host string) (Tracker, bool) {
	// The ASCII gate runs on the RAW UNTRIMMED host, BEFORE any Unicode
	// transform: BOTH strings.ToLower and strings.TrimSpace are full-Unicode
	// operations that can launder non-ASCII runes past the fail-closed
	// non-ASCII rule - ToLower's few ASCII-producing fold mappings
	// (U+0130 -> 'i', U+212A KELVIN SIGN -> 'k') would launder a homograph
	// host ("an\u0130mebytes.tv"), and TrimSpace's unicode.IsSpace trim
	// (U+00A0 NBSP, U+3000 ideographic space) would launder a
	// whitespace-decorated host ("nyaa.si\u00a0"). IsASCIIHost is byte-wise,
	// so a host with incidental ASCII space/tab padding still passes it and
	// is trimmed after; trimming or folding an ASCII-verified string is a
	// pure ASCII operation, so legitimate hosts are unaffected.
	if !urlform.IsASCIIHost(host) {
		return Tracker{}, false
	}
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return Tracker{}, false
	}
	host = strings.ToLower(host)
	// Most specific match wins, so the result cannot depend on Go's
	// randomized map iteration order once the table holds a host that is
	// a subdomain of another (sukebei.nyaa.si beside nyaa.si). Every
	// canonical key is non-empty by construction (byHost omits an
	// entry whose BaseURL yields no hostname), so bestLen > 0 is exactly
	// "something matched".
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
// to their owning tracker. SeaDex publishes AnimeBytes pages in the
// documented relative form "/torrents.php?...&torrentid=..."; that shape
// carries tracker identity even though the URL has no host, so consumers
// that would otherwise fall back to the untrusted tracker label (the
// AB-toggle visibility gate, the usable-link canonicalizer) key on this
// structural evidence instead. A slashless value ("torrents.php?...") is read
// as that same path rooted, the href reading the link publisher resolves it to
// (see hrefPath); any other shape matches nothing.
func LookupByRelativeURL(raw string) (Tracker, bool) {
	f := urlform.Classify(raw)
	rooted, ok := hrefPath(&f)
	if !ok {
		return Tracker{}, false
	}
	u, err := url.Parse(rooted)
	if err != nil || !equalASCIIFold(u.Path, "/torrents.php") || !rawQueryHasKeyFold(u.RawQuery, "torrentid") {
		return Tracker{}, false
	}
	return Lookup(NameAnimeBytes)
}

// hrefPath returns the rooted path an href-context consumer resolves a
// host-less value against, reporting whether the form has one at all. A
// rooted relative value ("/torrents.php?...") is already it; a slashless
// value ("torrents.php?...") classifies schemeless-host (net/url reads a
// bare path while an address bar would read a host), and its href reading is
// the same path rooted - which is exactly how the link publisher resolves it
// (trackerlink.usableSchemelessHost). Rooting here keeps the shape rule
// single-homed: every consumer of LookupByRelativeURL reads both
// spellings identically, so a mislabeled slashless AB torrent-page URL
// cannot publish as an animebytes.tv link while the AB gates read it as
// non-AB. Every other form (absolute, protocol-relative, hidden-host,
// malformed, empty) carries no host-less path and matches nothing - tracker
// identity for those comes from the host gate.
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
// Class. Form.Trimmed is the preprocessed but NOT slash-canonicalized string,
// so a rooted value spelled with a leading backslash ("\torrents.php?...")
// reaches the shape rule as a path net/url reads verbatim while a browser
// resolves it as "/torrents.php?...". Canonicalizing here keeps the shape rule
// reading what a browser reads, so a smuggled AB torrent-page URL cannot grade
// as non-AnimeBytes evidence. Only the pre-query/fragment part is rewritten
// (past the first '?' or '#' a backslash is an ordinary character, urlform's
// own rule).
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

// equalASCIIFold reports whether a and b are equal under ASCII case folding.
// Non-ASCII bytes can never compare equal to an ASCII protocol token.
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// rawQueryHasKeyFold reports whether the RAW query carries key under ASCII
// case folding. The raw reading (urlform.RawQueryNames: split on both '&' and
// ';', percent-decode each name) is a strict superset of the parsed u.Query()
// view, which drops a malformed pair wholesale - so a semicolon-smuggled pair
// ("?torrentid=1;x") cannot evade the AB torrent-page shape check. The fold
// stays here rather than in the library because the two consumers of that walk
// need opposite fail directions: this gate matches only the one name it
// recognizes, while internal/config's credential warning matches broadly.
func rawQueryHasKeyFold(rawQuery, key string) bool {
	for name := range urlform.RawQueryNames(rawQuery) {
		if equalASCIIFold(name, key) {
			return true
		}
	}
	return false
}

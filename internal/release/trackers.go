package release

import (
	"net/url"
	"strings"

	"github.com/cplieger/urlform"
)

// Canonical tracker names: the Tracker.Name values of the table entries.
// Consumers that branch on a specific tracker compare LookupTracker's
// canonical Name against these instead of re-spelling alias sets.
const (
	// TrackerNameNyaa is the canonical name of the Nyaa tracker.
	TrackerNameNyaa = "Nyaa"
	// TrackerNameAnimeBytes is the canonical name of the AnimeBytes tracker.
	TrackerNameAnimeBytes = "AnimeBytes"
	// TrackerNameAnimeTosho is the canonical name of the AnimeTosho tracker.
	TrackerNameAnimeTosho = "AnimeTosho"
	// TrackerNameRuTracker is the canonical name of the RuTracker tracker.
	TrackerNameRuTracker = "RuTracker"
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
	Type TrackerType
	// aliases are additional accepted spellings; the canonical Name is
	// always accepted case-insensitively and is not repeated here.
	aliases []string
}

// trackerTable is the canonical table, limited to the trackers SeaDex actually
// uses (verified against the live API: Nyaa and AB carry ~all entries;
// AnimeTosho and RuTracker are a negligible tail).
var trackerTable = []Tracker{
	{Name: TrackerNameNyaa, Type: TrackerPublic, BaseURL: "https://nyaa.si"},
	{Name: TrackerNameAnimeBytes, aliases: []string{"ab"}, Type: TrackerPrivate, BaseURL: "https://animebytes.tv"},
	{Name: TrackerNameAnimeTosho, Type: TrackerPublic, BaseURL: "https://animetosho.org"},
	{Name: TrackerNameRuTracker, Type: TrackerPublic, BaseURL: "https://rutracker.org"},
}

// trackerByAlias indexes the table by lowercased canonical name and alias for
// LookupTracker.
var trackerByAlias = func() map[string]Tracker {
	m := make(map[string]Tracker, len(trackerTable)*2)
	for _, t := range trackerTable {
		m[strings.ToLower(t.Name)] = t
		for _, a := range t.aliases {
			m[strings.ToLower(a)] = t
		}
	}
	return m
}()

// LookupTracker resolves a tracker name or alias (case- and
// whitespace-insensitively) to its canonical table entry, reporting whether
// the tracker is known. An empty or unrecognized name is not found.
func LookupTracker(name string) (Tracker, bool) {
	t, ok := trackerByAlias[strings.ToLower(strings.TrimSpace(name))]
	return t, ok
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

// trackerByHost indexes the table by canonical lowercased site hostname
// (derived from BaseURL, so the table stays the single home of the hosts).
// An entry whose BaseURL does not parse to a hostname is omitted, so a
// malformed table entry fails closed instead of matching arbitrary hosts.
var trackerByHost = func() map[string]Tracker {
	m := make(map[string]Tracker, len(trackerTable))
	for _, t := range trackerTable {
		if h := t.Host(); h != "" {
			m[h] = t
		}
	}
	return m
}()

// LookupTrackerByHost resolves a URL hostname (case-insensitively; one
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
func LookupTrackerByHost(host string) (Tracker, bool) {
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
	// canonical key is non-empty by construction (trackerByHost omits an
	// entry whose BaseURL yields no hostname), so bestLen > 0 is exactly
	// "something matched".
	var best Tracker
	bestLen := 0
	for canonical, t := range trackerByHost {
		if len(canonical) > bestLen && urlform.HostMatchesDomain(host, canonical) {
			best, bestLen = t, len(canonical)
		}
	}
	return best, bestLen > 0
}

// LookupTrackerByRelativeURL resolves tracker-specific relative page shapes
// to their owning tracker. SeaDex publishes AnimeBytes pages in the
// documented relative form "/torrents.php?...&torrentid=..."; that shape
// carries tracker identity even though the URL has no host, so consumers
// that would otherwise fall back to the untrusted tracker label (the
// AB-toggle visibility gate, the usable-link canonicalizer) key on this
// structural evidence instead. A slashless value ("torrents.php?...") is read
// as that same path rooted, the href reading the link publisher resolves it to
// (see hrefPath); any other shape matches nothing.
func LookupTrackerByRelativeURL(raw string) (Tracker, bool) {
	f := urlform.Classify(raw)
	rooted, ok := hrefPath(&f)
	if !ok {
		return Tracker{}, false
	}
	u, err := url.Parse(rooted)
	if err != nil || !equalASCIIFold(u.Path, "/torrents.php") || !rawQueryHasKeyFold(u.RawQuery, "torrentid") {
		return Tracker{}, false
	}
	return LookupTracker(TrackerNameAnimeBytes)
}

// hrefPath returns the rooted path an href-context consumer resolves a
// host-less value against, reporting whether the form has one at all. A
// rooted relative value ("/torrents.php?...") is already it; a slashless
// value ("torrents.php?...") classifies schemeless-host (net/url reads a
// bare path while an address bar would read a host), and its href reading is
// the same path rooted - which is exactly how the link publisher resolves it
// (seadex.usableSchemelessHost). Rooting here keeps the shape rule
// single-homed: every consumer of LookupTrackerByRelativeURL reads both
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

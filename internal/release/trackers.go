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

// trackerByHost indexes the table by canonical lowercased site hostname
// (derived from BaseURL, so the table stays the single home of the hosts).
// An entry whose BaseURL does not parse to a hostname is omitted, so a
// malformed table entry fails closed instead of matching arbitrary hosts.
var trackerByHost = func() map[string]Tracker {
	m := make(map[string]Tracker, len(trackerTable))
	for _, t := range trackerTable {
		if u, err := url.Parse(t.BaseURL); err == nil && u.Hostname() != "" {
			m[strings.ToLower(u.Hostname())] = t
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
// a non-empty label chain counts (see hostMatchesDomain).
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
	for canonical, t := range trackerByHost {
		if hostMatchesDomain(host, canonical) {
			return t, true
		}
	}
	return Tracker{}, false
}

// LookupTrackerByRelativeURL resolves tracker-specific relative page shapes
// to their owning tracker. SeaDex publishes AnimeBytes pages in the
// documented relative form "/torrents.php?...&torrentid=..."; that shape
// carries tracker identity even though the URL has no host, so consumers
// that would otherwise fall back to the untrusted tracker label (the
// AB-toggle visibility gate, the usable-link canonicalizer) key on this
// structural evidence instead. A non-relative or unrecognized shape matches
// nothing.
func LookupTrackerByRelativeURL(raw string) (Tracker, bool) {
	f := urlform.Classify(raw)
	if f.Class != urlform.ClassRelative {
		return Tracker{}, false
	}
	u, err := url.Parse(f.Trimmed)
	if err != nil || !strings.EqualFold(u.Path, "/torrents.php") || !hasQueryKeyFold(u.Query(), "torrentid") {
		return Tracker{}, false
	}
	return LookupTracker(TrackerNameAnimeBytes)
}

// hasQueryKeyFold reports whether the parsed query carries key under ASCII
// case folding, mirroring the path comparison's case tolerance so the same
// evasion (an off-case spelling of the AB torrent-page shape) cannot slip
// one half of the shape check while the other half tolerates it.
func hasQueryKeyFold(q url.Values, key string) bool {
	for k := range q {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// hostMatchesDomain reports whether host equals domain or is a real
// dot-delimited subdomain of it: host must end in "."+domain and every label
// of the subdomain prefix must be non-empty. Plain suffix matching would also
// accept empty DNS labels (".nyaa.si" via its leading dot, "a..nyaa.si" via
// the inner one); no resolvable DNS name carries an empty label, so those
// forms are adversarial and must not classify as the tracker.
func hostMatchesDomain(host, domain string) bool {
	if host == domain {
		return true
	}
	prefix, ok := strings.CutSuffix(host, "."+domain)
	if !ok {
		return false
	}
	for label := range strings.SplitSeq(prefix, ".") {
		if label == "" {
			return false
		}
	}
	return true
}

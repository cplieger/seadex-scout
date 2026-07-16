package release

import (
	"net/url"
	"strings"
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
// it equals or is a dot-delimited subdomain of, reporting whether one
// matched. The tracker label is untrusted upstream data, so consumers that
// validate an absolute URL's host key on this evidence instead; an empty or
// unknown host matches nothing, and neither a suffix-confusion host
// ("evilnyaa.si") nor a parent-domain spoof ("nyaa.si.evil.example")
// survives the dot-delimited comparison.
func LookupTrackerByHost(host string) (Tracker, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return Tracker{}, false
	}
	for canonical, t := range trackerByHost {
		if host == canonical || strings.HasSuffix(host, "."+canonical) {
			return t, true
		}
	}
	return Tracker{}, false
}

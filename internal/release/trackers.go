package release

import "strings"

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
	// Aliases are additional accepted spellings (lowercased); the canonical
	// Name is always accepted case-insensitively and is not repeated here.
	Aliases []string
}

// trackerTable is the canonical table, limited to the trackers SeaDex actually
// uses (verified against the live API: Nyaa and AB carry ~all entries;
// AnimeTosho and RuTracker are a negligible tail).
var trackerTable = []Tracker{
	{Name: TrackerNameNyaa, Type: TrackerPublic, BaseURL: "https://nyaa.si"},
	{Name: TrackerNameAnimeBytes, Aliases: []string{"ab"}, Type: TrackerPrivate, BaseURL: "https://animebytes.tv"},
	{Name: TrackerNameAnimeTosho, Type: TrackerPublic, BaseURL: "https://animetosho.org"},
	{Name: TrackerNameRuTracker, Type: TrackerPublic, BaseURL: "https://rutracker.org"},
}

// trackerByAlias indexes the table by lowercased canonical name and alias for
// LookupTracker.
var trackerByAlias = func() map[string]Tracker {
	m := make(map[string]Tracker, len(trackerTable)*2)
	for _, t := range trackerTable {
		m[strings.ToLower(t.Name)] = t
		for _, a := range t.Aliases {
			m[a] = t
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

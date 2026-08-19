package indexer

import (
	"regexp"
	"strconv"
	"testing"
)

// abPasskeyFuzzSentinel is a passkey no tracker id, host or route text can
// contain, so an assertion about where it may appear cannot pass by accident.
// It carries only unreserved bytes, so url.PathEscape leaves it unchanged.
const abPasskeyFuzzSentinel = "pk-SECRET-Zq7"

// The only two download forms downloadURLForScope may ever produce. They are an
// INDEPENDENT oracle over the whole returned string, never a second copy of the
// builder: the host comes from the canonical tracker table and the id from
// validTrackerID, so a link matching neither form means the untrusted source URL
// reached the host or the route - and on the Nyaa arm, that the operator's
// AnimeBytes passkey could ride a public-tracker link.
var (
	nyaaDownloadForm = regexp.MustCompile(
		`^https://nyaa\.si/download/[0-9]{1,` + strconv.Itoa(maxTrackerIDDigits) + `}\.torrent$`,
	)
	abDownloadForm = regexp.MustCompile(
		`^https://animebytes\.tv/torrent/[0-9]{1,` + strconv.Itoa(maxTrackerIDDigits) +
			`}/download/` + regexp.QuoteMeta(abPasskeyFuzzSentinel) + `$`,
	)
)

// FuzzDownloadURL_staysOnTheCanonicalTrackerRoute exercises the download-link
// builder on untrusted SeaDex record fields (a tracker label and a source URL a
// curator types freely). Invariants: it never panics, a refusal yields the empty
// string, and any link it DOES return is exactly one of the two canonical
// tracker download forms - so no source URL can steer the link off the tracker's
// own host, and the AnimeBytes passkey (a secret, handed to the arr and recorded
// in its grab history) can only ever appear in the AnimeBytes form.
func FuzzDownloadURL_staysOnTheCanonicalTrackerRoute(f *testing.F) {
	f.Add("Nyaa", "https://nyaa.si/view/1234567")
	f.Add("AB", "/torrents.php?id=1&torrentid=1167293")
	f.Add("AB", "https://animebytes.tv/torrent/1167293/group")
	f.Add("AB", "animebytes.tv/torrents.php?id=1&torrentid=456")
	f.Add("Nyaa", "https://evil.example/view/123")
	f.Add("Nyaa", "https://sukebei.nyaa.si/view/123")
	f.Add("AB", "evil@animebytes.tv/torrents.php?torrentid=456")
	f.Add("Nyaa", "\thttps://nyaa.si/view/123")
	f.Add("AnimeTosho", "https://animetosho.org/view/1")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, trackerName, sourceURL string) {
		got, ok := downloadURLForScope(trackerScope(trackerName), sourceURL, abPasskeyFuzzSentinel)
		if !ok {
			if got != "" {
				t.Fatalf("downloadURLForScope(%q, %q) = %q with ok=false, want the empty string", trackerName, sourceURL, got)
			}
			return
		}
		if !nyaaDownloadForm.MatchString(got) && !abDownloadForm.MatchString(got) {
			t.Fatalf("downloadURLForScope(%q, %q) = %q, want a canonical Nyaa or AnimeBytes download link", trackerName, sourceURL, got)
		}
	})
}

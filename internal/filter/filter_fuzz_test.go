package filter

import (
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
)

// abLabel shapes fuzz input into a guaranteed-valid DNS label (letters and
// digits only, never empty) so constructed hosts stay hosts instead of
// sprouting path/userinfo separators that would move the AnimeBytes suffix
// out of the host position - and so the subdomain invariant below never
// constructs an empty-labeled host (".animebytes.tv"), which the shared
// tracker predicate deliberately does not classify as AnimeBytes (no
// resolvable DNS name has an empty label).
func abLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "sub"
	}
	return b.String()
}

// FuzzABVisible drives the AnimeBytes visibility gate with untrusted tracker
// labels and upstream URLs: with the toggle on nothing hides; with it off an
// AB-labeled or AB-hosted release never surfaces and a lookalike host is never
// hidden as AnimeBytes.
func FuzzABVisible(f *testing.F) {
	f.Add("Nyaa", "https://nyaa.si/view/1")
	f.Add("AB", "/torrents.php?id=1&torrentid=2")
	f.Add("animebytes", "")
	f.Add("Nyaa", "https://animebytes.tv/torrents.php?id=1")
	f.Add("Nyaa", "https://ANIMEBYTES.TV/t/1")
	f.Add("Nyaa", "https://cdn.animebytes.tv/t/1")
	f.Add("Nyaa", "https://notanimebytes.tv/t/1")
	f.Add("Nyaa", "https://animebytes.tv.evil.example/t/1")
	f.Add("Nyaa", "https://user@animebytes.tv/t/1")
	f.Add("Nyaa", "https://animebytes.tv@nyaa.si/t/1")
	f.Add("Nyaa", "https://nyaa.si/\x7f")
	f.Add("Nyaa", `a\b@animebytes.tv/x`)
	f.Add("Nyaa", `/\animebytes.tv/x`)
	f.Add("Nyaa", `\\animebytes.tv/x`)
	f.Add("Nyaa", "https://animebytes\uFF0Etv/torrents.php?id=1")
	f.Add("unknown", "/local/path")
	f.Fuzz(func(t *testing.T, tracker, rawURL string) {
		// Toggle on shows everything: the operator has AB access, nothing hides.
		if !ABVisible(tracker, rawURL, true) {
			t.Errorf("ABVisible(%q, %q, true) = false, want true", tracker, rawURL)
		}
		off := ABVisible(tracker, rawURL, false)
		// An AB-labeled tracker is always hidden when the toggle is off,
		// whatever the URL says (cross-function consistency with
		// release.IsAnimeBytes).
		if release.IsAnimeBytes(tracker) && off {
			t.Errorf("ABVisible(%q, %q, false) = true, want false for an AB label", tracker, rawURL)
		}
		// Metamorphic: production trims the URL, so whitespace padding must not
		// change the verdict (a padded AB URL must not slip past the gate).
		if padded := ABVisible(tracker, " "+rawURL+"\t", false); padded != off {
			t.Errorf("ABVisible(%q, padded, false) = %v, want %v (url %q)", tracker, padded, off, rawURL)
		}
		// Cross-function consistency: the fail-open predicate is a subset of
		// the fail-closed gate. Anything DEFINITELY AnimeBytes must also be
		// AB-gated (hidden with the toggle off); the converse is deliberately
		// free - the gate also hides malformed, ambiguous, and non-ASCII
		// evidence the fail-open predicate cannot prove is AnimeBytes.
		if DefinitelyAB(tracker, rawURL) && !ABGated(tracker, rawURL) {
			t.Errorf("DefinitelyAB(%q, %q) = true but ABGated = false; the fail-open set must stay inside the fail-closed gate", tracker, rawURL)
		}
		// Security: no fuzzer-built subdomain of the AB host may surface while
		// the toggle is off, and a lookalike suffix host must not be hidden as
		// AB. Built from generated input, not by re-running the parser.
		label := abLabel(rawURL)
		if ABVisible("Nyaa", "https://"+label+".animebytes.tv/x", false) {
			t.Errorf("subdomain %q.animebytes.tv surfaced with the toggle off", label)
		}
		if !ABVisible("Nyaa", "https://"+label+"animebytes.tv.example/x", false) {
			t.Errorf("lookalike host %sanimebytes.tv.example was hidden as AnimeBytes", label)
		}
	})
}

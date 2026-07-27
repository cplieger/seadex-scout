package filter

import (
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/release"
	"github.com/cplieger/seadex-scout/internal/trackerlink"
	"github.com/cplieger/urlform"
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
	f.Add("Nyaa", "torrents.php?id=1&torrentid=2")
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
		// Cross-function consistency: ABVisible must be exactly the grade
		// comparison, with no second reading of the evidence. The old
		// definite-is-a-subset-of-gated property is structural now (one value
		// cannot be two grades), so what is worth fuzzing is that the policy
		// function and the grader never disagree.
		if want := ClassifyAB(tracker, rawURL) == ABNone; off != want {
			t.Errorf("ABVisible(%q, %q, false) = %v but ClassifyAB = %v; the gate must be exactly the ABNone comparison", tracker, rawURL, off, ClassifyAB(tracker, rawURL))
		}
		// Totality: every input lands in one of the three named grades, so an
		// exhaustive consumer switch (notify.classifyTrackerLink) cannot fall
		// through to its unreachable default.
		switch g := ClassifyAB(tracker, rawURL); g {
		case ABNone, ABAmbiguous, ABDefinite:
		default:
			t.Errorf("ClassifyAB(%q, %q) = %v, outside the three named grades", tracker, rawURL, g)
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

// FuzzABToggleNeverPublishesAnimeBytes pins the COMPOSED toggle invariant this
// package owns only half of: with the operator's animebytes toggle off, no
// release the daemon's obtainability gate admits - and no row the audit report
// keeps (its gate is ClassifyAB != ABDefinite) - may carry a published
// animebytes.tv link. The hide half lives here (ClassifyAB/ABVisible) and the
// publish half in trackerlink.Publish; the two agree today only because every
// publish path that can emit an AnimeBytes base (an AB label, an AB URL host,
// the AB torrent-page relative shape) is also a grade-ABDefinite path. Nothing
// asserted that, so a change to either ladder could open a leak silently.
func FuzzABToggleNeverPublishesAnimeBytes(f *testing.F) {
	for _, seed := range [][2]string{
		{"Nyaa", "https://nyaa.si/view/1"},
		{"Nyaa", "/torrents.php?id=1&torrentid=2"},
		{"Nyaa", "torrents.php?id=1&torrentid=2"},
		{"Nyaa", "/TORRENTS.PHP?TORRENTID=2"},
		{"Nyaa", "https://animebytes.tv/torrents.php?id=1&torrentid=2"},
		{"Nyaa", "http://animebytes.tv/t/1"},
		{"Nyaa", "animebytes.tv/torrents.php?id=1&torrentid=2"},
		{"Nyaa", "https:/animebytes.tv/t/1"},
		{"Nyaa", "https:animebytes.tv/t/1"},
		{"Nyaa", "animebytes.tv:443/t/1"},
		{"Nyaa", "/animebytes.tv/torrents.php?torrentid=2"},
		{"Nyaa", "https://cdn.animebytes.tv/t/1"},
		{"AB", "Chihiro"},
		{"unknown", "/torrents.php?id=1&torrentid=2"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, tracker, rawURL string) {
		published := trackerlink.Publish(tracker, rawURL)
		if !release.IsAnimeBytesHost(urlform.Classify(published).Host) {
			return
		}
		// The daemon direction (fail closed): an obtainable release with the
		// toggle off must never resolve to an AnimeBytes link.
		rel := release.Classify(&release.Input{Tracker: tracker})
		if Obtainable(&rel, rawURL, published, false) {
			t.Errorf("Obtainable(%q, %q, toggle off) = true but publishes AnimeBytes link %q", tracker, rawURL, published)
		}
		// The report direction (fail open on listing, but never on identity):
		// a row the audit keeps with the toggle off must not carry an
		// AnimeBytes link, so an AB-publishing pair must grade ABDefinite.
		if g := ClassifyAB(tracker, rawURL); g != ABDefinite {
			t.Errorf("ClassifyAB(%q, %q) = %v but the pair publishes AnimeBytes link %q; the audit report would keep the row", tracker, rawURL, g, published)
		}
	})
}

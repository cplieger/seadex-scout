package notify

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
)

func TestDedupeKey(t *testing.T) {
	h1 := strings.Repeat("a", 40)
	h2 := strings.Repeat("b", 40)
	f := &compare.Finding{
		AniListID:         42,
		Status:            compare.StatusBetter,
		RecommendedGroups: []string{"b", "a"},
		CurrentGroup:      "x",
		InfoHash:          h1,
	}
	got := dedupeKey(f)
	want := `42|better_release|a,b|x|hash:` + h1
	if got != want {
		t.Errorf("dedupeKey() = %q, want %q", got, want)
	}

	swap := *f
	swap.InfoHash = h2
	if dedupeKey(&swap) == got {
		t.Error("a new infoHash (same-group quality swap) must produce a different dedupe key")
	}

	// The InfoHash is untrusted SeaDex data: anything that is not a valid
	// 40-hex hash (garbage, a truncated value, the AB redaction marker) must
	// not become the identity - the finding keys on its release URL instead,
	// domain-tagged so the two sources can never alias.
	garbage := *f
	garbage.InfoHash = "hash1"
	garbage.ReleaseURL = "https://nyaa.si/view/7"
	if k := dedupeKey(&garbage); !strings.Contains(k, "|url:https://nyaa.si/view/7") {
		t.Errorf("invalid-hash dedupeKey() = %q, want the url: fallback identity", k)
	}
	bareHexURL := *f
	bareHexURL.InfoHash = ""
	bareHexURL.ReleaseURL = h1
	if dedupeKey(&bareHexURL) == got {
		t.Error("a release URL spelling a valid hash must not alias the hash identity (domain separation)")
	}

	// SeaDex redacts AB info hashes: two AB-only replacement torrents differing
	// only in their torrent page URL must not share a key, or the later
	// replacement would be suppressed as already alerted.
	abA := *f
	abA.InfoHash = "<redacted>"
	abA.ReleaseURL = "https://animebytes.tv/torrents.php?id=9&torrentid=10"
	abA.Links = []compare.ReleaseLink{{Tracker: "AB", URL: abA.ReleaseURL}}
	abB := abA
	abB.ReleaseURL = "https://animebytes.tv/torrents.php?id=9&torrentid=11"
	abB.Links = []compare.ReleaseLink{{Tracker: "AB", URL: abB.ReleaseURL}}
	if dedupeKey(&abA) == dedupeKey(&abB) {
		t.Error("redacted AB-only findings with different ReleaseURLs must produce different dedupe keys")
	}

	// Enabling AnimeBytes adds an AB link beside an unchanged public
	// representative: the key must change so the new source re-surfaces.
	publicOnly := *f
	publicOnly.Links = []compare.ReleaseLink{{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}}
	withAB := publicOnly
	withAB.Links = []compare.ReleaseLink{
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=9&torrentid=10"},
	}
	if dedupeKey(&publicOnly) == dedupeKey(&withAB) {
		t.Error("adding an AnimeBytes link must change the dedupe key")
	}

	// The FULL obtainable-source set is keyed, not just the headline
	// identity: a NON-headline public candidate's torrent replacement (a new
	// page URL beside an unchanged headline) must re-surface the finding -
	// keying only the headline suppressed such a swap forever.
	twoSources := publicOnly
	twoSources.Links = []compare.ReleaseLink{
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/2"},
	}
	secondarySwap := publicOnly
	secondarySwap.Links = []compare.ReleaseLink{
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/3"},
	}
	if dedupeKey(&twoSources) == dedupeKey(&secondarySwap) {
		t.Error("a non-headline source replacement must change the dedupe key")
	}

	// The full key shape is pinned once: identity domain tag plus the sorted
	// link-set component. (This encoding deliberately invalidates pre-change
	// persisted keys - a benign one-time re-alert burst, the accepted cost of
	// the validated, domain-separated identity and the full-source coverage.)
	if k := dedupeKey(&publicOnly); k != want+"|links=https://nyaa.si/view/1" {
		t.Errorf("public-only dedupeKey() = %q, want %q", k, want+"|links=https://nyaa.si/view/1")
	}
}

// TestDedupeKeyEscapesDelimiters pins the collision-proofing: an untrusted
// component containing the key grammar's ',' or '|' delimiters (or the '\'
// escape itself) cannot make two distinct findings share a key, which would
// suppress the second as already alerted.
func TestDedupeKeyEscapesDelimiters(t *testing.T) {
	base := compare.Finding{AniListID: 42, Status: compare.StatusBetter, InfoHash: "hash1"}

	// One group named "a,b" vs two groups "a" and "b": identical naive join.
	oneGroup := base
	oneGroup.RecommendedGroups = []string{"a,b"}
	twoGroups := base
	twoGroups.RecommendedGroups = []string{"a", "b"}
	if dedupeKey(&oneGroup) == dedupeKey(&twoGroups) {
		t.Error(`group "a,b" and groups "a","b" must not share a dedupe key`)
	}

	// A '|' inside a component must not shift the field boundary: group "x"
	// with identity URL "h|y" naively joins identically to group "x|h" with
	// identity URL "y".
	pipeInURL := base
	pipeInURL.CurrentGroup = "x"
	pipeInURL.ReleaseURL = "h|y"
	pipeInGroup := base
	pipeInGroup.CurrentGroup = "x|h"
	pipeInGroup.ReleaseURL = "y"
	if dedupeKey(&pipeInURL) == dedupeKey(&pipeInGroup) {
		t.Error(`("x", "h|y") and ("x|h", "y") must not share a dedupe key`)
	}

	// The escape character itself must be escaped or the mapping is not
	// injective: with delimiter-only escaping, ("x\", "y") and ("x", "|y")
	// both join to x\|y.
	trailingBackslash := base
	trailingBackslash.CurrentGroup = `x\`
	trailingBackslash.ReleaseURL = "y"
	leadingPipe := base
	leadingPipe.CurrentGroup = "x"
	leadingPipe.ReleaseURL = "|y"
	if dedupeKey(&trailingBackslash) == dedupeKey(&leadingPipe) {
		t.Error(`("x\", "y") and ("x", "|y") must not share a dedupe key (backslash must be escaped)`)
	}

	// The structured current-group set must survive flattening: distinct
	// two-group states ["a,b","c"] and ["a","b,c"] share the display join
	// "a,b,c", and a two-group ["A","B"] shares it with the one-group literal
	// ["A,B"]; the element-wise escaped encoding keeps their keys distinct.
	splitAB := base
	splitAB.CurrentGroups = []string{"a,b", "c"}
	splitAB.CurrentGroup = "a,b,c"
	splitBC := base
	splitBC.CurrentGroups = []string{"a", "b,c"}
	splitBC.CurrentGroup = "a,b,c"
	if dedupeKey(&splitAB) == dedupeKey(&splitBC) {
		t.Error(`current groups ["a,b","c"] and ["a","b,c"] must not share a dedupe key`)
	}
	oneLiteral := base
	oneLiteral.CurrentGroups = []string{"A,B"}
	oneLiteral.CurrentGroup = "A,B"
	twoGroupsCur := base
	twoGroupsCur.CurrentGroups = []string{"A", "B"}
	twoGroupsCur.CurrentGroup = "A,B"
	if dedupeKey(&oneLiteral) == dedupeKey(&twoGroupsCur) {
		t.Error(`current group literal ["A,B"] and groups ["A","B"] must not share a dedupe key`)
	}
}

// TestDedupeKeyBoundsOversizedComponents pins the size bound on untrusted key
// components: the SeaDex client admits up to 512 torrents per entry with
// arbitrarily long syntactically valid URLs, so an oversized AB link set must
// reduce to a fixed-size hashed identity - the key stays bounded instead of
// materializing megabytes - while distinct oversized sets still key
// distinctly (injectivity survives the hashing).
func TestDedupeKeyBoundsOversizedComponents(t *testing.T) {
	abLinks := func(tag string) []compare.ReleaseLink {
		links := make([]compare.ReleaseLink, 0, 512)
		for i := range 512 {
			links = append(links, compare.ReleaseLink{
				Tracker: "AB",
				URL: "https://animebytes.tv/torrents.php?id=9&torrentid=" + strconv.Itoa(i) +
					"&pad=" + tag + strings.Repeat("x", 4096),
			})
		}
		return links
	}
	base := compare.Finding{AniListID: 42, Status: compare.StatusBetter, InfoHash: "<redacted>"}
	setA := base
	setA.Links = abLinks("a")
	keyA := dedupeKey(&setA)
	if len(keyA) > 1024 {
		t.Errorf("dedupe key over 512 oversized AB links = %d bytes, want bounded (hashed component)", len(keyA))
	}
	setB := base
	setB.Links = abLinks("b")
	if keyB := dedupeKey(&setB); keyB == keyA {
		t.Error("distinct oversized AB link sets must not share a dedupe key")
	}
}

func TestDedupeKeyABLinkOrderIndependent(t *testing.T) {
	abA := compare.ReleaseLink{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=9&torrentid=10"}
	abB := compare.ReleaseLink{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=9&torrentid=11"}
	forward := &compare.Finding{AniListID: 42, Status: compare.StatusBetter, InfoHash: "hash1", Links: []compare.ReleaseLink{abA, abB}}
	reversed := &compare.Finding{AniListID: 42, Status: compare.StatusBetter, InfoHash: "hash1", Links: []compare.ReleaseLink{abB, abA}}
	if dedupeKey(forward) != dedupeKey(reversed) {
		t.Errorf("dedupe key must not depend on AB link order: %q vs %q", dedupeKey(forward), dedupeKey(reversed))
	}
}

// TestDedupeKeyABLinkDuplicatesIgnored pins that the AB link key describes
// the URL SET, not the occurrence list: the same AB-host URL supplied twice -
// once correctly labeled AB, once under a mislabeled Nyaa tracker (both pass
// the URL-aware ABGated check) - keys identically to the single link, so a
// later dedup or label correction upstream cannot re-alert an unchanged
// obtainable source.
func TestDedupeKeyABLinkDuplicatesIgnored(t *testing.T) {
	const abURL = "https://animebytes.tv/torrents.php?id=9&torrentid=10"
	ab := compare.ReleaseLink{Tracker: "AB", URL: abURL}
	mislabeled := compare.ReleaseLink{Tracker: "Nyaa", URL: abURL}
	single := &compare.Finding{AniListID: 42, Status: compare.StatusBetter, InfoHash: "hash1", Links: []compare.ReleaseLink{ab}}
	duplicated := &compare.Finding{AniListID: 42, Status: compare.StatusBetter, InfoHash: "hash1", Links: []compare.ReleaseLink{ab, mislabeled}}
	if dedupeKey(single) != dedupeKey(duplicated) {
		t.Errorf("dedupe key must ignore duplicate AB URLs: %q vs %q", dedupeKey(single), dedupeKey(duplicated))
	}
}

// TestDedupeKeyLinkSetIgnoresEmptyURL pins obtainableLinkKey's empty-URL
// guard: a link whose URL is empty (or whitespace-only) carries no source
// identity, so it must not perturb the key, and a finding whose every link
// is empty must key identically to a link-less finding (no links component).
func TestDedupeKeyLinkSetIgnoresEmptyURL(t *testing.T) {
	base := compare.Finding{AniListID: 42, Status: compare.StatusBetter, InfoHash: "hash1"}
	withEmpty := base
	withEmpty.Links = []compare.ReleaseLink{
		{Tracker: "Nyaa", URL: ""},
		{Tracker: "Nyaa", URL: "  "},
		{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"},
	}
	withoutEmpty := base
	withoutEmpty.Links = []compare.ReleaseLink{{Tracker: "Nyaa", URL: "https://nyaa.si/view/1"}}
	if got, want := dedupeKey(&withEmpty), dedupeKey(&withoutEmpty); got != want {
		t.Errorf("dedupeKey with empty-URL links = %q, want %q (an empty URL carries no source identity and must not perturb the key)", got, want)
	}

	allEmpty := base
	allEmpty.Links = []compare.ReleaseLink{{Tracker: "Nyaa", URL: ""}, {Tracker: "AB", URL: "  "}}
	if got, want := dedupeKey(&allEmpty), dedupeKey(&base); got != want {
		t.Errorf("dedupeKey with only empty-URL links = %q, want the link-less key %q", got, want)
	}
}

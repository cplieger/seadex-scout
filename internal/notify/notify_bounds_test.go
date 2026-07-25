package notify

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cplieger/seadex-scout/internal/compare"
)

// TestAttrJoinerRecapsAfterSanitizeGrowth pins attrJoiner.write's
// post-sanitize re-cap: runesafe.Sanitize grows each invalid UTF-8 byte into
// the three-byte U+FFFD, so the pre-sanitize cap alone lets an all-invalid
// hostile SeaDex value emit ~3x the per-attribute budget - the very
// log-pipeline line-limit overrun (and memory amplification) the budget
// exists to prevent. The emitted attribute must stay within the budget plus
// the "..." marker AND stay valid UTF-8 (the re-cap backs off to a rune
// boundary, so no partial rune re-mints raw 0x80-0x9F bytes a terminal reads
// as C1 introducers). Every other bounding test uses valid ASCII, which
// never grows.
func TestAttrJoinerRecapsAfterSanitizeGrowth(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	f := testFinding("grow", "Frieren")
	f.ReleaseURL = strings.Repeat("\xff", 16<<10) // every byte invalid: 3x growth

	notifier.Notify([]compare.Finding{f}, nil, nil, time.Now())

	got, ok := recorder.AttrValue("better release available", "release_url")
	if !ok {
		t.Fatal("finding line carries no release_url attribute")
	}
	if len(got) > maxAttrBytes+len("...") {
		t.Errorf("release_url = %d bytes, want <= %d (sanitize growth must be re-capped)", len(got), maxAttrBytes+len("..."))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("release_url = %d bytes without the ... truncation marker", len(got))
	}
	if !utf8.ValidString(got) {
		t.Error("release_url is not valid UTF-8: the re-cap split a rune")
	}
}

// TestJoinedAttrsMarkTruncationWhenBudgetEndsAtSeparator pins the honesty of
// the "..." marker on the exact-fit boundary: when the first source consumes
// the whole per-attribute budget, every later source is silently dropped at
// the separator, and the attribute must still be marked truncated. Without
// the drop marker the joined attribute renders as a complete list, so an
// operator reading recommended_groups or release_urls in Loki cannot tell
// sources were discarded. This is the only path that reaches write's
// budget-exhausted branch (capAttr writes a single piece and the aggregate
// tests always truncate mid-piece).
func TestJoinedAttrsMarkTruncationWhenBudgetEndsAtSeparator(t *testing.T) {
	notifier, recorder := newCapturedNotifier()
	exactGroup := strings.Repeat("g", maxAttrBytes)
	const tracker = "Nyaa"
	const urlPrefix = "https://nyaa.si/"
	exactURL := urlPrefix + strings.Repeat("u", maxAttrBytes-len(tracker)-len("=")-len(urlPrefix))
	f := testFinding("exact", "Frieren")
	f.RecommendedGroups = []string{exactGroup, "dropped-group"}
	f.Links = []compare.ReleaseLink{
		{Tracker: tracker, URL: exactURL},
		{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=1"},
	}

	notifier.Notify([]compare.Finding{f}, nil, nil, time.Now())

	groups, ok := recorder.AttrValue("better release available", "recommended_groups")
	if !ok {
		t.Fatal("finding line carries no recommended_groups attribute")
	}
	if want := exactGroup + "..."; groups != want {
		t.Errorf("recommended_groups = %d bytes ending %q, want the exact-fit group plus the ... marker (%d bytes)",
			len(groups), lastAttrBytes(groups), len(want))
	}
	links, ok := recorder.AttrValue("better release available", "release_urls")
	if !ok {
		t.Fatal("finding line carries no release_urls attribute")
	}
	if want := tracker + "=" + exactURL + "..."; links != want {
		t.Errorf("release_urls = %d bytes ending %q, want the exact-fit first source plus the ... marker (%d bytes)",
			len(links), lastAttrBytes(links), len(want))
	}
}

// lastAttrBytes returns a short tail of s for a failure message, so a
// multi-kilobyte attribute does not flood the test log.
func lastAttrBytes(s string) string {
	const tail = 12
	if len(s) <= tail {
		return s
	}
	return s[len(s)-tail:]
}

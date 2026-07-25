package notify

import (
	"encoding/json"
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

// TestStoredFindingBoundsPersistedStrings pins the PERSISTENCE bound on the
// dedupe record's untrusted strings. The emit path's per-attribute cap only
// bounds what is logged; title and the two group names also ride into
// state.json, so an upstream value near SeaDex's per-page allowance would push
// the encoded state past its own write bound and freeze dedupe persistence for
// every later cycle (CWE-400). Each persisted string must stay within
// maxAttrBytes, carry the truncation marker, hold no unsafe runes, and the
// projection must be idempotent so a record read back from legacy state is not
// re-truncated.
func TestStoredFindingBoundsPersistedStrings(t *testing.T) {
	hostile := strings.Repeat("A", 40<<20)
	f := testFinding("bound", hostile)
	f.CurrentGroup = hostile
	f.RecommendedGroup = hostile + "\u0007\u202e"

	stored := storedFinding(&f)

	for name, got := range map[string]string{
		"Title":            stored.Title,
		"CurrentGroup":     stored.CurrentGroup,
		"RecommendedGroup": stored.RecommendedGroup,
	} {
		if len(got) > maxAttrBytes {
			t.Errorf("persisted %s = %d bytes, want <= %d", name, len(got), maxAttrBytes)
		}
		if !strings.HasSuffix(got, attrTruncMarker) {
			t.Errorf("persisted %s = ...%q, want the %q truncation marker", name, lastAttrBytes(got), attrTruncMarker)
		}
		if !utf8.ValidString(got) {
			t.Errorf("persisted %s is not valid UTF-8", name)
		}
		if strings.ContainsAny(got, "\u0007\u202e") {
			t.Errorf("persisted %s carries an unsafe rune", name)
		}
	}

	// A record projected twice (the shape a legacy state read-back takes) must
	// be byte-identical: capPersisted is idempotent, so dedupe continuity does
	// not depend on how many times a value passed through it.
	again := compare.Finding{
		Arr:              stored.Arr,
		CurrentGroup:     stored.CurrentGroup,
		RecommendedGroup: stored.RecommendedGroup,
		Title:            stored.Title,
		Status:           stored.Status,
		AniListID:        stored.AniListID,
		Season:           stored.Season,
	}
	if got := storedFinding(&again); got != stored {
		t.Errorf("re-projected record differs from the first projection, want an idempotent capPersisted")
	}

	// The bounded record must encode small enough that a state file holding
	// many of them stays far below the store's own 32 MiB write bound.
	encoded, err := json.Marshal(Alerted{AlertedAt: time.Unix(0, 0).UTC(), Finding: stored})
	if err != nil {
		t.Fatalf("json.Marshal(Alerted): %v", err)
	}
	if maxRecordBytes := 4 * maxAttrBytes; len(encoded) > maxRecordBytes {
		t.Errorf("encoded dedupe record = %d bytes, want <= %d", len(encoded), maxRecordBytes)
	}
}

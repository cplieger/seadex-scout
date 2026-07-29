package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cplieger/seadex-scout/internal/compare"
)

// TestAttrJoinerRecapsAfterSanitizeGrowth pins logattr.Joiner.Write's
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
	f.Links = gradedLinks(
		compare.ReleaseLink{Tracker: tracker, URL: exactURL},
		compare.ReleaseLink{Tracker: "AB", URL: "https://animebytes.tv/torrents.php?id=1"},
	)

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

// TestPreservedRecordBoundsReadBackStatus pins the persistence half of the
// read-back trust boundary on the ONE field the projection path does not
// produce itself: a prior record decoded from a tampered or legacy state.json
// takes the failed-item preservation branch, which returns it through
// capStored WITHOUT going through emitResolved, so an oversized or unsafe
// status must be bounded there too - otherwise the preserved record carries a
// multi-megabyte value straight back into the next state Save and can cross
// the store's 32 MiB write bound (CWE-400).
func TestPreservedRecordBoundsReadBackStatus(t *testing.T) {
	notifier, _ := newCapturedNotifier()
	const alID = 154587
	oldTime := time.Unix(0, 0).UTC()
	prior := map[string]Alerted{
		"legacy": {AlertedAt: oldTime, Finding: StoredFinding{
			Arr:       "sonarr",
			Title:     "Frieren",
			Status:    compare.Status(strings.Repeat("s", 40<<10) + "\u202e"),
			AniListID: alID,
		}},
	}

	current := notifier.Notify(nil, prior, map[int]struct{}{alID: {}}, time.Now())

	rec, ok := current["legacy"]
	if !ok {
		t.Fatalf("failed item's prior record was not preserved: %+v", current)
	}
	got := string(rec.Finding.Status)
	if len(got) > maxAttrBytes {
		t.Errorf("preserved Status = %d bytes, want <= %d", len(got), maxAttrBytes)
	}
	if !strings.HasSuffix(got, attrTruncMarker) {
		t.Errorf("preserved Status = ...%q, want the %q truncation marker", lastAttrBytes(got), attrTruncMarker)
	}
	if strings.ContainsAny(got, "\u202e") {
		t.Error("preserved Status carries an unsafe rune")
	}
}

// TestCapAlertTextAttrNeutralizesMarkupAndMentions pins the alert-sink output
// encoding: capAttr bounds and sanitizes a value for the JSON slog sink but
// performs no markup encoding, and alerts.yaml interpolates alert_title /
// alert_recommended_group into a Discord/Slack annotation, so an untrusted
// SeaDex title must not be able to render as a link, a code span, or a
// receiver mention (CWE-116).
func TestCapAlertTextAttrNeutralizesMarkupAndMentions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "inline link",
			in:   "[security update](https://attacker.example)",
			want: `\[security update\]\(https://attacker.example\)`,
		},
		{name: "everyone mention", in: "@everyone", want: `\@everyone`},
		{name: "slack user mention", in: "<@U123>", want: `&lt;\@U123&gt;`},
		{name: "ampersand", in: "Fate & Co", want: "Fate &amp; Co"},
		{name: "emphasis and code", in: "*a*_b_`c`~d~|e|", want: `\*a\*\_b\_` + "\\`c\\`" + `\~d\~\|e\|`},
		{name: "backslash first", in: `a\b`, want: `a\\b`},
		{name: "newline", in: "a\n# b", want: "a # b"},
		{name: "carriage return", in: "a\rb", want: "a b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := capAlertTextAttr(tt.in); got != tt.want {
				t.Errorf("capAlertTextAttr(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCapAlertTextAttrDropsBidiControls pins that the alert twin keeps
// capAttr's rune sanitization: a bidi override in a SeaDex title must not
// survive into an annotation a human reads (the escaper only adds markup
// encoding on top, it must never bypass the sanitize pass).
func TestCapAlertTextAttrDropsBidiControls(t *testing.T) {
	if got := capAlertTextAttr("Frieren\u202eevil"); strings.ContainsAny(got, "\u202e") {
		t.Errorf("capAlertTextAttr = %q, want no bidi control rune", got)
	}
}

// TestCapAlertTextAttrRecapsAfterEscapeGrowth pins the re-cap: every escaped
// byte grows the value, so the pre-escape cap alone would emit ~2x the
// per-attribute budget into the log pipeline.
func TestCapAlertTextAttrRecapsAfterEscapeGrowth(t *testing.T) {
	got := capAlertTextAttr(strings.Repeat("*", 2*maxAttrBytes))
	if len(got) > maxAttrBytes {
		t.Errorf("capAlertTextAttr len = %d, want <= %d", len(got), maxAttrBytes)
	}
	if !strings.HasSuffix(got, attrTruncMarker) {
		t.Errorf("capAlertTextAttr = ...%q, want the %q truncation marker", lastAttrBytes(got), attrTruncMarker)
	}
}

// TestFindingKVsCarriesEscapedAlertLabels pins the alerts.yaml contract: the
// raw title / recommended_group labels stay for Loki search and grouping, and
// the annotation-facing alert_* twins carry the escaped values. Without this
// the two labels alerts.yaml renders could be dropped silently.
func TestFindingKVsCarriesEscapedAlertLabels(t *testing.T) {
	f := compare.Finding{
		Title:            "[phish](https://attacker.example)",
		RecommendedGroup: "@everyone",
	}
	kvs := findingKVs(&f)
	got := map[string]string{}
	for i := 0; i+1 < len(kvs); i += 2 {
		key, ok := kvs[i].(string)
		if !ok {
			continue
		}
		if value, ok := kvs[i+1].(string); ok {
			got[key] = value
		}
	}
	for key, want := range map[string]string{
		"title":                   f.Title,
		"recommended_group":       f.RecommendedGroup,
		"alert_title":             `\[phish\]\(https://attacker.example\)`,
		"alert_recommended_group": `\@everyone`,
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// TestFindingKVsAlertLabelsCarryNoLineBreaks pins the line-structure half of
// the alert-sink encoding: capAttr keeps CR/LF for the JSON slog sink, but the
// annotation body alerts.yaml renders is a single line, so a newline in an
// untrusted title would re-open the line-start markdown constructs inline
// escaping cannot reach (a '#' heading, a list bullet, an auto-linked bare
// URL). The raw labels keep the newline; only the alert_* twins flatten it.
func TestFindingKVsAlertLabelsCarryNoLineBreaks(t *testing.T) {
	f := compare.Finding{
		Title:            "Frieren\n# Library compromised\r\nvisit https://attacker.example",
		RecommendedGroup: "PMR\nMTBB",
	}
	kvs := findingKVs(&f)
	for i := 0; i+1 < len(kvs); i += 2 {
		key, ok := kvs[i].(string)
		if !ok || !strings.HasPrefix(key, "alert_") {
			continue
		}
		value, ok := kvs[i+1].(string)
		if !ok {
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			t.Errorf("%s = %q, want no CR/LF", key, value)
		}
	}
}

// TestPreservedRecordBoundsEveryReadBackString widens the read-back bound to
// every untrusted string capStored re-bounds. A prior record decoded from a
// legacy or tampered state.json takes the failed-item preservation branch,
// which returns it straight into the next state Save without passing
// emitResolved, so an unbounded Arr, Title, CurrentGroup or RecommendedGroup
// rides back onto disk and can cross the store's 32 MiB write bound (CWE-400).
// The existing Status-only pin stays green if any of the other four re-bounds
// is deleted.
func TestPreservedRecordBoundsEveryReadBackString(t *testing.T) {
	notifier, _ := newCapturedNotifier()
	const alID = 154587
	hostile := strings.Repeat("A", 40<<10) + "\u202e"
	prior := map[string]Alerted{
		"legacy": {AlertedAt: time.Unix(0, 0).UTC(), Finding: StoredFinding{
			Arr:              hostile,
			Title:            hostile,
			CurrentGroup:     hostile,
			RecommendedGroup: hostile,
			Status:           compare.StatusBetter,
			AniListID:        alID,
		}},
	}

	current := notifier.Notify(nil, prior, map[int]struct{}{alID: {}}, time.Now())

	rec, ok := current["legacy"]
	if !ok {
		t.Fatalf("failed item's prior record was not preserved: %+v", current)
	}
	for name, got := range map[string]string{
		"Arr":              rec.Finding.Arr,
		"Title":            rec.Finding.Title,
		"CurrentGroup":     rec.Finding.CurrentGroup,
		"RecommendedGroup": rec.Finding.RecommendedGroup,
	} {
		if len(got) > maxAttrBytes {
			t.Errorf("preserved %s = %d bytes, want <= %d", name, len(got), maxAttrBytes)
		}
		if !strings.HasSuffix(got, attrTruncMarker) {
			t.Errorf("preserved %s = ...%q, want the %q truncation marker", name, lastAttrBytes(got), attrTruncMarker)
		}
		if strings.ContainsAny(got, "\u202e") {
			t.Errorf("preserved %s carries an unsafe rune", name)
		}
	}
}

// TestCapAlertTextAttrNeverEndsInADanglingEscape pins the truncation half of
// the alert twin: the re-cap cuts the ESCAPED string, so without
// trimTruncatedEscape the cut can fall between a backslash and the character it
// escapes and the value ends "\..." - a dangling escape the reader sees and a
// Markdown sink reads as an escaped '.'. Both input parities are driven, since
// which side of an escape pair the byte budget lands on depends on the value.
func TestCapAlertTextAttrNeverEndsInADanglingEscape(t *testing.T) {
	for name, in := range map[string]string{
		"even offset": strings.Repeat("*", 2*maxAttrBytes),
		"odd offset":  "a" + strings.Repeat("*", 2*maxAttrBytes),
	} {
		t.Run(name, func(t *testing.T) {
			got := capAlertTextAttr(in)
			body, truncated := strings.CutSuffix(got, attrTruncMarker)
			if !truncated {
				t.Fatalf("capAlertTextAttr(%d bytes) was not truncated; the case no longer exercises the cut", len(in))
			}
			run := 0
			for i := len(body) - 1; i >= 0 && body[i] == '\\'; i-- {
				run++
			}
			if run%2 == 1 {
				t.Errorf("truncated value ends in a dangling escape: %q", body[max(0, len(body)-8):]+attrTruncMarker)
			}
			if len(got) > maxAttrBytes {
				t.Errorf("truncated value is %d bytes, want at most %d", len(got), maxAttrBytes)
			}
		})
	}
}

package notify

import (
	"strings"
	"testing"
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

	notifier.Report([]compare.Finding{f}, nil)

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

	notifier.Report([]compare.Finding{f}, nil)

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

// TestCapAlertTextAttrNeutralizesMarkupAndMentions pins the alert-sink output
// encoding: capAttr bounds and sanitizes a value for the JSON slog sink but
// performs no markup encoding, and alerts.yaml interpolates alert_title /
// alert_recommended_group into a Discord annotation, so an untrusted
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
		{name: "here mention", in: "@here", want: `\@here`},
		// The Discord user-mention form is neutralized by the '@' escape
		// alone: the angle brackets are not markup for this sink and now
		// survive as the honest text they are.
		{name: "discord user mention", in: "<@123>", want: `<\@123>`},
		{name: "html tag", in: "<script>", want: "<script>"},
		{name: "ampersand", in: "Tiger & Bunny", want: "Tiger & Bunny"},
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

// TestCapAlertTextAttrEmitsNoHTMLEntities pins the single-sink decision
// (l-f84): the escaper targets Discord only, so the Slack-mrkdwn entity half
// is gone and an honest '&', '<' or '>' must reach the annotation as itself.
// A reintroduced entity encoding would make every such title read
// "Tiger &amp; Bunny" in the Discord receiver the homelab Alertmanager is
// provisioned with.
func TestCapAlertTextAttrEmitsNoHTMLEntities(t *testing.T) {
	corpus := []string{
		"Tiger & Bunny",
		"<script>alert(1)</script>",
		"<@123>",
		"<!everyone>",
		"a > b < c & d",
		"&amp;",
		"&<>",
		strings.Repeat("&<>", 4*maxAttrBytes),
	}
	for _, in := range corpus {
		got := capAlertTextAttr(in)
		for _, entity := range []string{"&amp;", "&lt;", "&gt;"} {
			if strings.Contains(got, entity) && !strings.Contains(in, entity) {
				t.Errorf("capAlertTextAttr(%q) = %q, want no %q entity encoding", in, got, entity)
			}
		}
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

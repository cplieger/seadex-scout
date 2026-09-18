package notify

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/compare"
)

// The shipped alerts/logql.yaml keys its better-release rule on an exact msg literal,
// groups by an exact label set, and interpolates a fixed set of labels into its
// annotations; these read all three out of the rules file so no attribute name or
// inventory is retyped in this test.
var (
	alertSumByRe = regexp.MustCompile(`sum by \(([^)]*)\)`)
	alertMsgRe   = regexp.MustCompile("msg=`([^`]+)`")
	alertLabelRe = regexp.MustCompile(`\$labels\.([a-z_]+)`)
)

// betterReleaseRule returns the shipped better-release rule's body, sliced out
// of the raw alerts/logql.yaml bytes so every assertion below reads the real consumer.
func betterReleaseRule(t *testing.T, raw []byte) string {
	t.Helper()
	const anchor = "alert: SeadexScoutBetterReleaseFound"
	body := string(raw)
	start := strings.Index(body, anchor)
	if start < 0 {
		t.Fatalf("alerts/logql.yaml carries no %q rule; the better-release alert contract was renamed or removed", anchor)
	}
	body = body[start:]
	if next := strings.Index(body[len(anchor):], "- alert:"); next >= 0 {
		body = body[:len(anchor)+next]
	}
	return body
}

// betterReleaseContract extracts the shipped better-release rule's matched
// message literal and its `sum by` label set from the raw alerts/logql.yaml bytes.
func betterReleaseContract(t *testing.T, raw []byte) (string, []string) {
	t.Helper()
	body := betterReleaseRule(t, raw)
	msg := alertMsgRe.FindStringSubmatch(body)
	if msg == nil {
		t.Fatalf("the SeadexScoutBetterReleaseFound rule matches no msg=`...` literal:\n%s", body)
	}
	labels := alertSumByRe.FindStringSubmatch(body)
	if labels == nil {
		t.Fatalf("the SeadexScoutBetterReleaseFound rule carries no `sum by (...)` label set:\n%s", body)
	}
	var want []string
	for label := range strings.SplitSeq(labels[1], ",") {
		if label = strings.TrimSpace(label); label != "" {
			want = append(want, label)
		}
	}
	return msg[1], want
}

// interpolatedAlertLabels returns the distinct labels the better-release rule's
// annotations actually INTERPOLATE, read out of alerts/logql.yaml. That inventory is
// the rules file's knowledge, never this test's: a value the annotation does not
// render occupies none of the embed's budget, and a value it starts rendering
// must be accounted for the moment it does.
func interpolatedAlertLabels(t *testing.T, raw []byte) []string {
	t.Helper()
	body := betterReleaseRule(t, raw)
	annotations := strings.Index(body, "annotations:")
	if annotations < 0 {
		t.Fatalf("the SeadexScoutBetterReleaseFound rule carries no annotations block:\n%s", body)
	}
	seen := map[string]bool{}
	var labels []string
	for _, m := range alertLabelRe.FindAllStringSubmatch(body[annotations:], -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			labels = append(labels, m[1])
		}
	}
	if len(labels) == 0 {
		t.Fatalf("the SeadexScoutBetterReleaseFound annotations interpolate no $labels.* value:\n%s", body)
	}
	return labels
}

// alertAttrBudgets is the byte budget findingKVs renders each interpolable
// attribute under. The BUDGETS are the code's knowledge; the INVENTORY is
// alerts/logql.yaml's, so neither side can be hand-copied wrong. A
// fixed-pattern app value carries 0.
//
// A label the annotation renders but this map does not classify FAILS the
// test: release_url and release_urls are absent and carry the multi-KB
// log-line budget, so rendering one needs its cap revisited first.
var alertAttrBudgets = map[string]int{
	"alert_title":             maxAlertTextBytes,
	"alert_recommended_group": maxAlertTextBytes,
	"public_tracker":          maxAlertTextBytes,
	"ab_tracker":              maxAlertTextBytes,
	"arr_url":                 maxAlertURLBytes,
	"nyaa_url":                maxAlertURLBytes,
	"public_url":              maxAlertURLBytes,
	"ab_url":                  maxAlertURLBytes,
	"al_id":                   0,
	"arr":                     0,
	"season":                  0,
	"seadex_tags":             0,
}

// TestAlertContractMatchesShippedRules pins the one observable contract this
// package has: the repo ships alerts/logql.yaml, whose better-release rule
// matches an exact message literal and groups by a fixed label set. The two
// halves deploy independently with no import edge, so a renamed or dropped
// attribute key otherwise goes silently quiet with a green build. Reading the
// message and the labels OUT of the rules file is what makes them unable to
// drift. Presence only, so it pins the contract and not the sample data.
func TestAlertContractMatchesShippedRules(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "alerts", "logql.yaml"))
	if err != nil {
		t.Fatalf("read alerts/logql.yaml: %v", err)
	}
	wantMsg, wantLabels := betterReleaseContract(t, raw)

	notifier, recorder := newCapturedNotifier()
	notifier.Report([]compare.Finding{testFinding("k1", "Frieren")}, nil)

	keys := map[string]bool{}
	var found bool
	for _, rec := range recorder.Records() {
		if rec.Message != wantMsg {
			continue
		}
		found = true
		rec.Attrs(func(a slog.Attr) bool {
			keys[a.Key] = true
			return true
		})
	}
	if !found {
		t.Fatalf("alerts/logql.yaml matches msg=%q but no line with that exact message was emitted", wantMsg)
	}
	for _, label := range wantLabels {
		if !keys[label] {
			t.Errorf("alerts/logql.yaml groups by %q but the finding line emits no such attribute", label)
		}
	}
}

// TestAlertAnnotationBudgetFitsTheEmbedLimit pins the ARITHMETIC behind
// maxAlertURLBytes, which a reader cannot check by eye.
//
// alerts/logql.yaml interpolates several untrusted values into ONE Discord
// annotation and renders the clickable tracker links LAST, so if the values can
// collectively exceed the embed's 4096-rune description limit, the half the
// operator acts on is what gets cut. Capping each value is not enough on its
// own: their SUM has to fit, which is why the URL bound is 256.
func TestAlertAnnotationBudgetFitsTheEmbedLimit(t *testing.T) {
	t.Parallel()
	// Discord's embed description limit, the ceiling Alertmanager's notifier
	// truncates the rendered annotation at.
	const discordEmbedDescriptionRunes = 4096
	raw, err := os.ReadFile(filepath.Join("..", "..", "alerts", "logql.yaml"))
	if err != nil {
		t.Fatalf("read alerts/logql.yaml: %v", err)
	}
	worst := 0
	for _, label := range interpolatedAlertLabels(t, raw) {
		budget, classified := alertAttrBudgets[label]
		if !classified {
			t.Errorf("alerts/logql.yaml interpolates %q into the annotation but this test does not know its budget; "+
				"classify it in alertAttrBudgets (an attribute rendered on the multi-KB log-line budget must be "+
				"re-capped before it can be interpolated at all)", label)
			continue
		}
		worst += budget
	}
	if worst > discordEmbedDescriptionRunes {
		t.Errorf("every interpolated attribute at its cap sums to %d bytes, over the %d-rune "+
			"embed description limit: the clickable links render LAST, so they are what a "+
			"truncation deletes. Lower maxAlertURLBytes (or maxAlertTextBytes) rather than "+
			"relaxing this test",
			worst, discordEmbedDescriptionRunes)
	}
	// The URL bound must stay above what the publisher can actually emit, or an
	// honest link is truncated into a dead one. Measured across the whole live
	// SeaDex catalogue (2821 entries / 9208 torrents, 2026-08): the longest
	// published URL is 96 bytes, an AnimeTosho view path carrying a release-name
	// slug. The margin covers the arr deep link too (the operator's own base
	// plus a TVDB title slug), which no upstream measurement can bound.
	const measuredLongestPublishedURL = 96
	if maxAlertURLBytes <= measuredLongestPublishedURL {
		t.Errorf("maxAlertURLBytes=%d is not above the measured longest published URL (%d); "+
			"an honest tracker link would be truncated into a dead one",
			maxAlertURLBytes, measuredLongestPublishedURL)
	}
}

// TestCapURLAttrHoldsTheAlertBound checks the bound is actually APPLIED, not
// merely declared: a hostile URL must not reach the annotation multi-KB long.
func TestCapURLAttrHoldsTheAlertBound(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"hostile long path":  "https://nyaa.si/view/" + strings.Repeat("a", 4000),
		"hostile long query": "https://animebytes.tv/torrents.php?id=1&x=" + strings.Repeat("b", 4000),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := capURLAttr(raw)
			if len(got) > maxAlertURLBytes+len(attrTruncMarker) {
				t.Errorf("capURLAttr returned %d bytes, over maxAlertURLBytes+marker (%d)",
					len(got), maxAlertURLBytes+len(attrTruncMarker))
			}
			if !strings.HasSuffix(got, attrTruncMarker) {
				t.Errorf("a truncated URL must carry the %q marker so a reader can tell it "+
					"from an honest one; got %q", attrTruncMarker, got)
			}
		})
	}
	// An honest URL at the measured worst case passes through byte-identical -
	// the property that makes the cap safe to tighten.
	honest := "https://animetosho.org/view/freepalestine-angels-3piece-tenshi-no-3p-bd-1080p-hi10-opus.n1741873"
	if got := capURLAttr(honest); got != honest {
		t.Errorf("the longest URL in the live catalogue must pass through unchanged;\n got %q\nwant %q",
			got, honest)
	}
}

// TestCapAlertTextAttrHoldsTheAlertBound is the TEXT twin of
// TestCapURLAttrHoldsTheAlertBound: capAlertTextAttr must re-cap on
// maxAlertTextBytes rather than the multi-KB Loki log-line budget, which is
// what makes TestAlertAnnotationBudgetFitsTheEmbedLimit's arithmetic describe
// the shipped template rather than just its constants. Every other assertion on
// this function bounds it by maxAttrBytes, 16x looser, so a regression to the
// log-line budget would let an oversized SeaDex title push the clickable
// tracker links out of the embed with the whole suite green.
func TestCapAlertTextAttrHoldsTheAlertBound(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"plain oversized title":  strings.Repeat("A", 4*maxAttrBytes),
		"escape-growing title":   strings.Repeat("*", 4*maxAttrBytes),
		"multi-byte CJK title":   strings.Repeat("葬", 4*maxAttrBytes),
		"oversized group marker": strings.Repeat("[PMR]", maxAttrBytes),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := capAlertTextAttr(raw)
			if len(got) > maxAlertTextBytes {
				t.Errorf("capAlertTextAttr returned %d bytes, over maxAlertTextBytes (%d)", len(got), maxAlertTextBytes)
			}
			if !strings.HasSuffix(got, attrTruncMarker) {
				t.Errorf("a truncated alert text must carry the %q marker so a reader can tell it "+
					"from an honest one; got the tail %q", attrTruncMarker, got[max(0, len(got)-12):])
			}
		})
	}
	// An honest value well inside the budget passes through with only the markup
	// escaping applied - the property that makes the tighter bound safe.
	const honest = "Sousou no Frieren [SubsPlease]"
	if got, want := capAlertTextAttr(honest), `Sousou no Frieren \[SubsPlease\]`; got != want {
		t.Errorf("capAlertTextAttr(%q) = %q, want %q", honest, got, want)
	}
}

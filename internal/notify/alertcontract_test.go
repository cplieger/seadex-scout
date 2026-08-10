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

// The shipped alerts.yaml keys its better-release rule on an exact msg literal
// and groups by an exact label set; these read both out of the rules file so no
// attribute name is retyped in this test.
var (
	alertSumByRe = regexp.MustCompile(`sum by \(([^)]*)\)`)
	alertMsgRe   = regexp.MustCompile("msg=`([^`]+)`")
)

// betterReleaseContract extracts the shipped better-release rule's matched
// message literal and its `sum by` label set from the raw alerts.yaml bytes.
func betterReleaseContract(t *testing.T, raw []byte) (string, []string) {
	t.Helper()
	const anchor = "alert: SeadexScoutBetterReleaseFound"
	body := string(raw)
	start := strings.Index(body, anchor)
	if start < 0 {
		t.Fatalf("alerts.yaml carries no %q rule; the better-release alert contract was renamed or removed", anchor)
	}
	body = body[start:]
	if next := strings.Index(body[len(anchor):], "- alert:"); next >= 0 {
		body = body[:len(anchor)+next]
	}
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

// TestAlertContractMatchesShippedRules pins the ONE observable contract this
// package has: observability is slog-only, and the repo ships alerts.yaml whose
// better-release rule matches an exact message literal and groups by a fixed
// label set. The two halves are deployed independently (this binary vs a rules
// file loaded into Loki/Mimir) with no import edge between them, so a renamed
// or dropped attribute key otherwise goes silently quiet with a green build.
// Reading the message and the labels OUT of the rules file - rather than
// re-spelling them here - is what makes the halves unable to drift: this test
// is a check of the real consumer, not a third hand-copied version of the
// contract. It asserts key PRESENCE only, so it pins the contract and not the
// sample data.
func TestAlertContractMatchesShippedRules(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "alerts.yaml"))
	if err != nil {
		t.Fatalf("read alerts.yaml: %v", err)
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
		t.Fatalf("alerts.yaml matches msg=%q but no line with that exact message was emitted", wantMsg)
	}
	for _, label := range wantLabels {
		if !keys[label] {
			t.Errorf("alerts.yaml groups by %q but the finding line emits no such attribute", label)
		}
	}
}

// TestAlertAnnotationBudgetFitsTheEmbedLimit pins the ARITHMETIC behind
// maxAlertURLBytes, which is the part a reader cannot check by eye and the part
// a future cap change would silently break.
//
// The failure it guards is specific: alerts.yaml interpolates nine untrusted
// values into ONE Discord annotation and renders the clickable tracker links
// LAST, so if the values can collectively exceed the embed's 4096-rune
// description limit, the half the operator acts on is what gets cut. Capping
// every interpolated value is therefore not enough on its own - their SUM has to
// fit, which is why the URL bound is 256 rather than reusing the 512 the text
// attributes carry or the multi-KB Loki log-line budget the URLs used to.
func TestAlertAnnotationBudgetFitsTheEmbedLimit(t *testing.T) {
	t.Parallel()
	// Discord's embed description limit, the ceiling Alertmanager's notifier
	// truncates the rendered annotation at.
	const discordEmbedDescriptionRunes = 4096
	// findingKVs interpolates four text attributes (alert_title,
	// alert_recommended_group, public_tracker, ab_tracker) and five URL
	// attributes (arr_url, release_url, nyaa_url, public_url, ab_url).
	const alertTextAttrs, alertURLAttrs = 4, 5
	worst := alertTextAttrs*maxAlertTextBytes + alertURLAttrs*maxAlertURLBytes
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
// merely declared: capURLAttr used to re-cap on the Loki log-line budget, so a
// hostile URL rode into the annotation multi-KB long.
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

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

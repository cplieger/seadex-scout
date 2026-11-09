package scout

import (
	"testing"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/seadex-scout/internal/logcontract"
)

// assertContractAttrs fails when the first msg line lacks an attribute
// the log contract declares stable on msg, so a promoted key cannot be
// renamed without the dashboard that reads it noticing.
func assertContractAttrs(t *testing.T, recorder *capture.Recorder, msg string) {
	t.Helper()
	c, err := logcontract.Load("../..")
	if err != nil {
		t.Fatalf("parse the log contract: %v", err)
	}
	want := c.Messages[msg]
	if len(want) == 0 {
		t.Fatalf("the log contract declares no attributes on %q", msg)
	}
	for _, key := range want {
		if _, ok := recorder.AttrValue(capture.Message(msg), capture.AttrName(key)); !ok {
			t.Errorf("%q line lacks %q, which the log contract declares stable", msg, key)
		}
	}
}

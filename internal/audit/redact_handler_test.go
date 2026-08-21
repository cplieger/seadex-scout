package audit

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/pathredact"
)

// TestRedactingHandlerRedactsAttachedAndGroupedAttrs pins the redaction
// contract on the slog.Handler surfaces no pipeline test reaches: attributes
// attached ahead of time via Logger.With (WithAttrs), attributes emitted
// under a Logger.WithGroup scope, inline group-valued attributes (the
// KindGroup recursion), and non-error KindAny values, which cannot carry the
// dir and must pass through unchanged. A regression that forwards attached
// attrs unredacted would ship the secret-capable report.dir value to Loki.
func TestRedactingHandlerRedactsAttachedAndGroupedAttrs(t *testing.T) {
	const dir = "/config/sekret-passkey-sentinel"

	tests := []struct {
		name string
		emit func(log *slog.Logger)
	}{
		{"attr attached via With is redacted", func(log *slog.Logger) {
			log.With("path", dir).Info("attached")
		}},
		{"attr under WithGroup is redacted", func(log *slog.Logger) {
			log.WithGroup("stage").Info("grouped", "path", dir+"/report.md")
		}},
		{"inline group member is redacted", func(log *slog.Logger) {
			log.Info("inline", slog.Group("io", slog.String("path", dir)))
		}},
		{"error attr attached via With is redacted", func(log *slog.Logger) {
			log.With("error", errors.New("open "+dir+": denied")).Info("attached error")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := pathredact.Logger(slog.New(slog.NewJSONHandler(&buf, nil)), dir)

			tt.emit(log)

			out := buf.String()
			if strings.Contains(out, "sekret-passkey-sentinel") {
				t.Errorf("redacting handler leaked the report.dir value: %s", out)
			}
			if !strings.Contains(out, pathredact.ReportDirMarker) {
				t.Errorf("redacting handler emitted no %q marker: %s", pathredact.ReportDirMarker, out)
			}
		})
	}

	t.Run("non-error any and numeric attrs pass through", func(t *testing.T) {
		var buf bytes.Buffer
		log := pathredact.Logger(slog.New(slog.NewJSONHandler(&buf, nil)), dir)

		log.Info("passthrough", "rows", 3, "obj", struct{ N int }{N: 7})

		out := buf.String()
		if !strings.Contains(out, `"rows":3`) {
			t.Errorf("numeric attr must pass through unchanged: %s", out)
		}
		if !strings.Contains(out, `"N":7`) {
			t.Errorf("non-error any attr must pass through unchanged: %s", out)
		}
	})
}

// nilReceiverError is an error whose Error() dereferences its receiver, so a
// typed-nil value of it panics when Error() is called. slog itself never calls
// it (fmt recovers a nil-receiver panic and renders "<nil>"), which is exactly
// why the redacting handler must not call it either.
type nilReceiverError struct{ msg string }

func (e *nilReceiverError) Error() string { return e.msg }

// TestRedactingHandlerPassesTypedNilErrorThrough pins the typed-nil guard in
// redactAttr (isNilErrValue): a KindAny attribute holding a non-nil error
// interface over a nil pointer must be forwarded untouched, because redacting
// it would call Error() on a nil receiver and panic inside the report
// pipeline's own logger - taking down the report run rather than logging it.
// Nothing else in the suite constructs one, so dropping the guard as a
// simplification stays green today.
func TestRedactingHandlerPassesTypedNilErrorThrough(t *testing.T) {
	const dir = "/config/sekret-passkey-sentinel"
	var typedNil *nilReceiverError
	var buf bytes.Buffer
	log := pathredact.Logger(slog.New(slog.NewJSONHandler(&buf, nil)), dir)

	log.Info("typed nil error attr", "error", error(typedNil))

	if got, want := buf.String(), `"error":"<nil>"`; !strings.Contains(got, want) {
		t.Errorf("record = %s, want the typed-nil error attr forwarded as %s", got, want)
	}
}

// TestRedactingHandlerRedactsRecordMessage pins the Record.Message half of
// the redaction contract, which every other handler test misses: the suite
// above only places the dir in attached, grouped, or inline attributes, so a
// regression that forwarded rec.Message verbatim would still pass while
// leaking the configured report directory to Loki.
func TestRedactingHandlerRedactsRecordMessage(t *testing.T) {
	const dir = "/config/sekret-passkey-sentinel"
	var buf bytes.Buffer
	log := pathredact.Logger(slog.New(slog.NewJSONHandler(&buf, nil)), dir)

	log.Info("failed to write " + dir + "/report.json")

	out := buf.String()
	if strings.Contains(out, "sekret-passkey-sentinel") {
		t.Errorf("redacting handler leaked report.dir from the record message: %s", out)
	}
	if !strings.Contains(out, pathredact.ReportDirMarker) {
		t.Errorf("redacting handler emitted no %q marker: %s", pathredact.ReportDirMarker, out)
	}
}

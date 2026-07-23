package audit

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
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
			log := redactingLogger(slog.New(slog.NewJSONHandler(&buf, nil)), dir)

			tt.emit(log)

			out := buf.String()
			if strings.Contains(out, "sekret-passkey-sentinel") {
				t.Errorf("redacting handler leaked the report.dir value: %s", out)
			}
			if !strings.Contains(out, redactedPath) {
				t.Errorf("redacting handler emitted no %q marker: %s", redactedPath, out)
			}
		})
	}

	t.Run("non-error any and numeric attrs pass through", func(t *testing.T) {
		var buf bytes.Buffer
		log := redactingLogger(slog.New(slog.NewJSONHandler(&buf, nil)), dir)

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

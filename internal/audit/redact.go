package audit

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
)

// redactedPath is the marker substituted for the configured report directory
// (and any path derived from it) in report-pipeline logs and returned errors.
// report.dir is a secret-capable config value: config.Load expands any
// allowlisted ${SEADEX_SCOUT_*} reference in every string field, so a paste
// typo such as `report.dir: ${SEADEX_SCOUT_AB_PASSKEY}` makes a passkey the
// effective directory. Filesystem calls keep the real path; only the
// diagnostics that cross into slog (shipped to Loki) or main's error log are
// redacted.
const redactedPath = "[report.dir]"

// redactPathText replaces every occurrence of the configured report dir - and
// of each of its path-prefix ancestors, which an os.PathError for a failed
// intermediate component (MkdirAll) can carry instead of the full dir - with
// the redactedPath marker. Ancestor redaction is deliberately broad: these
// texts are report-scoped diagnostics whose only path-like content derives
// from report.dir, so over-masking a benign ancestor costs nothing while a
// missed fragment could ship a secret.
func redactPathText(dir, s string) string {
	if dir == "" {
		return s
	}
	// A degenerate configured dir ("." or "/") can never hold a pasted
	// secret, and replacing it would rewrite every dot or slash in the
	// diagnostic text; skip redaction entirely (nothing secret to mask).
	if c := filepath.Clean(dir); c == "." || c == string(filepath.Separator) {
		return s
	}
	s = strings.ReplaceAll(s, dir, redactedPath)
	for p := filepath.Clean(dir); p != "." && p != string(filepath.Separator); {
		s = strings.ReplaceAll(s, p, redactedPath)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return s
}

// redactPathErr wraps err so its rendered text carries no report-dir-derived
// path while errors.Is/As classification (context cancellation, fs errnos,
// sentinel errors) still walks the original chain. An err whose text is
// already clean is returned unchanged.
func redactPathErr(dir string, err error) error {
	if err == nil {
		return nil
	}
	msg := redactPathText(dir, err.Error())
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, cause: err}
}

// redactedError renders a redacted message while unwrapping to the original
// cause, so errors.Is/As classification survives the redaction. Callers
// format errors with %v/%w (which read Error()), so the redacted text is what
// reaches logs; only an explicit errors.As excavation could reach the
// original path-bearing text, and no report consumer does that.
type redactedError struct {
	cause error
	msg   string
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// redactingLogger wraps log so every record it emits - including atomicfile's
// own WithLogger diagnostics, which carry temp/target paths the app never
// formats itself - has the configured report dir redacted out of its message
// and its string/error attributes.
func redactingLogger(log *slog.Logger, dir string) *slog.Logger {
	return slog.New(&redactingHandler{inner: log.Handler(), dir: dir})
}

// redactingHandler is the slog.Handler behind redactingLogger: a pass-through
// to the wrapped handler that rewrites string-valued attributes, error-valued
// attributes (re-emitted as their redacted text), group members, and the
// record message through redactPathText.
type redactingHandler struct {
	inner slog.Handler
	dir   string
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

//nolint:gocritic // hugeParam: the by-value slog.Record is the slog.Handler interface signature.
func (h *redactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, redactPathText(h.dir, rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = h.redactAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(red), dir: h.dir}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name), dir: h.dir}
}

// redactAttr rewrites one attribute: string values are redacted in place,
// error values are flattened to their redacted text (an *os.PathError's
// rendered form carries the full path), and groups recurse. Other kinds
// (ints, times, durations) cannot carry the dir and pass through.
func (h *redactingHandler) redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redactPathText(h.dir, v.String()))
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, redactPathText(h.dir, err.Error()))
		}
		return a
	case slog.KindGroup:
		members := v.Group()
		red := make([]slog.Attr, len(members))
		for i, m := range members {
			red[i] = h.redactAttr(m)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(red...)}
	default:
		return a
	}
}

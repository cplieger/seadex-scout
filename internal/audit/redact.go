package audit

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
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

// minRedactablePath is the shortest cleaned report.dir worth masking. The
// value this guard exists for is a pasted secret (an arr api key, the
// Prowlarr key, the AB passkey - all far longer), while a short
// separator-less token is substring-replaced INSIDE unrelated words of the
// diagnostic text, so masking it corrupts the records instead of protecting
// anything.
const minRedactablePath = 8

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
	// A dir with nothing maskable in it is skipped entirely: a degenerate
	// value ("." or "/") would rewrite every dot or slash, and a short
	// separator-less value would rewrite the letters of unrelated words -
	// including the alert-keyed "report written" message - while being far too
	// short to be one of this app's secrets.
	c := filepath.Clean(dir)
	if !redactablePath(c) {
		return s
	}
	if redactablePath(dir) {
		s = strings.ReplaceAll(s, dir, redactedPath)
	}
	for p := c; redactablePath(p); {
		s = strings.ReplaceAll(s, p, redactedPath)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return s
}

// redactablePath reports whether p is a path fragment worth substring-masking:
// not degenerate, and either long enough to be a pasted secret or clearly
// path-shaped (it carries a separator, so it cannot collide with prose).
func redactablePath(p string) bool {
	if p == "." || p == string(filepath.Separator) {
		return false
	}
	return len(p) >= minRedactablePath || strings.ContainsRune(p, filepath.Separator)
}

// RedactReportDirErr wraps err so its rendered text carries no
// report-dir-derived path, while errors.Is/As classification still walks the
// original chain. It is this package's report.dir redaction policy, exported for
// the composition root: the report lock lives in internal/cycle (the one home of
// exclusive-run coordination) and its errors can carry the configured dir, which
// config.Load may have expanded from an allowlisted ${SEADEX_SCOUT_*} reference -
// so a paste typo must not ship a secret to main's error log.
func RedactReportDirErr(dir string, err error) error {
	return redactPathErr(dir, err)
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
// rendered form carries the full path), and groups recurse. Every other
// value passes through unchanged: numeric/time kinds cannot carry the dir,
// and a non-error KindAny value is deliberately not flattened (pinned by
// the passthrough test) - no report-pipeline caller logs the dir inside
// such a value, and flattening would change the record's wire shape.
func (h *redactingHandler) redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redactPathText(h.dir, v.String()))
	case slog.KindAny:
		// A typed-nil error (a non-nil interface holding a nil pointer)
		// would panic in Error(); leave it to the wrapped handler, which
		// renders it without calling Error() (and it can carry no path).
		if err, ok := v.Any().(error); ok && err != nil && !isNilErrValue(err) {
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

// isNilErrValue reports whether err is a non-nil error interface holding a
// nil pointer/map/slice value, whose Error() method would dereference nil.
func isNilErrValue(err error) bool {
	rv := reflect.ValueOf(err)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func:
		return rv.IsNil()
	default:
		return false
	}
}

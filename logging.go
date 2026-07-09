package main

import (
	"log/slog"
	"os"
)

// logLevel backs the slog handler so the level can be set once config is read
// without reinstalling the handler for a level change.
var logLevel = new(slog.LevelVar)

// installLogger installs the initial JSON handler on stdout before config is
// read, so even config-parse warnings emit as structured JSON. The level starts
// at the LevelVar default (Info) until configureLogger sets it.
func installLogger() {
	slog.SetDefault(slog.New(newHandler("json")))
}

// configureLogger applies the configured level and, when LOG_FORMAT=text,
// swaps the JSON handler for a text one (sharing the same LevelVar). All
// handlers render the record time in UTC.
func configureLogger(level slog.Level, format string) {
	logLevel.Set(level)
	slog.SetDefault(slog.New(newHandler(format)))
}

// newHandler builds a stdout slog handler for the given format ("text" for the
// text handler, JSON otherwise), leveled by logLevel and UTC-normalized.
func newHandler(format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: logLevel, ReplaceAttr: utcTimeAttr}
	if format == "text" {
		return slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.NewJSONHandler(os.Stdout, opts)
}

// utcTimeAttr renders the record's built-in time key in UTC so log timestamps
// are zone-stable regardless of the container TZ (the fleet logs-in-UTC
// standard). It rewrites only the top-level time attribute.
func utcTimeAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}

package main

import (
	"log/slog"
	"os"

	"github.com/cplieger/slogx"
)

// installLogger installs the initial JSON handler on stdout before config is
// read, so even config-parse warnings emit as structured JSON. The level starts
// at Info until configureLogger applies the configured level (and format).
func installLogger() {
	slogx.Setup(slogx.Options{Format: slogx.JSON, Output: os.Stdout})
}

// configureLogger installs the final handler once config is read: it applies the
// configured level and, when format is "text", swaps the JSON handler for a text
// one. Both render the record time in UTC (via slogx.UTCTime).
func configureLogger(level slog.Level, format string) {
	f := slogx.JSON
	if format == "text" {
		f = slogx.Text
	}
	slogx.Setup(slogx.Options{Format: f, Output: os.Stdout, Level: level})
}

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

// configureLogger installs the final handler once config is read: it applies
// the configured level and typed format (parsed by internal/config via
// slogx.ParseFormat). Both formats render the record time in UTC (via
// slogx.UTCTime).
func configureLogger(level slog.Level, format slogx.Format) {
	slogx.Setup(slogx.Options{Format: format, Output: os.Stdout, Level: level})
}

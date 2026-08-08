package audit

import (
	"log/slog"

	"github.com/cplieger/seadex-scout/internal/pathredact"
)

// redactedPath is the marker substituted for the configured report directory
// (and any path derived from it) in report-pipeline logs and returned errors.
// report.dir is a secret-capable config value: config.Load expands any
// allowlisted ${SEADEX_SCOUT_*} reference in every string field, so a paste
// typo such as `report.dir: ${SEADEX_SCOUT_AB_PASSKEY}` makes a passkey the
// effective directory. Filesystem calls keep the real path; only the
// diagnostics that cross into slog (shipped to Loki) or main's error log are
// redacted.
//
// The MECHANISM lives in internal/pathredact (a leaf the composition root
// imports too, for the report-lock errors internal/cycle returns); what stays
// here is this package's policy: which marker the report pipeline spells, and
// where it applies it.
const redactedPath = pathredact.ReportDirMarker

// redactPathErr wraps err so its rendered text carries no report-dir-derived
// path while errors.Is/As classification (context cancellation, fs errnos,
// sentinel errors) still walks the original chain.
func redactPathErr(dir string, err error) error {
	return pathredact.Err(dir, redactedPath, err)
}

// redactingLogger wraps log so every record it emits - including atomicfile's
// own WithLogger diagnostics, which carry temp/target paths the app never
// formats itself - has the configured report dir redacted out of its message
// and its string/error attributes.
func redactingLogger(log *slog.Logger, dir string) *slog.Logger {
	return pathredact.Logger(log, dir, redactedPath)
}

package pathredact

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestErrRedactsMessageAndPreservesCause pins Err's documented errors.Is/As
// contract, not just its rendered text: the redacted wrapper must keep the
// original cause reachable so shutdown/errno classification survives the
// masking. A future simplification that preserves the redacted message but
// drops the cause would keep the rendered-text redaction tests green while
// silently breaking errors.Is(os.ErrPermission) and errors.As(*os.PathError)
// for every consumer.
func TestErrRedactsMessageAndPreservesCause(t *testing.T) {
	const dir = "/config/sekret-passkey/reports"
	cause := &os.PathError{Op: "open", Path: dir + "/report.json", Err: os.ErrPermission}

	got := Err(dir, cause)

	if got == nil {
		t.Fatal("Err() = nil, want a wrapped error")
	}
	if strings.Contains(got.Error(), "sekret-passkey") {
		t.Errorf("Err() leaked the masked dir in %q", got)
	}
	if !strings.Contains(got.Error(), ReportDirMarker) {
		t.Errorf("Err() = %q, want the %q marker", got, ReportDirMarker)
	}
	if !errors.Is(got, os.ErrPermission) {
		t.Errorf("errors.Is(Err(), os.ErrPermission) = false")
	}
	var pathErr *os.PathError
	if !errors.As(got, &pathErr) || pathErr != cause {
		t.Errorf("errors.As(Err(), *os.PathError) = %v, want original cause %v", pathErr, cause)
	}
	if Err(dir, nil) != nil {
		t.Error("Err(nil) must remain nil")
	}
	clean := errors.New("clean diagnostic")
	if unchanged := Err(dir, clean); unchanged != clean {
		t.Errorf("Err(clean error) = %v, want the original error identity", unchanged)
	}
}

// TestTextGuards pins Text's documented guard branches: an empty dir redacts
// nothing (there is no value to mask), a degenerate dir ("." or "/") skips
// redaction entirely (replacing it would rewrite every dot or slash in the
// diagnostic text), and a real dir is masked along with its path-prefix
// ancestors (an os.PathError for a failed intermediate MkdirAll component
// carries an ancestor, not the full dir).
func TestTextGuards(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		in   string
		want string
	}{
		{"empty dir redacts nothing", "", "open /config/reports/report.json: denied", "open /config/reports/report.json: denied"},
		{"degenerate dot dir leaves dots alone", ".", "read report.json: unexpected EOF", "read report.json: unexpected EOF"},
		{"degenerate root dir leaves slashes alone", "/", "mkdir /config/reports: denied", "mkdir /config/reports: denied"},
		{"unclean degenerate dir is still skipped", "//", "mkdir /config/reports: denied", "mkdir /config/reports: denied"},
		{"configured dir is masked", "/config/sekret/reports", "open /config/sekret/reports/report.json: denied", "open " + ReportDirMarker + "/report.json: denied"},
		{"ancestor of the dir is masked", "/config/sekret/reports", "mkdir /config/sekret: denied", "mkdir " + ReportDirMarker + ": denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.dir, tt.in); got != tt.want {
				t.Errorf("Text(%q, %q) = %q, want %q", tt.dir, tt.in, got, tt.want)
			}
		})
	}
}

// TestTextKeepsShortSeparatorlessDirIntact pins the minRedactablePath floor,
// which is the one thing standing between a relative configured path and
// corrupted diagnostics. A relative report.dir loads with only a WARN
// (config.warnRelativeReportDir), so a value like "reports" reaches this
// masker, and substring-replacing it would rewrite the letters of unrelated
// words - including the alert-keyed "report written" message a Loki rule
// matches on. The masking half stays pinned by the rows above; these rows pin
// the refusal to mask, on both sides of the length floor and for a short value
// that IS path-shaped.
func TestTextKeepsShortSeparatorlessDirIntact(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		in   string
		want string
	}{
		{
			name: "short separator-less dir leaves its own diagnostic intact",
			dir:  "reports",
			in:   `report.dir "reports" is not an absolute path; reports were not written`,
			want: `report.dir "reports" is not an absolute path; reports were not written`,
		},
		{
			name: "short separator-less dir does not garble the alert-keyed message",
			dir:  "report",
			in:   "report written",
			want: "report written",
		},
		{
			name: "separator-less dir at the length floor is masked",
			dir:  "reportdir",
			in:   "open reportdir/report.json: denied",
			want: "open " + ReportDirMarker + "/report.json: denied",
		},
		{
			name: "short dir carrying a separator is masked",
			dir:  "out/rpt",
			in:   "open out/rpt/report.json: denied",
			want: "open " + ReportDirMarker + "/report.json: denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.dir, tt.in); got != tt.want {
				t.Errorf("Text(%q, %q) = %q, want %q", tt.dir, tt.in, got, tt.want)
			}
		})
	}
}

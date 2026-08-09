package config

import (
	"strings"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// generatedShapeWarn is the exact message warnUnexpectedAPIKeyShape emits. The
// tests below count it exactly rather than by substring: the point of the warning
// is that it fires once, for the right field, and not at all for a key that is
// the generated shape.
const generatedShapeWarn = "api key is not the shape Sonarr/Radarr/Prowlarr generate " +
	"(32 hex characters); it is accepted, but a truncated or mistyped paste looks " +
	"exactly like this and every call to that upstream would fail to authenticate"

// TestWellFormedCredential pins the ONE shape rule every gated credential field
// shares. The '$' leg is the load-bearing one: it is what makes the gate closed
// against every unexpanded-reference spelling - braced, brace-less, embedded, and
// the unterminated paste no reference regex matches - without the app enumerating
// them.
func TestWellFormedCredential(t *testing.T) {
	for name, tc := range map[string]struct {
		val  string
		want bool
	}{
		"generated arr key":         {testArrAPIKey, true},
		"hex of another length":     {strings.Repeat("ab", 8), true},
		"punctuation but no dollar": {"p@ssw0rd!#%^&*()", true},
		"upper-case hex":            {strings.Repeat("AB", 16), true},
		"empty":                     {"", false},
		"braced ref":                {"${SEADEX_SCOUT_SONARR_KEY}", false},
		"brace-less ref":            {"$SEADEX_SCOUT_SONARR_KEY", false},
		"unterminated braced ref":   {"${SEADEX_SCOUT_SONARR_KEY", false},
		"ref embedded in a value":   {"pre-${VAR}-post", false},
		"bare dollar":               {"abc$def", false},
		"leading space":             {" " + testArrAPIKey, false},
		"inner space":               {"two words", false},
		"trailing newline":          {testArrAPIKey + "\n", false},
		"tab":                       {"a\tb", false},
		"control rune":              {"a\x00b", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := wellFormedCredential(tc.val); got != tc.want {
				t.Errorf("wellFormedCredential(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestGeneratedArrAPIKey pins the GENERATOR shape read from Sonarr/Radarr/
// Prowlarr's shared ConfigFileProvider.GenerateApiKey: a .NET Guid in its
// default lower-case "D" format with the hyphens stripped, so 32 lower-case hex
// characters. Upper-case hex and the hyphenated Guid are deliberately NOT the
// generated shape - they are plausible operator values, which is why this
// predicate only drives a warning.
func TestGeneratedArrAPIKey(t *testing.T) {
	for name, tc := range map[string]struct {
		val  string
		want bool
	}{
		"generated shape":       {testArrAPIKey, true},
		"all zeroes":            {strings.Repeat("0", 32), true},
		"all f":                 {strings.Repeat("f", 32), true},
		"upper-case hex":        {strings.Repeat("AB", 16), false},
		"hyphenated guid":       {"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", false},
		"31 chars":              {strings.Repeat("0f1e2d3c", 4)[:31], false},
		"33 chars":              {strings.Repeat("0f1e2d3c", 4) + "0", false},
		"non-hex letter":        {strings.Repeat("0f1e2d3g", 4), false},
		"empty":                 {"", false},
		"32 non-hex characters": {strings.Repeat("z", 32), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := generatedArrAPIKey(tc.val); got != tc.want {
				t.Errorf("generatedArrAPIKey(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// apiKeyField names one gated credential field plus a config builder that sets
// only that field's key, so one table can drive the same assertions across all
// three.
type apiKeyField struct {
	build func(key string) Config
	field string
}

// gatedAPIKeyFields enumerates the three fields that gained a hard shape gate.
// indexer.prowlarr_api_key is deliberately built WITHOUT a torznab url: its gate
// is unconditional, so it must decide on a config validateIndexer never reaches.
func gatedAPIKeyFields() []apiKeyField {
	return []apiKeyField{
		{field: "sonarr.api_key", build: func(key string) Config {
			return Config{RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: key}
		}},
		{field: "radarr.api_key", build: func(key string) Config {
			return Config{RunMode: RunModeDaemon, RadarrURL: "http://radarr:7878", RadarrAPIKey: key}
		}},
		{field: "indexer.prowlarr_api_key", build: func(key string) Config {
			return Config{
				RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989",
				SonarrAPIKey: testArrAPIKey, IndexerProwlarrAPIKey: key,
			}
		}},
	}
}

// TestValidateRejectsMalformedAPIKeys pins the hard gate on the three arr and
// Prowlarr keys: a value carrying a '$', whitespace, or a control rune fails the
// config, while the 32-hex key the upstreams generate is accepted. The error
// names the field and the remedy and never echoes the value (CWE-532; the whole
// error battery in this file is field-name-only because a ${VAR} typo can place
// an expanded secret in any position).
func TestValidateRejectsMalformedAPIKeys(t *testing.T) {
	rejected := map[string]string{
		"braced env ref":          "${SEADEX_SCOUT_KEY_SENTINEL}",
		"brace-less env ref":      "$SEADEX_SCOUT_KEY_SENTINEL",
		"unterminated braced ref": "${SEADEX_SCOUT_KEY_SENTINEL",
		"dollar inside a value":   "sentinel$value",
		"inner space":             "sentinel value",
		"leading space":           " sentinel",
		"trailing newline":        "sentinel\n",
		"tab":                     "sen\tinel",
		"control rune":            "sen\x01inel",
	}
	for _, f := range gatedAPIKeyFields() {
		t.Run(f.field, func(t *testing.T) {
			for name, key := range rejected {
				t.Run(name, func(t *testing.T) {
					cfg := f.build(key)
					err := cfg.Validate()
					if err == nil {
						t.Fatalf("Validate() = nil, want a hard error for %s = %q", f.field, key)
					}
					if !strings.Contains(err.Error(), f.field) {
						t.Errorf("Validate() error = %q, want it to name %s", err, f.field)
					}
					if strings.Contains(err.Error(), "sentinel") ||
						strings.Contains(err.Error(), "SENTINEL") {
						t.Errorf("Validate() error echoes the configured key value: %q", err)
					}
					if !strings.Contains(err.Error(), "32-character hex key") {
						t.Errorf("Validate() error = %q, want it to name the real remedy "+
							"(the 32-character hex key the arrs generate)", err)
					}
				})
			}
			t.Run("generated key accepted", func(t *testing.T) {
				cfg := f.build(testArrAPIKey)
				if err := cfg.Validate(); err != nil {
					t.Errorf("Validate() = %v, want nil for the 32-hex key the upstreams generate", err)
				}
			})
		})
	}
}

// TestValidateRejectionNamesEnvRefOnlyForDollar pins that the unexpanded-reference
// HINT is keyed on the '$' character rather than on a reference regex: the charset
// rule is what refused the value, so the hint must be read from the same fact. A
// whitespace rejection carries no env-ref hint (it is not a placeholder), and the
// unterminated "${NAME" paste - which matches no well-formed reference grammar -
// still gets one.
func TestValidateRejectionNamesEnvRefOnlyForDollar(t *testing.T) {
	const hint = "environment-variable reference left unexpanded"
	for name, tc := range map[string]struct {
		key      string
		wantHint bool
	}{
		"braced ref":              {"${SEADEX_SCOUT_MISSING}", true},
		"unterminated braced ref": {"${SEADEX_SCOUT_MISSING", true},
		"brace-less ref":          {"$SEADEX_SCOUT_MISSING", true},
		"dollar then lower-case":  {"abc$def", true},
		"inner space only":        {"two words", false},
		"control rune only":       {"a\x01b", false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: tc.key,
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want a hard error for %q", tc.key)
			}
			if got := strings.Contains(err.Error(), hint); got != tc.wantHint {
				t.Errorf("env-ref hint present = %v, want %v; error = %q", got, tc.wantHint, err)
			}
		})
	}
}

// TestValidateWarnsOnNonGeneratedAPIKeyShape pins the warn-and-never-error half:
// a key that PASSES the shape gate but is not 32 lower-case hex is not what any
// of the three upstreams generates, so it is probably a truncated paste - but the
// same upstreams accept an operator-supplied key of any shape (their only check on
// SONARR__AUTH__APIKEY is IsNullOrWhiteSpace), so it must not refuse the config.
// Exactly one warning, naming the field, never echoing the key; none at all for
// the generated shape.
func TestValidateWarnsOnNonGeneratedAPIKeyShape(t *testing.T) {
	const sentinel = "shortkeysentinel"
	for _, f := range gatedAPIKeyFields() {
		t.Run(f.field, func(t *testing.T) {
			t.Run("non-generated shape warns once", func(t *testing.T) {
				rec := capture.Default(t)
				cfg := f.build(sentinel)
				if err := cfg.Validate(); err != nil {
					t.Fatalf("Validate: %v", err)
				}
				if n := rec.CountExact(generatedShapeWarn); n != 1 {
					t.Fatalf("generated-shape warnings = %d, want exactly 1: %v", n, rec.Messages())
				}
				if !rec.AttrContains(generatedShapeWarn, "field", f.field) {
					t.Errorf("generated-shape warning does not name %s: %v", f.field, rec.Messages())
				}
				if strings.Contains(strings.Join(rec.Messages(), "\n"), sentinel) ||
					rec.AttrContains(generatedShapeWarn, "", sentinel) {
					t.Errorf("generated-shape warning echoes the key value: %v", rec.Messages())
				}
			})
			t.Run("generated shape stays silent", func(t *testing.T) {
				rec := capture.Default(t)
				cfg := f.build(testArrAPIKey)
				if err := cfg.Validate(); err != nil {
					t.Fatalf("Validate: %v", err)
				}
				if n := rec.CountExact(generatedShapeWarn); n != 0 {
					t.Errorf("generated-shape warnings = %d, want 0 for the 32-hex generated key: %v",
						n, rec.Messages())
				}
			})
		})
	}
}

// TestValidateWarnsPerNonGeneratedArrKey pins the per-field grain: two arrs whose
// keys are both non-generated produce two warnings, one per field, rather than a
// single aggregate line the operator cannot act on.
func TestValidateWarnsPerNonGeneratedArrKey(t *testing.T) {
	rec := capture.Default(t)
	c := Config{
		RunMode:      RunModeDaemon,
		SonarrURL:    "http://sonarr:8989",
		SonarrAPIKey: "sonarrshortkey",
		RadarrURL:    "http://radarr:7878",
		RadarrAPIKey: "radarrshortkey",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n := rec.CountExact(generatedShapeWarn); n != 2 {
		t.Fatalf("generated-shape warnings = %d, want 2 (one per arr): %v", n, rec.Messages())
	}
	for _, field := range []string{"sonarr.api_key", "radarr.api_key"} {
		if !rec.AttrContains(generatedShapeWarn, "field", field) {
			t.Errorf("no generated-shape warning naming %s: %v", field, rec.Messages())
		}
	}
}

// TestValidateAcceptsEmptyProwlarrKey pins the one field whose EMPTY value is
// legitimate: Prowlarr with auth "Disabled for Local Addresses" needs no key, so
// the shape gate must not turn the existing empty-key WARN into a refusal, and it
// must not emit the generated-shape warning for a value that is simply absent.
func TestValidateAcceptsEmptyProwlarrKey(t *testing.T) {
	rec := capture.Default(t)
	c := Config{
		RunMode: RunModeDaemon, SonarrURL: "http://sonarr:8989", SonarrAPIKey: testArrAPIKey,
		IndexerNyaaTorznabURL: "http://prowlarr:9696/22/api",
		IndexerAPIKey:         testArrAPIKey,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n := rec.CountExact(generatedShapeWarn); n != 0 {
		t.Errorf("generated-shape warnings = %d, want 0 for an absent prowlarr key: %v",
			n, rec.Messages())
	}
	if !rec.Contains("indexer.prowlarr_api_key is empty") {
		t.Errorf("the empty-prowlarr-key warning is gone: %v", rec.Messages())
	}
}

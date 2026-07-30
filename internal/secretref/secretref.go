// Package secretref is the one home for the rule "is this configured secret
// still an unexpanded environment-variable reference, and can it be used".
//
// Two packages judge that question and used to do it independently, with
// different answers:
//
//   - internal/config's boot diagnostics (warnUnexpandedSecretRefs,
//     validateIndexerEndpoints) matched BOTH spellings an operator can leave
//     behind: the braced ${VAR} form yamlenv expands only for an allowlisted
//     name, and the shell-style $NAME form docker compose itself accepts,
//     making it a plausible paste.
//   - internal/indexer's usability guards (unusableFeedKey, unusableABPasskey)
//     matched only `strings.Contains(v, "${")`.
//
// That difference was a live defect, not a stylistic split. An unbraced
// $SEADEX_SCOUT_AB_PASSKEY warned at startup and was then treated as a USABLE
// passkey: url.PathEscape minted the literal placeholder into every AnimeBytes
// download link, so each arr grab failed at the tracker while the feed reported
// success - exactly the outcome config's warning describes and the indexer's
// guard exists to prevent. The braced form was caught by both; the brace-less
// form by only one.
//
// Why one home rather than exporting from internal/config: only the composition
// root imports internal/config, and internal/indexer deliberately takes no
// dependency on it (see internal/indexer/prowlarr.go), so a pure leaf both may
// import is the only home that keeps that rule intact. This is the same shape
// and the same reasoning as internal/credname, which owns the neighbouring
// question of which parameter NAME denotes a credential; the two are kept apart
// because a name's meaning and a value's state are different rules with
// different consumers.
package secretref

import "regexp"

// refRe matches an unexpanded environment-variable reference in either
// spelling: any braced ${...} form, or the shell-style $NAME. The brace-less arm
// is upper-case-only (the convention every allowlisted prefix follows), so a hex
// or base64 secret carrying a stray '$' does not trip it.
var refRe = regexp.MustCompile(`\$\{[^}]*\}|\$[A-Z_][A-Z0-9_]*`)

// Unexpanded reports whether v still CONTAINS an environment-variable
// reference in either spelling. It is the diagnostic policy: a secret carrying a
// placeholder anywhere is worth warning about, because the literal is what gets
// sent as the credential.
func Unexpanded(v string) bool { return refRe.MatchString(v) }

// IsWholeRef reports whether v is ENTIRELY one environment-variable reference.
// It is the stricter reading a gate uses when the whole value must be the
// reference for the diagnostic to be honest: a generated credential cannot take
// that shape, since no hex or base64 output is a single $NAME or ${NAME}.
func IsWholeRef(v string) bool { return refRe.FindString(v) == v && v != "" }

// Unusable reports whether a configured secret cannot be used at all: it is
// absent, or it still holds a placeholder in either spelling. Callers that must
// fail closed on an unusable credential read this rather than testing for a
// brace, so both spellings take the same path.
func Unusable(v string) bool { return v == "" || Unexpanded(v) }

// Package secretref is the one home for the rule "is this configured secret
// still an unexpanded environment-variable reference, and can it be used".
package secretref

import "regexp"

// refRe matches an unexpanded environment-variable reference in either
// spelling: a braced ${...} form whose closing brace is OPTIONAL, or the
// shell-style $NAME. The brace-less arm is upper-case-only (the convention every
// allowlisted prefix follows), so a hex or base64 secret carrying a stray '$'
// does not trip it.
var refRe = regexp.MustCompile(`\$\{[^}]*\}?|\$[A-Z_][A-Z0-9_]*`)

// Unexpanded reports whether v still CONTAINS an environment-variable
// reference in either spelling. It is the diagnostic policy: a secret carrying a
// placeholder anywhere is worth warning about, because the literal is what gets
// sent as the credential.
func Unexpanded(v string) bool { return refRe.MatchString(v) }

// Unusable reports whether a configured secret cannot be used at all: it is
// absent, or it still holds a placeholder in either spelling. Callers that must
// fail closed on an unusable credential read this rather than testing for a
// brace, so both spellings take the same path.
func Unusable(v string) bool { return v == "" || Unexpanded(v) }

package logcontract

import (
	"os"
	"slices"
	"testing"
)

const header = `# preamble
#   level=ERROR with condition=...  a lasting condition
#     attrs: condition, arr
#   msg="one"                       first message
#     attrs: a, b
#     attrs: c
#   msg="two"                       no attributes
#                                   msg="prose mention" is not an entry
#   msg="three"
#     attrs: d
groups:
#   msg="after the header"
#     attrs: z
`

// TestParseAttachesAttrsToTheirEntry pins the grammar: attrs: lines belong to
// the nearest entry above, accumulate across lines, the level=ERROR entry
// feeds Global, and nothing past the leading comment counts.
func TestParseAttachesAttrsToTheirEntry(t *testing.T) {
	c, err := Parse([]byte(header))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string][]string{"one": {"a", "b", "c"}, "two": nil, "three": {"d"}}
	if len(c.Messages) != len(want) {
		t.Errorf("Parse messages = %v, want %v", c.Messages, want)
	}
	for msg, attrs := range want {
		if got, ok := c.Messages[msg]; !ok || !slices.Equal(got, attrs) {
			t.Errorf("Parse messages[%q] = %v (present %v), want %v", msg, got, ok, attrs)
		}
	}
	if !slices.Equal(c.Global, []string{"msg", "level", "condition", "arr"}) {
		t.Errorf("Parse global = %v, want [msg level condition arr]", c.Global)
	}
	if !c.Allows("one", "b") || !c.Allows("two", "arr") || c.Allows("two", "a") || c.Allows("", "d") {
		t.Errorf("Allows disagrees with the parsed contract %+v", c)
	}
}

// TestParseRejectsAFileWithoutAContract pins that a header with no attrs:
// line is an error rather than an empty contract every consumer passes.
func TestParseRejectsAFileWithoutAContract(t *testing.T) {
	if _, err := Parse([]byte("#   msg=\"one\"\ngroups:\n")); err == nil {
		t.Error("Parse(no attrs) = nil error, want errNoContract")
	}
}

// TestParseShippedRules pins that the shipped file parses into a contract
// carrying the messages its own rules key on.
func TestParseShippedRules(t *testing.T) {
	raw, err := os.ReadFile("../../alerts/logql.yaml")
	if err != nil {
		t.Fatalf("read alerts/logql.yaml: %v", err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(alerts/logql.yaml): %v", err)
	}
	for _, msg := range []string{"better release available", "reconcile complete", "indexer request", "library summary"} {
		if _, ok := c.Messages[msg]; !ok {
			t.Errorf("shipped contract lacks %q", msg)
		}
	}
	if !c.Allows("indexer request", "feed") || !c.Allows("", "condition") {
		t.Errorf("shipped contract = %+v, want feed on indexer request and condition global", c)
	}
}

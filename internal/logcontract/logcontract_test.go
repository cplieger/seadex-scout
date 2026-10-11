package logcontract

import (
	"slices"
	"testing"
)

const page = "# Monitoring\n" +
	"\n" +
	"| Message | Logged when | Stable attributes |\n" +
	"| --- | --- | --- |\n" +
	"| `before` | a table outside the section | `y` |\n" +
	"\n" +
	"### Log contract\n" +
	"\n" +
	"| Message | Logged when | Stable attributes |\n" +
	"| --- | --- | --- |\n" +
	"| `level=ERROR`, no `condition` | a one-off fault | |\n" +
	"| `level=ERROR` with `condition` | a lasting condition | `condition`, `arr` |\n" +
	"| `one` | first message, citing `prose` | `a`, `b`, `c` |\n" +
	"| `two` | no attributes | |\n" +
	"| `three, with a comma` | | `d` |\n" +
	"\n" +
	"Prose naming `not an entry`.\n" +
	"\n" +
	"### Next section\n" +
	"\n" +
	"| Message | Logged when | Stable attributes |\n" +
	"| --- | --- | --- |\n" +
	"| `after` | past the section | `z` |\n"

// TestParseAttachesAttrsToTheirRow pins the grammar: the first cell's first
// code span names the row, the third cell's code spans are its attributes,
// the level=ERROR rows feed Global, and nothing outside the section counts.
func TestParseAttachesAttrsToTheirRow(t *testing.T) {
	c, err := Parse([]byte(page))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string][]string{"one": {"a", "b", "c"}, "two": nil, "three, with a comma": {"d"}}
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

// TestParseRejects pins that a page with no stable attribute, or a row the
// grammar cannot read, is an error rather than a contract every consumer passes.
func TestParseRejects(t *testing.T) {
	const head = "### Log contract\n\n| Message | Logged when | Stable attributes |\n| --- | --- | --- |\n"
	for name, raw := range map[string]string{
		"no section":         "| `one` | x | `a` |\n",
		"no attribute":       head + "| `one` | x | |\n",
		"row without a name": head + "| one | x | `a` |\n",
		"row with two cells": head + "| `one` | `a` |\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Errorf("Parse(%q) = nil error, want an error", raw)
			}
		})
	}
}

// TestLoadShippedContract pins that the shipped page parses into a contract
// carrying the messages the shipped rules key on.
func TestLoadShippedContract(t *testing.T) {
	c, err := Load("../..")
	if err != nil {
		t.Fatalf("Load: %v", err)
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

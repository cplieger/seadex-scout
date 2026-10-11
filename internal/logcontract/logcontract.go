// Package logcontract reads the stable log contract docs/monitoring.md
// publishes in its "Log contract" table: the messages consumers may match on
// and the attributes stable on each. Tests on both sides of the contract parse
// it, so an emitter and a consumer can only drift by failing one of them.
package logcontract

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Path is the contract's file, relative to the repository root.
const Path = "docs/monitoring.md"

// heading opens the section whose table is the contract; the next heading
// closes it.
const heading = "### Log contract"

// Contract is the parsed stable-message list.
type Contract struct {
	// Messages maps each stable msg to the attributes stable on it.
	Messages map[string][]string
	// Global holds the attributes stable on every line that carries them: msg,
	// level, and the attributes listed on the level=ERROR rows.
	Global []string
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

var (
	// errNoContract reports a page whose contract table lists no stable
	// attribute, which every consumer would otherwise pass vacuously.
	errNoContract = errors.New("logcontract: no row of the Log contract table carries a stable attribute")
	// errRow reports a table row whose first cell names no message or level.
	errRow = errors.New("logcontract: a Log contract row names no message in its first cell")
)

// Load reads and parses the contract under the repository root at root.
func Load(root string) (Contract, error) {
	raw, err := os.ReadFile(filepath.Join(root, Path))
	if err != nil {
		return Contract{}, err
	}
	return Parse(raw)
}

// Parse reads the contract out of the "Log contract" table of a
// docs/monitoring.md. Each row is | message | description | attributes |: the
// first code span of the first cell is the msg, or a level=ERROR row whose
// attributes are global, and every code span of the third cell is a stable
// attribute. The header and delimiter rows, and anything outside the table,
// are ignored.
func Parse(raw []byte) (Contract, error) {
	rows, err := sectionRows(raw)
	if err != nil {
		return Contract{}, err
	}
	c := Contract{Messages: map[string][]string{}, Global: []string{"msg", "level"}}
	sawAttrs := false
	for _, line := range rows {
		added, err := c.addRow(line)
		if err != nil {
			return Contract{}, err
		}
		sawAttrs = sawAttrs || added
	}
	if !sawAttrs {
		return Contract{}, errNoContract
	}
	return c, nil
}

// sectionRows returns the data rows of every table under the contract heading.
func sectionRows(raw []byte) ([]string, error) {
	var rows []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	inSection, row := false, 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			if inSection {
				break
			}
			inSection = line == heading
			continue
		}
		if !inSection || !strings.HasPrefix(line, "|") {
			row = 0
			continue
		}
		row++
		if row > 2 { // past the header and delimiter rows
			rows = append(rows, line)
		}
	}
	return rows, sc.Err()
}

// addRow records one table row and reports whether it listed an attribute.
func (c *Contract) addRow(line string) (bool, error) {
	cells := strings.Split(strings.Trim(line, "|"), "|")
	if len(cells) != 3 {
		return false, fmt.Errorf("logcontract: row %q has %d cells, want 3", line, len(cells))
	}
	name := codeSpan.FindStringSubmatch(cells[0])
	if name == nil {
		return false, fmt.Errorf("%w: %q", errRow, line)
	}
	global := strings.HasPrefix(name[1], "level=ERROR")
	if !global {
		if _, ok := c.Messages[name[1]]; !ok {
			c.Messages[name[1]] = nil
		}
	}
	attrs := codeSpan.FindAllStringSubmatch(cells[2], -1)
	for _, a := range attrs {
		if global {
			c.Global = appendUnique(c.Global, a[1])
		} else {
			c.Messages[name[1]] = appendUnique(c.Messages[name[1]], a[1])
		}
	}
	return len(attrs) > 0, nil
}

func appendUnique(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

// Allows reports whether key is a stable attribute on a line carrying msg. An
// empty msg allows only the Global attributes.
func (c *Contract) Allows(msg, key string) bool {
	return slices.Contains(c.Global, key) || slices.Contains(c.Messages[msg], key)
}

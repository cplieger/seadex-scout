// Package logcontract reads the stable log contract alerts/logql.yaml
// publishes in its header: the messages consumers may match on and the
// attributes stable on each. Tests on both sides of the contract parse it, so
// an emitter and a consumer can only drift by failing one of them.
package logcontract

import (
	"bufio"
	"bytes"
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Contract is the parsed stable-message list.
type Contract struct {
	// Messages maps each stable msg to the attributes stable on it.
	Messages map[string][]string
	// Global holds the attributes stable on every line that carries them: msg,
	// level, and the attrs listed under the level=ERROR entries.
	Global []string
}

var (
	msgLine   = regexp.MustCompile(`^# {3}msg="([^"]+)"`)
	levelLine = regexp.MustCompile(`^# {3}level=ERROR`)
	attrsLine = regexp.MustCompile(`^# {5}attrs: (.+)$`)
)

// errNoContract reports a file whose header carries no stable-message list.
var errNoContract = errors.New("logcontract: no stable message carries an attrs: line")

type parser struct {
	current  string
	c        Contract
	global   bool
	sawAttrs bool
}

// Parse reads the contract out of the leading comment of an alerts/logql.yaml.
// An attrs: line belongs to the nearest msg= or level=ERROR entry above it.
func Parse(raw []byte) (Contract, error) {
	p := parser{c: Contract{Messages: map[string][]string{}, Global: []string{"msg", "level"}}}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "#") {
			break
		}
		p.line(line)
	}
	if err := sc.Err(); err != nil {
		return Contract{}, err
	}
	if !p.sawAttrs {
		return Contract{}, errNoContract
	}
	return p.c, nil
}

func (p *parser) line(line string) {
	if m := msgLine.FindStringSubmatch(line); m != nil {
		p.current, p.global = m[1], false
		if _, ok := p.c.Messages[p.current]; !ok {
			p.c.Messages[p.current] = nil
		}
		return
	}
	if levelLine.MatchString(line) {
		p.current, p.global = "", true
		return
	}
	m := attrsLine.FindStringSubmatch(line)
	if m == nil || (p.current == "" && !p.global) {
		return
	}
	p.sawAttrs = true
	for a := range strings.SplitSeq(m[1], ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if p.global {
			p.c.Global = appendUnique(p.c.Global, a)
		} else {
			p.c.Messages[p.current] = appendUnique(p.c.Messages[p.current], a)
		}
	}
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

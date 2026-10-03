package scout

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/cplieger/seadex-scout/internal/config"
	"go.yaml.in/yaml/v3"
)

// The standing-condition rules in the shipped alerts/logql.yaml are the consumer
// of the degradation.Condition vocabulary. These tests read both sides at run
// time, so a condition added without a rule, a rule grouped by a per-emission
// value, or a window shorter than the gap between two passes fails here.

const alertRulesPath = "../../alerts/logql.yaml"

var (
	conditionRegexRe = regexp.MustCompile("condition=~`([^`]+)`")
	sumByRe          = regexp.MustCompile(`sum by \(([^)]*)\)`)
	lookbackRe       = regexp.MustCompile(`\[(\d+[smh])\]`)
	labelRefRe       = regexp.MustCompile(`\$labels\.([a-z_]+)`)
)

// standingRules names the two rules that own the condition vocabulary.
var standingRules = []string{"SeadexScoutUpstreamUnavailable", "SeadexScoutLibraryDegraded"}

type alertRule struct {
	Annotations map[string]string `yaml:"annotations"`
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
}

// shippedRules loads every rule in alerts/logql.yaml, keyed by alert name.
func shippedRules(t *testing.T) map[string]alertRule {
	t.Helper()
	raw, err := os.ReadFile(alertRulesPath)
	if err != nil {
		t.Fatalf("read %s: %v", alertRulesPath, err)
	}
	var doc struct {
		Groups []struct {
			Rules []alertRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", alertRulesPath, err)
	}
	rules := map[string]alertRule{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			rules[r.Alert] = r
		}
	}
	return rules
}

// rule returns the named rule or fails the test naming the missing contract.
func rule(t *testing.T, rules map[string]alertRule, name string) alertRule {
	t.Helper()
	r, ok := rules[name]
	if !ok {
		t.Fatalf("alerts/logql.yaml carries no %s rule", name)
	}
	return r
}

// declaredConditions parses internal/degradation and returns the value of every
// constant of type Condition, so a new value cannot be missed by this test.
func declaredConditions(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("../degradation/*.go")
	if err != nil {
		t.Fatalf("glob degradation sources: %v", err)
	}
	var values []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "Condition" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok {
						t.Fatalf("%s: a Condition constant is not a string literal", path)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: unquote %s: %v", path, lit.Value, err)
					}
					values = append(values, s)
				}
			}
		}
	}
	if len(values) == 0 {
		t.Fatal("internal/degradation declares no Condition constants")
	}
	return values
}

// ruleConditions returns the values a standing rule's condition=~ filter matches.
func ruleConditions(t *testing.T, r alertRule) []string {
	t.Helper()
	m := conditionRegexRe.FindStringSubmatch(r.Expr)
	if m == nil {
		t.Fatalf("%s matches no condition=~`...` filter:\n%s", r.Alert, r.Expr)
	}
	return strings.Split(m[1], "|")
}

// grouping returns a rule's sum by label set.
func grouping(t *testing.T, r alertRule) []string {
	t.Helper()
	m := sumByRe.FindStringSubmatch(r.Expr)
	if m == nil {
		t.Fatalf("%s carries no sum by (...) grouping:\n%s", r.Alert, r.Expr)
	}
	var labels []string
	for label := range strings.SplitSeq(m[1], ",") {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

// lookback returns a rule's range-vector window.
func lookback(t *testing.T, r alertRule) time.Duration {
	t.Helper()
	m := lookbackRe.FindStringSubmatch(r.Expr)
	if m == nil {
		t.Fatalf("%s carries no [window] lookback:\n%s", r.Alert, r.Expr)
	}
	d, err := time.ParseDuration(m[1])
	if err != nil {
		t.Fatalf("%s lookback %q: %v", r.Alert, m[1], err)
	}
	return d
}

// TestAlertRulesCoverEveryCondition pins the two standing rules against the
// condition vocabulary: together they match every declared condition and none
// twice, they group only by labels that stay constant while a condition holds,
// and their window spans several passes, because Loki's ruler has no
// keep_firing_for and a window shorter than the gap between two emissions
// resolves and re-fires the alert on every pass.
func TestAlertRulesCoverEveryCondition(t *testing.T) {
	rules := shippedRules(t)
	minWindow := 3 * config.DefaultPollInterval

	owner := map[string]string{}
	for _, name := range standingRules {
		r := rule(t, rules, name)
		for _, c := range ruleConditions(t, r) {
			if prev, dup := owner[c]; dup {
				t.Errorf("condition %q is matched by both %s and %s, want exactly one owner", c, prev, name)
			}
			owner[c] = name
		}
		labels := grouping(t, r)
		if !slices.Contains(labels, "condition") {
			t.Errorf("%s groups by %v, want condition in the set (one alert per condition)", name, labels)
		}
		for _, l := range labels {
			if l != "condition" && l != "arr" {
				t.Errorf("%s groups by %q, want only condition and arr (a per-emission value would mint a new alert every pass)", name, l)
			}
		}
		if d := lookback(t, r); d < minWindow {
			t.Errorf("%s lookback = %v, want at least %v (three passes at the default poll_interval)", name, d, minWindow)
		}
		for _, text := range r.Annotations {
			for _, m := range labelRefRe.FindAllStringSubmatch(text, -1) {
				if !slices.Contains(labels, m[1]) {
					t.Errorf("%s interpolates $labels.%s, which its sum by (%v) drops", name, m[1], labels)
				}
			}
		}
	}
	declared := declaredConditions(t)
	for _, c := range declared {
		if _, ok := owner[c]; !ok {
			t.Errorf("condition %q has no standing rule, want it matched by one of %v", c, standingRules)
		}
	}
	for c, name := range owner {
		if !slices.Contains(declared, c) {
			t.Errorf("%s matches condition %q, which internal/degradation does not declare", name, c)
		}
	}
}

// TestCycleErrorRuleExcludesStandingConditions pins the split: the one-off fault
// rule drops every line that names a condition, so a lasting outage fires only
// its standing rule, and its window outlasts a pass so a fault repeating on every
// pass notifies once.
func TestCycleErrorRuleExcludesStandingConditions(t *testing.T) {
	r := rule(t, shippedRules(t), "SeadexScoutCycleError")
	if !strings.Contains(r.Expr, "| condition=``") {
		t.Errorf("SeadexScoutCycleError does not filter condition=``, want it to exclude every standing-condition line:\n%s", r.Expr)
	}
	if d, minWindow := lookback(t, r), 3*config.DefaultPollInterval; d < minWindow {
		t.Errorf("SeadexScoutCycleError lookback = %v, want at least %v", d, minWindow)
	}
}

// renderAnnotation executes an annotation template the way the ruler does, with
// labels bound to $labels.
func renderAnnotation(t *testing.T, name, text string, labels map[string]string) string {
	t.Helper()
	tmpl, err := template.New(name).Parse("{{ $labels := .Labels }}" + text)
	if err != nil {
		t.Fatalf("%s annotation does not parse: %v", name, err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, struct{ Labels map[string]string }{labels}); err != nil {
		t.Fatalf("%s annotation does not render: %v", name, err)
	}
	return out.String()
}

// TestStandingRuleTextNamesEveryCondition pins that every condition a standing
// rule matches gets its own summary and description rather than the generic
// fallback, and that the fallback still names an unknown condition.
func TestStandingRuleTextNamesEveryCondition(t *testing.T) {
	const fallback = "reports a lasting"
	rules := shippedRules(t)
	for _, name := range standingRules {
		r := rule(t, rules, name)
		for _, field := range []string{"summary", "description"} {
			text, ok := r.Annotations[field]
			if !ok {
				t.Fatalf("%s carries no %s annotation", name, field)
			}
			for _, c := range ruleConditions(t, r) {
				out := renderAnnotation(t, name, text, map[string]string{"condition": c, "arr": "sonarr"})
				if strings.Contains(out, fallback) || strings.Contains(out, "<no value>") || strings.TrimSpace(out) == "" {
					t.Errorf("%s %s for condition %q renders the generic text, want its own:\n%s", name, field, c, out)
				}
			}
			unknown := renderAnnotation(t, name, text, map[string]string{"condition": "not-a-condition"})
			if !strings.Contains(unknown, "not-a-condition") {
				t.Errorf("%s %s for an unknown condition does not name it, want the fallback to:\n%s", name, field, unknown)
			}
		}
	}
}

// sentenceEndRe matches the end of a sentence in rendered annotation prose.
var sentenceEndRe = regexp.MustCompile(`[.!?](\s|$)`)

// TestAlertDescriptionsFitAParagraphBudget pins the public text budget on the
// rules whose descriptions carry the most guidance: every rendered paragraph
// holds at most five sentences and 100 words, so a notification stays readable
// on one pass.
func TestAlertDescriptionsFitAParagraphBudget(t *testing.T) {
	const maxWords, maxSentences = 100, 5
	rules := shippedRules(t)
	check := func(name, out string) {
		for para := range strings.SplitSeq(out, "\n") {
			if para = strings.TrimSpace(para); para == "" {
				continue
			}
			if words := len(strings.Fields(para)); words > maxWords {
				t.Errorf("%s description paragraph has %d words, want at most %d:\n%s", name, words, maxWords, para)
			}
			if n := len(sentenceEndRe.FindAllString(para, -1)); n > maxSentences {
				t.Errorf("%s description paragraph has %d sentences, want at most %d:\n%s", name, n, maxSentences, para)
			}
		}
	}
	fault := rule(t, rules, "SeadexScoutCycleError")
	check(fault.Alert, renderAnnotation(t, fault.Alert, fault.Annotations["description"], nil))
	for _, name := range standingRules {
		r := rule(t, rules, name)
		for _, c := range ruleConditions(t, r) {
			check(name+"/"+c, renderAnnotation(t, name, r.Annotations["description"], map[string]string{"condition": c, "arr": "sonarr"}))
		}
	}
}

package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/seadex-scout/internal/logcontract"
)

// The bundled grafana-dashboard.json is a consumer of the log contract
// alerts/logql.yaml publishes, so every message and attribute a panel reads
// must be in that contract, and the time windows must follow the operator's
// Grafana range rather than assume a retention.

const (
	dashboardPath  = "grafana-dashboard.json"
	alertRulesFile = "alerts/logql.yaml"
)

var (
	// pipelineRe captures one log pipeline: a stream selector, its stages, and
	// the range that closes it. No stage this dashboard uses contains a '['.
	pipelineRe   = regexp.MustCompile(`\{[^{}]*\}((?:[^\[{]|\{\{[^}]*\}\})*)\[([^\]]+)\]`)
	jsonStageRe  = regexp.MustCompile(`\|\s*json\b([^|]*)`)
	jsonParamRe  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([^"]*)"`)
	msgEqRe      = regexp.MustCompile("\\|\\s*msg\\s*=\\s*`([^`]*)`")
	msgReRe      = regexp.MustCompile("\\|\\s*msg\\s*=~\\s*`([^`]*)`")
	msgOtherRe   = regexp.MustCompile(`\|\s*msg\s*(!=|!~|=\s*")`)
	lineFilterRe = regexp.MustCompile("\\|=\\s*`([^`]*)`")
	regexLineRe  = regexp.MustCompile("(^|[\\s}])(\\|~|!~|!=)\\s*`")
	offsetRe     = regexp.MustCompile(`offset\s+([^\s)]+)`)
	regexMetaRe  = regexp.MustCompile(`[\\.*+?()\[\]{}^$]`)
	urlHostRe    = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://([^/"\s:?#]+)`)
	ipv4Re       = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
)

// allowedWindows are the fixed windows that encode seadex-scout's own cadence;
// anything reaching back into history must use Grafana's range.
var allowedWindows = []string{"$__range", "$__interval", "1h", "2h", "3h", "26h", "72h"}

type dashTarget struct {
	panel string
	expr  string
}

func loadContract(t *testing.T) logcontract.Contract {
	t.Helper()
	raw, err := os.ReadFile(alertRulesFile)
	if err != nil {
		t.Fatalf("read %s: %v", alertRulesFile, err)
	}
	c, err := logcontract.Parse(raw)
	if err != nil {
		t.Fatalf("parse the log contract in %s: %v", alertRulesFile, err)
	}
	return c
}

func loadDashboard(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("read %s: %v", dashboardPath, err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", dashboardPath, err)
	}
	return d
}

func dashboardTargets(d map[string]any) []dashTarget {
	var out []dashTarget
	var walk func(ps []any)
	walk = func(ps []any) {
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			title, _ := pm["title"].(string)
			ts, _ := pm["targets"].([]any)
			for _, tg := range ts {
				tm, _ := tg.(map[string]any)
				if expr, ok := tm["expr"].(string); ok {
					out = append(out, dashTarget{panel: title, expr: expr})
				}
			}
			nested, _ := pm["panels"].([]any)
			walk(nested)
		}
	}
	panels, _ := d["panels"].([]any)
	walk(panels)
	return out
}

// pipelineMessages returns the msg values one pipeline filters on, each a
// literal: an alternation regex is split on '|', and an alternative that is
// itself a regex is reported as a problem.
func pipelineMessages(stages string) (msgs, problems []string) {
	for _, m := range msgEqRe.FindAllStringSubmatch(stages, -1) {
		msgs = append(msgs, m[1])
	}
	for _, m := range msgReRe.FindAllStringSubmatch(stages, -1) {
		for alt := range strings.SplitSeq(m[1], "|") {
			if regexMetaRe.MatchString(alt) {
				problems = append(problems, "msg alternative "+alt+" is a regex, not a literal")
				continue
			}
			msgs = append(msgs, alt)
		}
	}
	if msgOtherRe.MatchString(stages) {
		problems = append(problems, "msg is filtered with an operator this check cannot read; use msg=`...` or msg=~`a|b`")
	}
	return msgs, problems
}

func checkPipeline(c *logcontract.Contract, stages, window string) []string {
	var problems []string
	if !slices.Contains(allowedWindows, window) {
		problems = append(problems, "window ["+window+"] is neither Grafana's range nor one of the app's cadence windows")
	}
	if regexLineRe.MatchString(stages) {
		problems = append(problems, "a regex or negative line filter cannot be checked; filter on msg instead")
	}
	msgs, msgProblems := pipelineMessages(stages)
	problems = append(problems, msgProblems...)
	for _, msg := range msgs {
		if _, ok := c.Messages[msg]; !ok {
			problems = append(problems, "msg "+msg+" is not a stable message")
		}
	}
	for _, m := range lineFilterRe.FindAllStringSubmatch(stages, -1) {
		if !slices.ContainsFunc(msgs, func(msg string) bool { return strings.Contains(msg, m[1]) }) {
			problems = append(problems, "line filter "+m[1]+" matches none of the pipeline's stable messages")
		}
	}
	for _, stage := range jsonStageRe.FindAllStringSubmatch(stages, -1) {
		params := jsonParamRe.FindAllStringSubmatch(stage[1], -1)
		if len(params) == 0 {
			problems = append(problems, "a bare | json stage extracts every attribute; name the keys")
		}
		for _, p := range params {
			key := p[2]
			if slices.ContainsFunc(msgs, func(msg string) bool { return c.Allows(msg, key) }) || c.Allows("", key) {
				continue
			}
			problems = append(problems, "json key "+key+" is not stable on "+strings.Join(msgs, ", "))
		}
	}
	return problems
}

func checkExpr(c *logcontract.Contract, expr string) []string {
	pipes := pipelineRe.FindAllStringSubmatch(expr, -1)
	if len(pipes) == 0 {
		return []string{"no log pipeline found"}
	}
	var problems []string
	for _, p := range pipes {
		problems = append(problems, checkPipeline(c, p[1], p[2])...)
	}
	for _, m := range offsetRe.FindAllStringSubmatch(expr, -1) {
		if m[1] != "$__range" {
			problems = append(problems, "offset "+m[1]+" is a fixed look-back; use $__range")
		}
	}
	return problems
}

func TestDashboardReadsOnlyTheLogContract(t *testing.T) {
	c := loadContract(t)
	targets := dashboardTargets(loadDashboard(t))
	if len(targets) == 0 {
		t.Fatalf("%s carries no query", dashboardPath)
	}
	for _, tg := range targets {
		for _, problem := range checkExpr(&c, tg.expr) {
			t.Errorf("panel %q: %s\nexpr: %s", tg.panel, problem, tg.expr)
		}
	}
}

// TestDashboardContractCheckRejects pins that the contract check can fail: each
// expression breaks the contract one way.
func TestDashboardContractCheckRejects(t *testing.T) {
	c := loadContract(t)
	const sel = `{container="$container"}`
	cases := map[string]string{
		"uncontracted msg":    sel + " | json msg=\"msg\" | msg=`made up message` [2h]",
		"uncontracted key":    sel + " | json msg=\"msg\", recommended_group=\"recommended_group\" | msg=`library summary` [26h]",
		"global-only key":     sel + " | json level=\"level\", title=\"title\" | level=`ERROR` [1h]",
		"bare json":           sel + " | json | msg=`library summary` [26h]",
		"fixed day window":    sel + " | json msg=\"msg\" | msg=`reconcile complete` [30d]",
		"fixed offset":        sel + " | json msg=\"msg\" | msg=`reconcile complete` [2h] offset 7d",
		"regex alternative":   sel + " | json msg=\"msg\" | msg=~`cycle .*` [3h]",
		"regex line filter":   sel + " |~ `reconcile (started|complete)` | json msg=\"msg\" | msg=`reconcile complete` [3h]",
		"foreign line filter": sel + " |= `findings reported` | json msg=\"msg\" | msg=`reconcile complete` [3h]",
		"no pipeline":         "vector(1)",
	}
	for name, expr := range cases {
		t.Run(name, func(t *testing.T) {
			if problems := checkExpr(&c, expr); len(problems) == 0 {
				t.Errorf("checkExpr(%q) reported nothing, want a contract violation", expr)
			}
		})
	}
	ok := sel + " |= `library summary` | json msg=\"msg\", have_best=\"have_best\" | msg=`library summary` | unwrap have_best [26h]"
	if problems := checkExpr(&c, ok); len(problems) != 0 {
		t.Errorf("checkExpr(%q) = %v, want no problem", ok, problems)
	}
}

// TestDashboardShape pins what makes the file importable into any deployment:
// a string uid, a datasource variable first and used by every query, a
// container variable, no fixed number of days, and no host but the SeaDex site.
func TestDashboardShape(t *testing.T) {
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("read %s: %v", dashboardPath, err)
	}
	d := loadDashboard(t)
	if uid, _ := d["uid"].(string); uid != "seadex-scout" {
		t.Errorf("uid = %v, want the string seadex-scout", d["uid"])
	}
	tmpl, _ := d["templating"].(map[string]any)
	vars, _ := tmpl["list"].([]any)
	var names []string
	for _, v := range vars {
		vm, _ := v.(map[string]any)
		name, _ := vm["name"].(string)
		names = append(names, name)
	}
	if !slices.Equal(names, []string{"datasource", "container"}) {
		t.Errorf("template variables = %v, want [datasource container]", names)
	}
	text := string(raw)
	if n, m := strings.Count(text, `"expr"`), strings.Count(text, `"uid": "${datasource}"`); m < n {
		t.Errorf("%d queries but %d datasource references to ${datasource}, want every panel and target on it", n, m)
	}
	for _, tg := range dashboardTargets(d) {
		if !strings.Contains(tg.expr, `container="$container"`) {
			t.Errorf("panel %q does not select on $container: %s", tg.panel, tg.expr)
		}
	}
	if m := regexp.MustCompile(`\b\d+ ?days?\b|\[\d+d\]|offset \d+d`).FindString(text); m != "" {
		t.Errorf("dashboard carries %q, want no fixed number of days anywhere", m)
	}
	for _, m := range urlHostRe.FindAllStringSubmatch(text, -1) {
		if m[1] != "releases.moe" {
			t.Errorf("dashboard links to host %q, want only releases.moe", m[1])
		}
	}
	if m := ipv4Re.FindString(text); m != "" {
		t.Errorf("dashboard carries the address %q, want no address of any deployment", m)
	}
}

// A library summary is one snapshot, so a panel reading it takes the newest
// sample: any other range aggregation can combine fields from two summaries,
// and keeps a count that fell at the last check at its older, larger value.
func TestDashboardReadsTheLatestLibrarySummary(t *testing.T) {
	summaryRe := regexp.MustCompile("(\\w+)\\(\\{[^{}]*\\}\\s*\\|=\\s*`library summary`")
	n := 0
	for _, tg := range dashboardTargets(loadDashboard(t)) {
		for _, m := range summaryRe.FindAllStringSubmatch(tg.expr, -1) {
			n++
			if m[1] != "last_over_time" {
				t.Errorf("panel %q reads the library summary with %s, want last_over_time\nexpr: %s", tg.panel, m[1], tg.expr)
			}
		}
	}
	if n == 0 {
		t.Fatalf("%s reads no library summary", dashboardPath)
	}
}

// Dividing by a zero items_with_entry yields NaN, which Grafana renders as a
// value; dividing by (x > 0) yields no series, so the panel shows its no-value text.
func TestDashboardGuardsTheBestShareDenominator(t *testing.T) {
	guarded := regexp.MustCompile(`(?s)^\(.*>\s*0\s*\)$`)
	ratios := map[string]bool{}
	for _, tg := range dashboardTargets(loadDashboard(t)) {
		for _, d := range divisors(tg.expr) {
			if !strings.Contains(d, "items_with_entry") {
				continue
			}
			ratios[tg.panel] = true
			if !guarded.MatchString(d) {
				t.Errorf("panel %q divides by %s, want the divisor compared with > 0\nexpr: %s", tg.panel, d, tg.expr)
			}
		}
	}
	for _, panel := range []string{"Anime at SeaDex best", "Anime at SeaDex best, over time"} {
		if !ratios[panel] {
			t.Errorf("panel %q does not divide by items_with_entry, want the best-share ratio", panel)
		}
	}
}

// divisors returns the right operand of every '/' in expr, read as one
// parenthesized group or one function call.
func divisors(expr string) []string {
	var out []string
	for i, c := range expr {
		if c != '/' {
			continue
		}
		if op := leadingOperand(strings.TrimLeft(expr[i+1:], " \t\n")); op != "" {
			out = append(out, op)
		}
	}
	return out
}

func leadingOperand(s string) string {
	open := strings.IndexFunc(s, func(r rune) bool {
		return r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	if open < 0 || s[open] != '(' {
		return ""
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}

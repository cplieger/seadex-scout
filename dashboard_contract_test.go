package main

import (
	"encoding/json"
	"maps"
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
	datasource any
	panel      string
	expr       string
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

func field(v any, keys ...string) any {
	for _, k := range keys {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

// dashboardTargets keeps a query with no expr as an empty one, so the
// contract checks report it instead of skipping it.
func dashboardTargets(d map[string]any) []dashTarget {
	elements, _ := field(d, "spec", "elements").(map[string]any)
	var out []dashTarget
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		el := elements[name]
		title, _ := field(el, "spec", "title").(string)
		queries, _ := field(el, "spec", "data", "spec", "queries").([]any)
		for _, q := range queries {
			query := field(q, "spec", "query")
			expr, _ := field(query, "spec", "expr").(string)
			out = append(out, dashTarget{panel: title, expr: expr, datasource: field(query, "datasource")})
		}
	}
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
// a schema v2 resource whose metadata.name is seadex-scout, a datasource
// variable first and used by every query, a container variable, the
// optional-upgrades switch defaulting to Hide, no fixed number of days, and no
// host but the SeaDex site.
func TestDashboardShape(t *testing.T) {
	raw, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("read %s: %v", dashboardPath, err)
	}
	d := loadDashboard(t)
	if d["apiVersion"] != "dashboard.grafana.app/v2" || d["kind"] != "Dashboard" {
		t.Errorf("apiVersion, kind = %v, %v, want dashboard.grafana.app/v2, Dashboard", d["apiVersion"], d["kind"])
	}
	if name := field(d, "metadata", "name"); name != "seadex-scout" {
		t.Errorf("metadata.name = %v, want seadex-scout", name)
	}
	vars, _ := field(d, "spec", "variables").([]any)
	var names []string
	for _, v := range vars {
		name, _ := field(v, "spec", "name").(string)
		names = append(names, name)
	}
	if !slices.Equal(names, []string{"datasource", "container", "optional"}) {
		t.Errorf("variables = %v, want [datasource container optional]", names)
	}
	if len(vars) == 3 {
		opt := vars[2]
		kind, query, cur := field(opt, "kind"), field(opt, "spec", "query"), field(opt, "spec", "current")
		if kind != "CustomVariable" || query != "Hide : alt, Show : none" ||
			field(cur, "text") != "Hide" || field(cur, "value") != "alt" {
			t.Errorf("optional variable kind = %v, query = %v, current = %v, want a CustomVariable with options Hide : alt, Show : none and Hide selected",
				kind, query, cur)
		}
	}
	targets := dashboardTargets(d)
	if len(targets) == 0 {
		t.Fatalf("%s carries no query", dashboardPath)
	}
	want := map[string]any{"name": "${datasource}"}
	for _, tg := range targets {
		if ds, _ := tg.datasource.(map[string]any); !maps.Equal(ds, want) {
			t.Errorf("panel %q query datasource = %v, want %v", tg.panel, tg.datasource, want)
		}
		if !strings.Contains(tg.expr, `container="$container"`) {
			t.Errorf("panel %q does not select on $container: %s", tg.panel, tg.expr)
		}
	}
	text := string(raw)
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
	for _, panel := range []string{"SeaDex best", "SeaDex alt"} {
		if !ratios[panel] {
			t.Errorf("panel %q does not divide by items_with_entry, want the best-share ratio", panel)
		}
	}
}

// The upgrades tile and table must hide the same optional upgrades, or the
// count and the list disagree, so every finding read carries the filter.
func TestDashboardFiltersOptionalUpgradesEverywhere(t *testing.T) {
	findingRe := regexp.MustCompile("\\{[^{}]*\\}\\s*\\|=\\s*`better release available`")
	const filter = `| current_tier != "$optional"`
	n := 0
	for _, tg := range dashboardTargets(loadDashboard(t)) {
		for _, loc := range findingRe.FindAllStringIndex(tg.expr, -1) {
			n++
			pipe := tg.expr[loc[0]:]
			if end := strings.Index(pipe, "["); end >= 0 {
				pipe = pipe[:end]
			}
			if !strings.Contains(pipe, filter) {
				t.Errorf("panel %q reads findings without %s\nexpr: %s", tg.panel, filter, tg.expr)
			}
		}
	}
	if n == 0 {
		t.Fatalf("%s reads no finding", dashboardPath)
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

func TestDashboardNamesTheUncomparedSpecialsPlainly(t *testing.T) {
	const label = "Episode not known"
	elements, _ := field(loadDashboard(t), "spec", "elements").(map[string]any)
	found := false
	for _, el := range elements {
		if title, _ := field(el, "spec", "title").(string); title != "Library overview" {
			continue
		}
		refID := ""
		queries, _ := field(el, "spec", "data", "spec", "queries").([]any)
		for _, q := range queries {
			if expr, _ := field(q, "spec", "query", "spec", "expr").(string); strings.Contains(expr, "unwrap unattributed ") {
				refID, _ = field(q, "spec", "refId").(string)
			}
		}
		overrides, _ := field(el, "spec", "vizConfig", "spec", "fieldConfig", "overrides").([]any)
		for _, o := range overrides {
			if opt, _ := field(o, "matcher", "options").(string); opt != refID || refID == "" {
				continue
			}
			props, _ := field(o, "properties").([]any)
			for _, p := range props {
				if id, _ := field(p, "id").(string); id == "displayName" {
					found = true
					if v, _ := field(p, "value").(string); v != label {
						t.Errorf("Library overview names the unattributed bar %q, want %q", v, label)
					}
				}
			}
		}
		if desc, _ := field(el, "spec", "description").(string); !strings.Contains(desc, label+" means") {
			t.Errorf("Library overview description = %q, want it to define %q", desc, label)
		}
	}
	if !found {
		t.Fatalf("%s has no Library overview bar unwrapping unattributed with a display name", dashboardPath)
	}
}

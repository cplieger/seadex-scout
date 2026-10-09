package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

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
	logQueryRe   = regexp.MustCompile(`^\{[^{}]*\}((?:[^\[{]|\{\{[^}]*\}\})*)$`)
	overTimeRe   = regexp.MustCompile(`\b[a-z_]+_over_time\(`)
	// LogQL takes a by () grouping only on these unwrapped range aggregations;
	// the others, sum_over_time among them, take it on an outer sum.
	groupableRe   = regexp.MustCompile(`^(avg|min|max|stddev|stdvar|quantile|first|last)_over_time\($`)
	outerSumRe    = regexp.MustCompile(`sum\s+by\s*\([^)]*\)\s*\($`)
	byRe          = regexp.MustCompile(`\bby\s*\(([^)]*)\)`)
	summaryReadRe = regexp.MustCompile("\\|=\\s*`(upgrade sizes|biggest upgrade)`")
	regexMetaRe   = regexp.MustCompile(`[\\.*+?()\[\]{}^$]`)
	urlHostRe     = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://([^/"\s:?#]+)`)
	ipv4Re        = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
)

// allowedWindows are the fixed windows that encode seadex-scout's own cadence;
// anything reaching back into history must use Grafana's range.
var allowedWindows = []string{"$__range", "$__interval", "1h", "2h", "3h", "26h", "72h"}

type dashTarget struct {
	datasource any
	panel      string
	expr       string
	maxLines   float64
}

type dashPanel struct {
	title      string
	targets    []dashTarget
	transforms []map[string]any
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
	var out []dashTarget
	for _, p := range dashboardPanels(d) {
		out = append(out, p.targets...)
	}
	return out
}

func dashboardPanels(d map[string]any) []dashPanel {
	elements, _ := field(d, "spec", "elements").(map[string]any)
	var out []dashPanel
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		el := elements[name]
		p := dashPanel{}
		p.title, _ = field(el, "spec", "title").(string)
		queries, _ := field(el, "spec", "data", "spec", "queries").([]any)
		for _, q := range queries {
			query := field(q, "spec", "query")
			expr, _ := field(query, "spec", "expr").(string)
			maxLines, _ := field(query, "spec", "maxLines").(float64)
			p.targets = append(p.targets, dashTarget{panel: p.title, expr: expr, datasource: field(query, "datasource"), maxLines: maxLines})
		}
		transforms, _ := field(el, "spec", "data", "spec", "transformations").([]any)
		for _, tr := range transforms {
			m := map[string]any{"group": field(tr, "group")}
			opts, _ := field(tr, "spec", "options").(map[string]any)
			maps.Copy(m, opts)
			p.transforms = append(p.transforms, m)
		}
		out = append(out, p)
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

// checkExpr applies every expression-level rule. A log query, one stream
// selector and its stages with no range, is read whole; a metric query is read
// one range pipeline at a time.
func checkExpr(c *logcontract.Contract, expr string) []string {
	if m := logQueryRe.FindStringSubmatch(expr); m != nil {
		return checkPipeline(c, m[1], "$__range")
	}
	pipes := pipelineRe.FindAllStringSubmatch(expr, -1)
	if len(pipes) == 0 {
		return []string{"no log pipeline found"}
	}
	var problems []string
	for _, p := range pipes {
		problems = append(problems, checkPipeline(c, p[1], p[2])...)
	}
	problems = append(problems, checkGrouping(expr)...)
	for _, m := range offsetRe.FindAllStringSubmatch(expr, -1) {
		if m[1] != "$__range" {
			problems = append(problems, "offset "+m[1]+" is a fixed look-back; use $__range")
		}
	}
	return problems
}

// checkGrouping applies the series-count rules of a metric query: every range
// aggregation over an unwrapped value names its grouping, so extracted labels
// cannot split it into one series per line, no grouping names a finding's
// identity, which would make one series per finding, and every per-view
// summary read selects the view.
func checkGrouping(expr string) []string {
	var problems []string
	for _, loc := range overTimeRe.FindAllStringIndex(expr, -1) {
		end := matchingParen(expr, loc[1]-1)
		if end < 0 {
			problems = append(problems, "an unbalanced range aggregation")
			continue
		}
		if !strings.Contains(expr[loc[1]:end], "| unwrap") {
			continue
		}
		grouped := regexp.MustCompile(`^\s*by\s*\(`).MatchString(expr[end+1:])
		if !groupableRe.MatchString(expr[loc[0]:loc[1]]) {
			grouped = outerSumRe.MatchString(expr[:loc[0]])
		}
		if !grouped {
			problems = append(problems, "the unwrapped aggregation "+expr[loc[0]:loc[1]]+"...) names no by () grouping")
		}
	}
	for _, m := range byRe.FindAllStringSubmatch(expr, -1) {
		for label := range strings.SplitSeq(m[1], ",") {
			if label = strings.TrimSpace(label); slices.Contains([]string{"al_id", "info_hash", "title"}, label) {
				problems = append(problems, "a metric grouping names the finding identity "+label)
			}
		}
	}
	for _, loc := range summaryReadRe.FindAllStringIndex(expr, -1) {
		pipe := expr[loc[0]:]
		if end := strings.IndexByte(pipe, '['); end >= 0 {
			pipe = pipe[:end]
		}
		if !strings.Contains(pipe, `| hidden_tier="$optional"`) {
			problems = append(problems, "a per-view summary read without | hidden_tier=\"$optional\"")
		}
	}
	return problems
}

func matchingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// snapshotMessages are the lines a pass re-states in full, so a table over
// them shows one pass only by joining on the newest pass's pass_id.
var snapshotMessages = []string{"better release available", "manual review", "biggest upgrade"}

// passMessages are the per-pass lines whose newest pass_id a snapshot joins on.
var passMessages = []string{"findings reported", "upgrade sizes"}

// checkPanel applies the item-table rules: every log query has a line limit,
// and a log query over a snapshot message sits beside a one-line read of a pass
// message, joined inner on pass_id, so the table shows the newest pass alone.
func checkPanel(p *dashPanel) []string {
	var problems []string
	snapshot, pass := false, false
	for _, tg := range p.targets {
		if !logQueryRe.MatchString(tg.expr) {
			continue
		}
		if tg.maxLines <= 0 || tg.maxLines > 401 {
			problems = append(problems, fmt.Sprintf("a log query with line limit %v, want 1 to 401", tg.maxLines))
		}
		for _, msg := range snapshotMessages {
			snapshot = snapshot || strings.Contains(tg.expr, "|= `"+msg+"`")
		}
		for _, msg := range passMessages {
			pass = pass || (strings.Contains(tg.expr, "|= `"+msg+"`") && tg.maxLines == 1)
		}
	}
	if !snapshot {
		return problems
	}
	joined := slices.ContainsFunc(p.transforms, func(tr map[string]any) bool {
		return tr["group"] == "joinByField" && tr["byField"] == "pass_id" && tr["mode"] == "inner"
	})
	if !pass || !joined {
		problems = append(problems, "a snapshot read not joined inner on pass_id with a one-line pass read")
	}
	return problems
}

func TestDashboardItemTablesAreSnapshotsOrEventReads(t *testing.T) {
	n := 0
	for _, p := range dashboardPanels(loadDashboard(t)) {
		for _, problem := range checkPanel(&p) {
			t.Errorf("panel %q: %s", p.title, problem)
		}
		for _, tg := range p.targets {
			if logQueryRe.MatchString(tg.expr) {
				n++
			}
		}
	}
	if n == 0 {
		t.Fatalf("%s carries no log query, so the item-table rules check nothing", dashboardPath)
	}
}

func TestDashboardItemTableCheckRejects(t *testing.T) {
	const findings = `{container="$container"} |= ` + "`better release available` | json msg=\"msg\" | msg=`better release available`"
	const pass = `{container="$container"} |= ` + "`findings reported` | json msg=\"msg\", pass_id=\"pass_id\" | msg=`findings reported`"
	join := map[string]any{"group": "joinByField", "byField": "pass_id", "mode": "inner"}
	cases := map[string]dashPanel{
		"no line limit":  {targets: []dashTarget{{expr: pass, maxLines: 1}, {expr: findings}}, transforms: []map[string]any{join}},
		"no join":        {targets: []dashTarget{{expr: pass, maxLines: 1}, {expr: findings, maxLines: 401}}},
		"outer join":     {targets: []dashTarget{{expr: pass, maxLines: 1}, {expr: findings, maxLines: 401}}, transforms: []map[string]any{{"group": "joinByField", "byField": "pass_id", "mode": "outer"}}},
		"no pass read":   {targets: []dashTarget{{expr: findings, maxLines: 401}}, transforms: []map[string]any{join}},
		"wide pass read": {targets: []dashTarget{{expr: pass, maxLines: 5}, {expr: findings, maxLines: 401}}, transforms: []map[string]any{join}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if problems := checkPanel(&p); len(problems) == 0 {
				t.Errorf("checkPanel(%s) reported nothing, want a violation", name)
			}
		})
	}
	ok := dashPanel{targets: []dashTarget{{expr: pass, maxLines: 1}, {expr: findings, maxLines: 401}}, transforms: []map[string]any{join}}
	if problems := checkPanel(&ok); len(problems) != 0 {
		t.Errorf("checkPanel(joined snapshot) = %v, want no problem", problems)
	}
}

// TestDashboardSizeChangeSkipsUnknownSizes pins that every size the dashboard
// shows is one the app computed over known sizes only: no query adds or
// subtracts byte fields itself, where an unknown size would read as 0, and the
// net change tile and column read size_change_bytes.
func TestDashboardSizeChangeSkipsUnknownSizes(t *testing.T) {
	byteField := regexp.MustCompile(`unwrap (recommended_bytes|current_bytes|recommended_bytes_total|current_bytes_replaced|download_bytes_total)\b[^|]*\)[^)]*\)?\s*[-+]`)
	reads := 0
	for _, tg := range dashboardTargets(loadDashboard(t)) {
		if byteField.MatchString(tg.expr) {
			t.Errorf("panel %q computes a size difference in the query\nexpr: %s", tg.panel, tg.expr)
		}
		if strings.Contains(tg.expr, `size_change_bytes="size_change_bytes"`) {
			reads++
		}
	}
	if reads < 2 {
		t.Errorf("%d queries read size_change_bytes, want the net change tile and the upgrades table", reads)
	}
}

// The download total leaves out every upgrade whose download size is unknown,
// so it is a lower bound: its tile must say "at least", and the count it leaves
// out must be on the dashboard.
func TestDashboardDownloadTotalReadsAsALowerBound(t *testing.T) {
	elements, _ := field(loadDashboard(t), "spec", "elements").(map[string]any)
	totals, unsized := 0, 0
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		el := elements[name]
		queries, _ := field(el, "spec", "data", "spec", "queries").([]any)
		for _, q := range queries {
			expr, _ := field(q, "spec", "query", "spec", "expr").(string)
			if strings.Contains(expr, "unwrap upgrades_download_unsized ") {
				unsized++
			}
			if !strings.Contains(expr, "unwrap download_bytes_total ") {
				continue
			}
			totals++
			title, _ := field(el, "spec", "title").(string)
			shown, _ := field(el, "spec", "vizConfig", "spec", "fieldConfig", "defaults", "displayName").(string)
			if !strings.Contains(title+" "+shown, "at least") {
				t.Errorf("panel %q shows the download total as %q, want it to say at least", title, shown)
			}
		}
	}
	if totals == 0 || unsized == 0 {
		t.Fatalf("%s reads the download total %d times and the unsized count %d times, want both", dashboardPath, totals, unsized)
	}
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
		"ungrouped unwrap":    "max(last_over_time(" + sel + " |= `library summary` | json msg=\"msg\", rows=\"rows\" | msg=`library summary` | unwrap rows [26h]))",
		"identity grouping":   "sum by (title) (count_over_time(" + sel + " |= `better release available` | json msg=\"msg\", title=\"title\" | msg=`better release available` [2h]))",
		"info_hash grouping":  "count(sum by (info_hash, arr) (count_over_time(" + sel + " |= `better release available` | json msg=\"msg\", info_hash=\"info_hash\" | msg=`better release available` [2h])))",
		"ungrouped sum":       "sum(sum_over_time(" + sel + " |= `indexer feed snapshot written` | json msg=\"msg\", journal_new=\"journal_new\" | msg=`indexer feed snapshot written` | unwrap journal_new [$__range]))",
		"summary of no view":  "last_over_time(" + sel + " |= `upgrade sizes` | json msg=\"msg\", upgrades=\"upgrades\" | msg=`upgrade sizes` | unwrap upgrades [2h]) by ()",
		"log query bare json": sel + " |= `biggest upgrade` | json | msg=`biggest upgrade` | hidden_tier=\"$optional\"",
	}
	for name, expr := range cases {
		t.Run(name, func(t *testing.T) {
			if problems := checkExpr(&c, expr); len(problems) == 0 {
				t.Errorf("checkExpr(%q) reported nothing, want a contract violation", expr)
			}
		})
	}
	for _, ok := range []string{
		"last_over_time(" + sel + " |= `library summary` | json msg=\"msg\", have_best=\"have_best\" | msg=`library summary` | unwrap have_best [26h]) by ()",
		sel + " |= `biggest upgrade` | json msg=\"msg\", hidden_tier=\"hidden_tier\" | msg=`biggest upgrade` | hidden_tier=\"$optional\"",
	} {
		if problems := checkExpr(&c, ok); len(problems) != 0 {
			t.Errorf("checkExpr(%q) = %v, want no problem", ok, problems)
		}
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

// A library summary and an upgrade sizes line are each one snapshot, so a
// panel reading one takes the newest sample: any other range aggregation can
// combine fields from two snapshots, and keeps a count that fell at the last
// check at its older, larger value.
func TestDashboardReadsTheLatestSummaries(t *testing.T) {
	summaryRe := regexp.MustCompile("(\\w+)\\(\\{[^{}]*\\}\\s*\\|=\\s*`(library summary|upgrade sizes)`")
	reads := map[string]int{}
	for _, tg := range dashboardTargets(loadDashboard(t)) {
		for _, m := range summaryRe.FindAllStringSubmatch(tg.expr, -1) {
			reads[m[2]]++
			if m[1] != "last_over_time" {
				t.Errorf("panel %q reads the %s line with %s, want last_over_time\nexpr: %s", tg.panel, m[2], m[1], tg.expr)
			}
		}
	}
	for _, msg := range []string{"library summary", "upgrade sizes"} {
		if reads[msg] == 0 {
			t.Errorf("%s reads no %s line through a range aggregation", dashboardPath, msg)
		}
	}
}

// A breakdown that divides finding lines by findings reported lines over a
// window is an average per check, fractional whenever the set changed in the
// window, so its panel must say so and show a decimal rather than round it
// into a count that disagrees with the Upgrades tile.
func TestDashboardShowsPerCheckAveragesAsAverages(t *testing.T) {
	perCheckRe := regexp.MustCompile("/\\s*on\\s*\\(\\)\\s*group_left\\s+sum\\s+by\\s*\\(\\)\\s*\\(count_over_time\\(\\{[^{}]*\\}\\s*\\|=\\s*`findings reported`")
	elements, _ := field(loadDashboard(t), "spec", "elements").(map[string]any)
	n := 0
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		el := elements[name]
		title, _ := field(el, "spec", "title").(string)
		queries, _ := field(el, "spec", "data", "spec", "queries").([]any)
		if !slices.ContainsFunc(queries, func(q any) bool {
			expr, _ := field(q, "spec", "query", "spec", "expr").(string)
			return perCheckRe.MatchString(expr)
		}) {
			continue
		}
		n++
		if desc, _ := field(el, "spec", "description").(string); !strings.Contains(strings.ToLower(desc), "average") {
			t.Errorf("panel %q shows a per-check average, but its description never says average: %q", title, desc)
		}
		decimals := panelDecimals(field(el, "spec", "vizConfig", "spec", "fieldConfig"))
		if len(decimals) == 0 || slices.ContainsFunc(decimals, func(d float64) bool { return d < 1 }) {
			t.Errorf("panel %q shows a per-check average with decimals %v, want at least one decimal everywhere it sets them", title, decimals)
		}
	}
	if n == 0 {
		t.Fatalf("%s carries no per-check average, so this check checks nothing", dashboardPath)
	}
}

// tableColumns maps each table override's matched field to the header it
// shows: its displayName, else the field name.
func tableColumns(el any) map[string]map[string]any {
	columns := map[string]map[string]any{}
	overrides, _ := field(el, "spec", "vizConfig", "spec", "fieldConfig", "overrides").([]any)
	for _, o := range overrides {
		name, _ := field(o, "matcher", "options").(string)
		props := map[string]any{"displayName": name}
		list, _ := field(o, "properties").([]any)
		for _, p := range list {
			id, _ := field(p, "id").(string)
			props[id] = field(p, "value")
		}
		columns[name] = props
	}
	return columns
}

func tables(t *testing.T) map[string]any {
	t.Helper()
	elements, _ := field(loadDashboard(t), "spec", "elements").(map[string]any)
	out := map[string]any{}
	for name, el := range elements {
		if field(el, "spec", "vizConfig", "group") == "table" {
			out[name] = el
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no table", dashboardPath)
	}
	return out
}

// Grafana sorts by a column's shown header, so a sortBy naming a header no
// column shows leaves the table unsorted.
func TestDashboardTablesSortByAShownColumn(t *testing.T) {
	all := tables(t)
	for _, name := range slices.Sorted(maps.Keys(all)) {
		el := all[name]
		shown := map[string]bool{}
		for _, props := range tableColumns(el) {
			header, _ := props["displayName"].(string)
			shown[header] = true
		}
		sortBy, _ := field(el, "spec", "vizConfig", "spec", "options", "sortBy").([]any)
		for _, s := range sortBy {
			if header, _ := field(s, "displayName").(string); !shown[header] {
				t.Errorf("panel %q sorts by %q, which no column shows", field(el, "spec", "title"), header)
			}
		}
	}
}

// A fixed column narrower than its header plus the filter and sort icons cuts
// the header: at least 8 px per character plus 56 px.
func TestDashboardTableColumnsFitTheirHeaders(t *testing.T) {
	all := tables(t)
	for _, name := range slices.Sorted(maps.Keys(all)) {
		el := all[name]
		for column, props := range tableColumns(el) {
			header, _ := props["displayName"].(string)
			floor := float64(utf8.RuneCountInString(header)*8 + 56)
			for _, key := range []string{"custom.width", "custom.minWidth"} {
				if width, ok := props[key].(float64); ok && width < floor {
					t.Errorf("panel %q column %q (%q) has %s %v, want at least %v", field(el, "spec", "title"), column, header, key, width, floor)
				}
			}
		}
	}
}

func panelDecimals(fieldConfig any) []float64 {
	var out []float64
	if d, ok := field(fieldConfig, "defaults", "decimals").(float64); ok {
		out = append(out, d)
	}
	overrides, _ := field(fieldConfig, "overrides").([]any)
	for _, o := range overrides {
		props, _ := field(o, "properties").([]any)
		for _, p := range props {
			if field(p, "id") == "decimals" {
				if d, ok := field(p, "value").(float64); ok {
					out = append(out, d)
				}
			}
		}
	}
	return out
}

// The upgrades tile and table must hide the same optional upgrades, or the
// count and the list disagree, so every finding, event and ranking read
// carries the filter.
func TestDashboardFiltersOptionalUpgradesEverywhere(t *testing.T) {
	findingRe := regexp.MustCompile("\\{[^{}]*\\}\\s*\\|=\\s*`(better release available|upgrade resolved|upgrade found|biggest upgrade)`")
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

var errorsOnlyRe = regexp.MustCompile("\\|\\s*level\\s*=\\s*`ERROR`")

// A table that reads only ERROR records cannot show a stopped check, which
// logs nothing, so its title and its empty text must say errors rather than
// claim every problem behind Status.
func TestDashboardErrorTablesSayTheyListErrors(t *testing.T) {
	elements, _ := field(loadDashboard(t), "spec", "elements").(map[string]any)
	n := 0
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		el := elements[name]
		if field(el, "spec", "vizConfig", "group") != "table" {
			continue
		}
		queries, _ := field(el, "spec", "data", "spec", "queries").([]any)
		if len(queries) == 0 || slices.ContainsFunc(queries, func(q any) bool {
			expr, _ := field(q, "spec", "query", "spec", "expr").(string)
			return !errorsOnlyRe.MatchString(expr)
		}) {
			continue
		}
		n++
		title, _ := field(el, "spec", "title").(string)
		noValue, _ := field(el, "spec", "vizConfig", "spec", "fieldConfig", "defaults", "noValue").(string)
		if !strings.Contains(strings.ToLower(title), "error") {
			t.Errorf("panel %q reads only ERROR records, want a title that says errors", title)
		}
		if noValue != "No errors logged" {
			t.Errorf("panel %q no-value text = %q, want %q", title, noValue, "No errors logged")
		}
	}
	if n == 0 {
		t.Fatalf("%s carries no table of ERROR records, so this check checks nothing", dashboardPath)
	}
}

// The Status tile sits on another tab than the panels that explain it, so its
// description must name that tab as a tab.
func TestDashboardStatusNamesTheHealthTab(t *testing.T) {
	d := loadDashboard(t)
	desc, _ := field(d, "spec", "elements", "panel-10", "spec", "description").(string)
	health := tabHolding(t, d, "panel-52")
	if home := tabHolding(t, d, "panel-10"); home == health {
		t.Fatalf("Status and Last check are both on tab %q, want them on different tabs", home)
	}
	if !strings.Contains(desc, "the "+health+" tab") {
		t.Errorf("Status description = %q, want it to name the %s tab", desc, health)
	}
}

// A bar chart counting events per $__interval draws one bar per event at a
// week's range, so every bar chart buckets by day and its title says so.
func TestDashboardBarChartsCountPerDay(t *testing.T) {
	elements, _ := field(loadDashboard(t), "spec", "elements").(map[string]any)
	n := 0
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		el := elements[name]
		if field(el, "spec", "vizConfig", "group") != "timeseries" ||
			field(el, "spec", "vizConfig", "spec", "fieldConfig", "defaults", "custom", "drawStyle") != "bars" {
			continue
		}
		n++
		title, _ := field(el, "spec", "title").(string)
		if interval := field(el, "spec", "data", "spec", "queryOptions", "interval"); interval != "1d" {
			t.Errorf("bar chart %q has minimum query interval %v, want 1d", title, interval)
		}
		if !strings.HasSuffix(title, " per day") {
			t.Errorf("bar chart %q counts per day, want a title ending in \"per day\"", title)
		}
	}
	if n == 0 {
		t.Fatalf("%s carries no bar chart, so this check checks nothing", dashboardPath)
	}
}

func tabHolding(t *testing.T, d map[string]any, element string) string {
	t.Helper()
	tabs, _ := field(d, "spec", "layout", "spec", "tabs").([]any)
	for _, tab := range tabs {
		if referencesElement(field(tab, "spec", "layout"), element) {
			title, _ := field(tab, "spec", "title").(string)
			return title
		}
	}
	t.Fatalf("no tab of %s references %s", dashboardPath, element)
	return ""
}

func referencesElement(v any, element string) bool {
	switch x := v.(type) {
	case map[string]any:
		if x["kind"] == "ElementReference" && x["name"] == element {
			return true
		}
		for _, child := range x {
			if referencesElement(child, element) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if referencesElement(child, element) {
				return true
			}
		}
	}
	return false
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

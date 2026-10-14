package main

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"go.yaml.in/yaml/v3"
)

// alertTiles maps each Overview tile that shows only while seadex-scout needs a
// look to the shipped rule it mirrors, or to "" when no shipped rule watches it.
var alertTiles = map[string]string{
	"panel-60": "",
	"panel-61": "SeadexScoutCycleError",
	"panel-62": "SeadexScoutScanStalled",
	"panel-63": "SeadexScoutReconcileStalled",
	"panel-64": "SeadexScoutUpstreamUnavailable",
	"panel-65": "SeadexScoutLibraryDegraded",
}

// rangeCountTiles count events over the dashboard range in place of their
// rule's window; every other tile shows a current state and keeps the rule's
// windows, so a narrower range cannot hide it while its alert fires.
var rangeCountTiles = map[string]bool{"panel-61": true}

var (
	// badCaseRe matches a query that returns no series while the condition is
	// clear, which is what the has-data rule hides on.
	badCaseRe      = regexp.MustCompile(`(?s)(>\s*0\s*$|^absent_over_time\()`)
	conditionSetRe = regexp.MustCompile("condition\\s*=(~?)\\s*`([^`]*)`")
	windowRe       = regexp.MustCompile(`\[([^\]]+)\]`)
)

type shippedRule struct {
	Alert string `yaml:"alert"`
	Expr  string `yaml:"expr"`
}

func loadRules(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(alertRulesFile)
	if err != nil {
		t.Fatalf("read %s: %v", alertRulesFile, err)
	}
	var doc struct {
		Groups []struct {
			Rules []shippedRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", alertRulesFile, err)
	}
	rules := map[string]string{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			rules[r.Alert] = r.Expr
		}
	}
	return rules
}

// autoGridItems returns the spec of every auto-grid item under layout, keyed by
// the element it places.
func autoGridItems(layout any, out map[string]map[string]any) {
	switch x := layout.(type) {
	case map[string]any:
		if x["kind"] == "AutoGridLayout" {
			items, _ := field(x, "spec", "items").([]any)
			for _, it := range items {
				spec, _ := field(it, "spec").(map[string]any)
				name, _ := field(spec, "element", "name").(string)
				out[name] = spec
			}
		}
		for _, child := range x {
			autoGridItems(child, out)
		}
	case []any:
		for _, child := range x {
			autoGridItems(child, out)
		}
	}
}

func gatedByData(spec map[string]any) bool {
	items, _ := field(spec, "conditionalRendering", "spec", "items").([]any)
	return slices.ContainsFunc(items, func(it any) bool { return field(it, "kind") == "ConditionalRenderingData" })
}

// startsHiddenUntilData returns why a has-data item could show before its
// query answers, or "". Grafana 13.2.3 shows an item while its has-data rule is
// undecided, on load and on every refresh slower than 200 ms
// (grafana/grafana#134713), so an "any" group adds a range-size rule that is
// always false. A display: none panel measures no width, so its query runs
// only with a pinned maxDataPoints.
func startsHiddenUntilData(d, spec map[string]any) string {
	group := field(spec, "conditionalRendering")
	items, _ := field(group, "spec", "items").([]any)
	if field(group, "kind") != "ConditionalRenderingGroup" || field(group, "spec", "visibility") != "show" ||
		field(group, "spec", "condition") != "or" || len(items) != 2 {
		return "want a show group matching any of two rules"
	}
	hasData, neverTrue := false, false
	for _, it := range items {
		switch field(it, "kind") {
		case "ConditionalRenderingData":
			hasData = field(it, "spec", "value") == true
		case "ConditionalRenderingTimeRangeSize":
			neverTrue = field(it, "spec", "value") == "0s"
		}
	}
	if !hasData || !neverTrue {
		return `want a has-data rule and a time range size rule of "0s"`
	}
	element, _ := field(spec, "element", "name").(string)
	points, _ := field(d, "spec", "elements", element, "spec", "data", "spec", "queryOptions", "maxDataPoints").(float64)
	if points < 1 || points != float64(int(points)) {
		return "want a positive whole queryOptions.maxDataPoints on the panel"
	}
	return ""
}

func panelExprs(d map[string]any, element string) []string {
	queries, _ := field(d, "spec", "elements", element, "spec", "data", "spec", "queries").([]any)
	var exprs []string
	for _, q := range queries {
		expr, _ := field(q, "spec", "query", "spec", "expr").(string)
		exprs = append(exprs, expr)
	}
	return exprs
}

// A has-data rule works only on an auto-grid item, and it hides the tile only
// when its query returns nothing while the condition is clear.
func TestDashboardAlertTilesHideUntilTheyFire(t *testing.T) {
	d := loadDashboard(t)
	tabs, _ := field(d, "spec", "layout", "spec", "tabs").([]any)
	overview := map[string]map[string]any{}
	all := map[string]map[string]any{}
	for i, tab := range tabs {
		if i == 0 {
			autoGridItems(field(tab, "spec", "layout"), overview)
		}
		autoGridItems(field(tab, "spec", "layout"), all)
	}
	for _, name := range slices.Sorted(maps.Keys(alertTiles)) {
		item, ok := overview[name]
		if !ok {
			t.Errorf("%s is not an auto-grid item on the Overview tab, so no has-data rule can hide it", name)
			continue
		}
		if why := startsHiddenUntilData(d, item); why != "" {
			t.Errorf("%s conditionalRendering = %v: %s", name, item["conditionalRendering"], why)
		}
		exprs := panelExprs(d, name)
		if len(exprs) == 0 {
			t.Errorf("%s carries no query", name)
		}
		for _, expr := range exprs {
			if !badCaseRe.MatchString(strings.TrimSpace(expr)) {
				t.Errorf("%s query returns a series while the condition is clear, want > 0 or absent_over_time\nexpr: %s", name, expr)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(all)) {
		if _, pinned := alertTiles[name]; gatedByData(all[name]) && !pinned {
			t.Errorf("%s hides on a has-data rule but is not a pinned alert tile", name)
		}
	}
}

// Without preload a lit tile below the fold sits in a lazy loader: it skips a
// refresh until it is scrolled to, then queries, blinks and reflows the panels
// under it.
func TestDashboardPreloadsSoGatedTilesBelowTheFoldRefresh(t *testing.T) {
	if preload := field(loadDashboard(t), "spec", "preload"); preload != true {
		t.Errorf("spec.preload = %v, want true", preload)
	}
}

// A tile that shows a label draws one column per series, and two or more do
// not fit its height at the stat text size, so the query keeps the top series.
func TestDashboardAlertTilesShowOneValue(t *testing.T) {
	d := loadDashboard(t)
	for _, name := range slices.Sorted(maps.Keys(alertTiles)) {
		perSeries, _ := field(d, "spec", "elements", name, "spec", "vizConfig", "spec", "options", "reduceOptions", "values").(bool)
		if !perSeries {
			continue
		}
		for _, expr := range panelExprs(d, name) {
			if !strings.HasPrefix(strings.TrimSpace(expr), "topk(1,") {
				t.Errorf("%s shows one value per series, want its query to keep one with topk(1, ...)\nexpr: %s", name, expr)
			}
		}
	}
}

func windows(expr string) []string {
	var out []string
	for _, m := range windowRe.FindAllStringSubmatch(expr, -1) {
		out = append(out, m[1])
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func conditionSets(expr string) []string {
	var sets []string
	for _, m := range conditionSetRe.FindAllStringSubmatch(expr, -1) {
		alts := strings.Split(m[2], "|")
		slices.Sort(alts)
		sets = append(sets, m[1]+strings.Join(alts, "|"))
	}
	slices.Sort(sets)
	return sets
}

// A tile and the alert it mirrors must not disagree: the tile names the rule,
// filters the same conditions and levels, and looks back over the rule's own
// windows, or over the dashboard range alone where it counts events.
func TestDashboardAlertTilesReadTheirRuleSignal(t *testing.T) {
	d := loadDashboard(t)
	rules := loadRules(t)
	for _, name := range slices.Sorted(maps.Keys(alertTiles)) {
		alert := alertTiles[name]
		desc, _ := field(d, "spec", "elements", name, "spec", "description").(string)
		if alert == "" {
			if !strings.Contains(desc, "No shipped alert rule") {
				t.Errorf("%s mirrors no rule, want its description to say so: %q", name, desc)
			}
			continue
		}
		ruleExpr, ok := rules[alert]
		if !ok {
			t.Errorf("%s mirrors %s, which %s does not carry", name, alert, alertRulesFile)
			continue
		}
		if !strings.Contains(desc, alert) {
			t.Errorf("%s description = %q, want it to name %s", name, desc, alert)
		}
		expr := strings.Join(panelExprs(d, name), "\n")
		if got, want := conditionSets(expr), conditionSets(ruleExpr); !slices.Equal(got, want) {
			t.Errorf("%s filters conditions %v, %s filters %v", name, got, alert, want)
		}
		if got, want := strings.Contains(expr, "level=`ERROR`"), strings.Contains(ruleExpr, "level=`ERROR`"); got != want {
			t.Errorf("%s filters on the error level = %v, %s = %v", name, got, alert, want)
		}
		want := windows(ruleExpr)
		if rangeCountTiles[name] {
			want = []string{"$__range"}
		}
		if got := windows(expr); !slices.Equal(got, want) {
			t.Errorf("%s looks back over %v, want %v from %s", name, got, want, alert)
		}
	}
}

const rangeTitle = " in ${span} ${spanunit}"

// rangeVariable returns the spec of a hidden range variable of the given kind.
func rangeVariable(t *testing.T, d map[string]any, name, kind string) map[string]any {
	t.Helper()
	vars, _ := field(d, "spec", "variables").([]any)
	for _, v := range vars {
		if field(v, "spec", "name") != name {
			continue
		}
		spec, _ := field(v, "spec").(map[string]any)
		if field(v, "kind") != kind || spec["hide"] != "hideVariable" {
			t.Errorf("%s = %v hide %v, want a hidden %s", name, field(v, "kind"), spec["hide"], kind)
		}
		return spec
	}
	t.Fatalf("no %s variable", name)
	return nil
}

// rangeSource returns the stream and value regex of spanread, the one Loki
// query both range words come from, so a title never mixes two reads.
func rangeSource(t *testing.T, d map[string]any) (stream string, rx *regexp.Regexp) {
	t.Helper()
	spec := rangeVariable(t, d, "spanread", "QueryVariable")
	if spec["refresh"] != "onTimeRangeChanged" {
		t.Errorf("spanread refresh = %v, want onTimeRangeChanged", spec["refresh"])
	}
	q := field(spec, "query", "spec")
	if field(spec, "query", "group") != "loki" || field(q, "type") != 2.0 || field(spec, "query", "datasource", "name") != "${datasource}" {
		t.Errorf("spanread query = %v, want a detected field values query on ${datasource}", field(spec, "query"))
	}
	stream, _ = field(q, "stream").(string)
	label, _ := field(q, "label").(string)
	if want := `{container="$container"} | label_format ` + label + "=`"; !strings.HasPrefix(stream, want) || !strings.HasSuffix(stream, "`") {
		t.Errorf("spanread stream = %q, want it to start %q and end in a backtick", stream, want)
	}
	if field(spec, "current", "value") != "" || field(spec, "current", "text") != "" {
		t.Errorf("spanread saves %v, want it empty so a failed first read leaves the fallback", field(spec, "current"))
	}
	js, _ := spec["regex"].(string)
	rx, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(js, "/"), "/"))
	if err != nil || rx.SubexpIndex("value") < 0 || rx.SubexpIndex("text") < 0 {
		t.Fatalf("spanread regex %q: %v, want value and text groups", js, err)
	}
	return stream, rx
}

// customText is the text Grafana gives a custom variable whose query is query,
// which titles render (VizPanelRenderer interpolates titles with the text
// format). The parse follows CustomVariable.transformCsvStringToOptions,
// https://github.com/grafana/scenes/blob/main/packages/scenes/src/variables/variants/CustomVariable.tsx.
// More than one option would let Grafana keep a stale choice, so it fails.
func customText(t *testing.T, query string) string {
	t.Helper()
	entries := regexp.MustCompile(`(?:\\,|[^,])+`).FindAllString(query, -1)
	if len(entries) != 1 {
		t.Errorf("custom query %q gives %d options, want 1", query, len(entries))
		return ""
	}
	if m := regexp.MustCompile(`^\s*(.+)\s:\s(.+)$`).FindStringSubmatch(entries[0]); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(entries[0])
}

// rangeWords is what "${span} ${spanunit}" renders once spanread holds the
// option Grafana makes of line, or of no line at all ("").
func rangeWords(t *testing.T, d map[string]any, rx *regexp.Regexp, line string) string {
	t.Helper()
	var value, text string
	if line != "" {
		m := rx.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("spanread regex %v does not match %q", rx, line)
			return ""
		}
		value, text = m[rx.SubexpIndex("value")], m[rx.SubexpIndex("text")]
	}
	span, _ := rangeVariable(t, d, "span", "CustomVariable")["query"].(string)
	unit, _ := rangeVariable(t, d, "spanunit", "CustomVariable")["query"].(string)
	return customText(t, strings.ReplaceAll(span, "${spanread}", value)) + " " +
		customText(t, strings.ReplaceAll(unit, "${spanread:text}", text))
}

// renderRange runs a variable's label_format template as Loki would for a
// range of seconds ending now, with Loki's integer template functions.
func renderRange(t *testing.T, stream string, seconds int64) string {
	t.Helper()
	body := stream[strings.Index(stream, "`")+1 : len(stream)-1]
	const to = int64(1_800_000_000_000)
	body = strings.NewReplacer("${__to}", strconv.FormatInt(to, 10), "${__from}", strconv.FormatInt(to-seconds*1000, 10)).Replace(body)
	tpl, err := template.New("").Funcs(template.FuncMap{
		"add": func(a, b int64) int64 { return a + b },
		"sub": func(a, b int64) int64 { return a - b },
		"mul": func(a, b int64) int64 { return a * b },
		"div": func(a, b int64) int64 { return a / b },
		"mod": func(a, b int64) int64 { return a % b },
	}).Parse(body)
	if err != nil {
		t.Fatalf("parse %q: %v", body, err)
	}
	var out strings.Builder
	if err := tpl.Execute(&out, nil); err != nil {
		t.Fatalf("execute %q: %v", body, err)
	}
	return out.String()
}

// rangeTotals names the panels that total over the whole time range: an
// instant query over [$__range].
func rangeTotals(d map[string]any) []string {
	elements, _ := field(d, "spec", "elements").(map[string]any)
	var out []string
	for _, name := range slices.Sorted(maps.Keys(elements)) {
		queries, _ := field(elements[name], "spec", "data", "spec", "queries").([]any)
		for _, q := range queries {
			spec := field(q, "spec", "query", "spec")
			if expr, _ := field(spec, "expr").(string); field(spec, "queryType") == "instant" && strings.Contains(expr, "[$__range]") {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// A panel that totals over the range names its length the way the other
// dashboards do, "in 30 days", and reads "in the range" before the length is
// read, when the range holds no line and when the read fails. Grafana passes
// Loki's variable queries no $__range_s, so the length comes from ${__from}
// and ${__to}.
func TestDashboardRangeTilesNameTheRangeLength(t *testing.T) {
	d := loadDashboard(t)
	totals := rangeTotals(d)
	for _, name := range slices.Sorted(maps.Keys(rangeCountTiles)) {
		if !slices.Contains(totals, name) {
			t.Errorf("%s counts over the time range but no instant query of it reads [$__range]", name)
		}
	}
	for _, name := range totals {
		if title, _ := field(d, "spec", "elements", name, "spec", "title").(string); !strings.HasSuffix(title, rangeTitle) {
			t.Errorf("%s totals over the time range but is titled %q, want a title ending in %q", name, title, rangeTitle)
		}
	}
	stream, rx := rangeSource(t, d)
	saved := []any{
		field(rangeVariable(t, d, "span", "CustomVariable"), "current", "text"),
		field(rangeVariable(t, d, "spanunit", "CustomVariable"), "current", "text"),
	}
	if !slices.Equal(saved, []any{"the", "range"}) {
		t.Errorf("span and spanunit save %v, want [the range]", saved)
	}
	if got := rangeWords(t, d, rx, ""); got != "the range" {
		t.Errorf("with no line in the range or a failed read, titles read %q, want \"the range\"", got)
	}
	for seconds, want := range map[int64]string{
		45:      "45 seconds",
		119:     "119 seconds",
		120:     "2 minutes",
		150:     "3 minutes",
		5400:    "90 minutes",
		7200:    "2 hours",
		19980:   "5.6 hours",
		86400:   "24 hours",
		129600:  "36 hours",
		172800:  "2 days",
		216000:  "2.5 days",
		277200:  "3.2 days",
		282240:  "3.3 days",
		604800:  "7 days",
		2592000: "30 days",
	} {
		if got := rangeWords(t, d, rx, renderRange(t, stream, seconds)); got != want {
			t.Errorf("a %d s range renders %q, want %q", seconds, got, want)
		}
	}
}

// The errors tile sits on another tab than the table that names each error, so
// its description must name that tab as a tab.
func TestDashboardErrorTileNamesTheHealthTab(t *testing.T) {
	d := loadDashboard(t)
	desc, _ := field(d, "spec", "elements", "panel-61", "spec", "description").(string)
	health := tabHolding(t, d, "panel-50")
	if home := tabHolding(t, d, "panel-61"); home == health {
		t.Fatalf("the errors tile and the error table are both on tab %q, want them on different tabs", home)
	}
	if !strings.Contains(desc, "the "+health+" tab") {
		t.Errorf("errors tile description = %q, want it to name the %s tab", desc, health)
	}
}

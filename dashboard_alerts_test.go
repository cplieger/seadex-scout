package main

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

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
	"panel-66": "SeadexScoutFeedNotPolled",
}

// rangeCountTiles count events over the dashboard range in place of their
// rule's window; every other tile shows a current state and keeps the rule's
// windows, so a narrower range cannot hide it while its alert fires.
var rangeCountTiles = map[string]bool{"panel-61": true}

var (
	// badCaseRe matches a query that returns no series while the condition is
	// clear, which is what the has-data rule hides on.
	badCaseRe      = regexp.MustCompile(`(?s)(>\s*0\s*$|^absent_over_time\(|\bunless\b)`)
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

func hiddenUnlessData(spec map[string]any) bool {
	group := field(spec, "conditionalRendering")
	if field(group, "kind") != "ConditionalRenderingGroup" || field(group, "spec", "visibility") != "show" {
		return false
	}
	items, _ := field(group, "spec", "items").([]any)
	return slices.ContainsFunc(items, func(it any) bool {
		return field(it, "kind") == "ConditionalRenderingData" && field(it, "spec", "value") == true
	})
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
		if !hiddenUnlessData(item) {
			t.Errorf("%s conditionalRendering = %v, want a show group holding a has-data rule", name, item["conditionalRendering"])
		}
		exprs := panelExprs(d, name)
		if len(exprs) == 0 {
			t.Errorf("%s carries no query", name)
		}
		for _, expr := range exprs {
			if !badCaseRe.MatchString(strings.TrimSpace(expr)) {
				t.Errorf("%s query returns a series while the condition is clear, want > 0, absent_over_time or unless\nexpr: %s", name, expr)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(all)) {
		if _, pinned := alertTiles[name]; hiddenUnlessData(all[name]) && !pinned {
			t.Errorf("%s hides on a has-data rule but is not a pinned alert tile", name)
		}
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

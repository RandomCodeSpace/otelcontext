//go:build browser && !windows

package browser_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

type applicationMapPage struct {
	ctx     context.Context
	app     *appProcess
	smoke   *smokeRun
	fixture applicationMapFixture
	graph   applicationMapGraph
}

func openApplicationMap(t *testing.T, shape, artifacts string) applicationMapPage {
	t.Helper()
	binary, chrome := requiredBinary(t), requiredChrome(t)
	app := newAppProcess(t, binary)
	if err := app.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.stop() })
	readyCtx, readyCancel := context.WithTimeout(context.Background(), readyTimeout)
	if err := waitReady(readyCtx, app.baseURL()); err != nil {
		readyCancel()
		t.Fatalf("application readiness: %v\n%s", err, app.log.Bytes())
	}
	readyCancel()
	fixture := injectApplicationMap(t, app.baseURL(), shape)
	graph := waitApplicationMap(t, app.baseURL(), fixture)

	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options, chromedp.ExecPath(chrome), chromedp.UserDataDir(filepath.Join(t.TempDir(), "chrome")), chromedp.Flag("disable-dev-shm-usage", true))
	if os.Geteuid() == 0 {
		options = append(options, chromedp.NoSandbox)
	}
	allocator, cancelAllocator := chromedp.NewExecAllocator(context.Background(), options...)
	t.Cleanup(cancelAllocator)
	browser, cancelBrowser := chromedp.NewContext(allocator)
	t.Cleanup(cancelBrowser)
	ctx, cancelTest := context.WithTimeout(browser, 100*time.Second)
	t.Cleanup(cancelTest)
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	recorder := newEventRecorder(app.baseURL())
	recorder.listen(ctx)
	smoke := newSmokeRun(t, ctx, artifacts, binary, chrome, app, recorder)
	t.Cleanup(smoke.writeDiagnostics)
	t.Cleanup(func() { assertNoUnexpectedBrowserEvents(t, recorder) })
	smoke.phase("application-map-" + shape)
	if err := chromedp.Run(ctx, cdpruntime.Enable(), network.Enable(), cdplog.Enable(), chromedp.EmulateViewport(1600, 1000), chromedp.Navigate(app.baseURL()+"/")); err != nil {
		t.Fatal(err)
	}
	requireJS(t, ctx, `document.querySelector("#pulse-services")?.textContent.trim() === "150" && document.querySelector("#loading-state").hidden`, 20*time.Second)
	return applicationMapPage{ctx: ctx, app: app, smoke: smoke, fixture: fixture, graph: graph}
}

func mapEvaluate(t *testing.T, ctx context.Context, expression string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, nil)); err != nil {
		t.Fatal(err)
	}
}

func mapClick(t *testing.T, ctx context.Context, selector string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery)); err != nil {
		t.Fatalf("click %s: %v", selector, err)
	}
}

func mapSelect(t *testing.T, ctx context.Context, selector, value string) {
	t.Helper()
	mapEvaluate(t, ctx, fmt.Sprintf(`(() => { const control = document.querySelector(%q); control.value = %q; control.dispatchEvent(new Event("change", {bubbles:true})); })()`, selector, value))
}

func mapSearch(t *testing.T, ctx context.Context, value string) {
	t.Helper()
	mapEvaluate(t, ctx, fmt.Sprintf(`(() => { const search = document.querySelector("#service-search"); search.value = %q; search.dispatchEvent(new Event("input", {bubbles:true})); })()`, value))
}

func mapNoOverflow(t *testing.T, ctx context.Context) {
	t.Helper()
	requireJS(t, ctx, `document.documentElement.scrollWidth <= document.documentElement.clientWidth && document.body.scrollWidth <= document.body.clientWidth`, 5*time.Second)
}

func openDisconnectedInventory(t *testing.T, ctx context.Context) {
	t.Helper()
	if evaluateString(t, ctx, `String(document.querySelector("#disconnected-inventory").hidden)`) == "true" {
		mapClick(t, ctx, "#disconnected-button")
	}
}

func openFullComponent(t *testing.T, ctx context.Context) {
	t.Helper()
	if evaluateString(t, ctx, `document.querySelector("#scope-select").value`) != "all" {
		mapClick(t, ctx, "#reset-scope-button")
	}
}

func mapThemesAndMobile(t *testing.T, page applicationMapPage) {
	t.Helper()
	for _, theme := range []string{"dark", "light"} {
		if evaluateString(t, page.ctx, `document.documentElement.dataset.theme`) != theme {
			mapClick(t, page.ctx, "#theme-button")
		}
		requireJS(t, page.ctx, fmt.Sprintf(`document.documentElement.dataset.theme === %q`, theme), 5*time.Second)
		mapNoOverflow(t, page.ctx)
		page.smoke.screenshot("desktop-" + theme)
		if err := chromedp.Run(page.ctx, chromedp.EmulateViewport(1000, 850)); err != nil {
			t.Fatal(err)
		}
		mapNoOverflow(t, page.ctx)
		requireJS(t, page.ctx, `document.querySelector("#canvas-wrap").hidden || (() => { const label = document.querySelector("#graph-nodes .node-label"); return label && parseFloat(getComputedStyle(label).fontSize) * label.getScreenCTM().a >= 13; })()`, 5*time.Second)
		page.smoke.screenshot("intermediate-" + theme)
		if err := chromedp.Run(page.ctx, chromedp.EmulateViewport(390, 844)); err != nil {
			t.Fatal(err)
		}
		mapNoOverflow(t, page.ctx)
		mapClick(t, page.ctx, "#list-view-button")
		requireJS(t, page.ctx, `!document.querySelector("#mobile-list").hidden && !!document.querySelector("#mobile-list [data-service]")`, 5*time.Second)
		mapClick(t, page.ctx, "#mobile-list [data-service]")
		requireJS(t, page.ctx, `!document.querySelector("#inspector").inert`, 5*time.Second)
		mapNoOverflow(t, page.ctx)
		page.smoke.screenshot("mobile-" + theme + "-inspector")
		mapClick(t, page.ctx, "#close-inspector-button")
		mapClick(t, page.ctx, "#map-view-button")
		mapNoOverflow(t, page.ctx)
		page.smoke.screenshot("mobile-" + theme + "-map")
		if err := chromedp.Run(page.ctx, chromedp.EmulateViewport(1600, 1000)); err != nil {
			t.Fatal(err)
		}
	}
}

// mapEnterKey sends native key events so preventDefault on keydown suppresses
// the character event. chromedp.KeyEvent sends a separate character regardless;
// after the inspector takes focus, that character can activate its Close button.
func mapEnterKey() chromedp.Tasks {
	return chromedp.Tasks{
		input.DispatchKeyEvent(input.KeyDown).WithKey("Enter").WithCode("Enter").
			WithWindowsVirtualKeyCode(13).WithText("\r").WithUnmodifiedText("\r"),
		// Hold Enter across the inspector's focus handoff to exercise the regression.
		chromedp.Sleep(100 * time.Millisecond),
		input.DispatchKeyEvent(input.KeyUp).WithKey("Enter").WithCode("Enter").
			WithWindowsVirtualKeyCode(13),
	}
}

func TestApplicationMap150(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OTELCONTEXT_BROWSER_CASE")) != "application-map" {
		t.Skip("set OTELCONTEXT_BROWSER_CASE=application-map")
	}
	artifacts := artifactDirectory(t)
	for _, shape := range []string{"connected", "disconnected", "mixed", "large"} {
		t.Run(shape, func(t *testing.T) {
			page := openApplicationMap(t, shape, filepath.Join(artifacts, shape))
			switch shape {
			case "connected":
				verifyConnectedApplicationMap(t, page)
			case "disconnected":
				verifyDisconnectedApplicationMap(t, page)
			case "mixed":
				verifyMixedApplicationMap(t, page)
			case "large":
				verifyLargeApplicationMap(t, page)
			}
			page.smoke.complete("application-map-" + shape)
		})
	}
}

func verifyConnectedApplicationMap(t *testing.T, page applicationMapPage) {
	t.Helper()
	ctx := page.ctx
	requireJS(t, ctx, `[...document.querySelector("#component-select").options].filter(option => option.value !== "all").length === 20 && document.querySelectorAll("#graph-nodes .service-node").length > 0 && document.querySelectorAll("#graph-nodes .service-node").length <= 8 && document.querySelector("#scope-select").value === "neighbors"`, 10*time.Second)
	page.smoke.screenshot("initial-connected-scope")
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(1440, 900)); err != nil {
		t.Fatal(err)
	}
	mapClick(t, ctx, "#fit-button")
	requireJS(t, ctx, `(() => { const viewport = document.querySelector("#service-map").getBoundingClientRect(); return [...document.querySelectorAll("#graph-nodes .service-node")].every(node => { const rect = node.getBoundingClientRect(); return rect.left >= viewport.left && rect.right <= viewport.right && rect.top >= viewport.top && rect.bottom <= viewport.bottom; }); })()`, 5*time.Second)
	mapClick(t, ctx, "#graph-nodes .service-node")
	requireJS(t, ctx, `!document.querySelector("#inspector").inert && [...document.querySelectorAll("#graph-nodes .node-label")].every(label => parseFloat(getComputedStyle(label).fontSize) * label.getScreenCTM().a >= 13)`, 5*time.Second)
	mapClick(t, ctx, "#close-inspector-button")
	mapClick(t, ctx, "#fit-button")
	mapClick(t, ctx, "#map-evidence-summary")
	requireJS(t, ctx, `document.querySelector("#map-evidence").open && [...document.querySelectorAll("#graph-nodes .node-label")].every(label => parseFloat(getComputedStyle(label).fontSize) * label.getScreenCTM().a >= 13)`, 5*time.Second)
	mapClick(t, ctx, "#map-evidence-summary")
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(1000, 850)); err != nil {
		t.Fatal(err)
	}
	requireJS(t, ctx, `(() => { const label = document.querySelector("#graph-nodes .node-label"); return parseFloat(getComputedStyle(label).fontSize) * label.getScreenCTM().a >= 13; })()`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(1600, 1000)); err != nil {
		t.Fatal(err)
	}
	openFullComponent(t, ctx)
	requireJS(t, ctx, `(() => { const nodes = [...document.querySelectorAll("#graph-nodes .service-node")].map(node => node.dataset.service).sort(); const members = [...document.querySelectorAll("#map-member-list [data-service]")].map(node => node.dataset.service).sort(); return nodes.length > 0 && nodes.length <= 8 && JSON.stringify(nodes) === JSON.stringify(members); })()`, 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Reload()); err != nil {
		t.Fatal(err)
	}
	requireJS(t, ctx, `(() => { const nodes = [...document.querySelectorAll("#graph-nodes .service-node")].map(node => node.dataset.service).sort(); const members = [...document.querySelectorAll("#map-member-list [data-service]")].map(node => node.dataset.service).sort(); return nodes.length > 0 && nodes.length <= 8 && JSON.stringify(nodes) === JSON.stringify(members); })() && document.querySelector("#scope-select").value === "all"`, 10*time.Second)
	wantEdges, _ := json.Marshal(page.graph.Edges)
	requireJS(t, ctx, fmt.Sprintf(`(() => {
		const expected = new Set(%s.map(edge => edge.source + ">" + edge.target));
		const edges = [...document.querySelectorAll("#graph-edges .graph-edge")];
		const nodes = [...document.querySelectorAll("#graph-nodes .service-node")];
		const visible = new Set(nodes.map(node => node.dataset.service));
		return edges.length === %s.filter(edge => visible.has(edge.source) && visible.has(edge.target)).length && edges.every(edge => {
			const path = edge.matches("path") ? edge : edge.querySelector("path");
			return expected.has(edge.dataset.source + ">" + edge.dataset.target) && path &&
				/M/.test(path.getAttribute("d")) && /[LC]/.test(path.getAttribute("d")) &&
				(path.hasAttribute("marker-end") || !!edge.querySelector("[marker-end]")) &&
				edge.getAttribute("tabindex") === "0";
		}) && nodes.every(node => {
			const label = node.querySelector(".node-label");
			return !!node.querySelector("rect") && label && label.textContent.trim().length > 0 &&
				getComputedStyle(label).display !== "none" && getComputedStyle(label).visibility !== "hidden";
		});
	})()`, wantEdges, wantEdges), 5*time.Second)
	page.smoke.screenshot("directed-components")
	verifyLayoutModule(t, page, 10)

	// Every neighborhood is bounded; together they expose each connected service once.
	var groups []string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`[...document.querySelector("#component-select").options].filter(option => option.value !== "all").map(option => option.value)`, &groups)); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, 150)
	for _, group := range groups {
		mapSelect(t, ctx, "#component-select", group)
		openFullComponent(t, ctx)
		requireJS(t, ctx, `(() => { const count = document.querySelectorAll("#graph-nodes .service-node").length; return count > 0 && count <= 8; })()`, 5*time.Second)
		verifyNeighborhoodEvidence(t, page)
		var names []string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`[...document.querySelectorAll("#graph-nodes .service-node")].map(node => node.dataset.service)`, &names)); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if seen[name] {
				t.Fatalf("service %s appears in multiple neighborhoods", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != 150 {
		t.Fatalf("group expansion exposed %d unique services, want 150", len(seen))
	}
	verifyApplicationMapOverview(t, page, 20)
	mapSelect(t, ctx, "#component-select", groups[0])
	openFullComponent(t, ctx)
	verifyBoundaryConnectionAndBack(t, page)

	// Keyboard activation selects a real measured connection and exposes its evidence.
	source := evaluateString(t, ctx, `document.querySelector("#graph-edges .graph-edge").dataset.source`)
	target := evaluateString(t, ctx, `document.querySelector("#graph-edges .graph-edge").dataset.target`)
	if err := chromedp.Run(ctx, chromedp.Focus("#graph-edges .graph-edge", chromedp.ByQuery), mapEnterKey()); err != nil {
		t.Fatal(err)
	}
	requireJS(t, ctx, fmt.Sprintf(`!document.querySelector("#inspector").inert && (() => { const text = document.querySelector("#inspector").textContent; return text.includes(%q) && text.includes(%q) && /calls/i.test(text) && /latency/i.test(text) && /error/i.test(text); })()`, source, target), 5*time.Second)
	page.smoke.screenshot("connection-evidence")
	mapClick(t, ctx, "#close-inspector-button")
	if err := chromedp.Run(ctx, chromedp.Focus("#graph-nodes .service-node", chromedp.ByQuery), mapEnterKey()); err != nil {
		t.Fatal(err)
	}
	selected := evaluateString(t, ctx, `document.querySelector("#inspector-title").textContent.trim()`)
	requireJS(t, ctx, fmt.Sprintf(`new URL(location.href).searchParams.get("service") === %q`, selected), 5*time.Second)

	verifyApplicationMapScopes(t, page, selected)
	verifyApplicationMapMetricRefresh(t, page)
	mapClick(t, ctx, "#close-inspector-button")
	mapThemesAndMobile(t, page)
}

func verifyApplicationMapScopes(t *testing.T, page applicationMapPage, selected string) {
	t.Helper()
	for _, scope := range []string{"callers", "dependencies", "neighbors"} {
		want := map[string]bool{selected: true}
		for _, edge := range page.graph.Edges {
			if edge.Target == selected && scope != "dependencies" {
				want[edge.Source] = true
			}
			if edge.Source == selected && scope != "callers" {
				want[edge.Target] = true
			}
		}
		encoded, _ := json.Marshal(want)
		mapSelect(t, page.ctx, "#scope-select", scope)
		requireJS(t, page.ctx, fmt.Sprintf(`(() => { const want = %s; const nodes = [...document.querySelectorAll("#graph-nodes .service-node")]; return nodes.length === Object.keys(want).length && nodes.every(node => want[node.dataset.service]); })()`, encoded), 5*time.Second)
	}
}

func verifyApplicationMapMetricRefresh(t *testing.T, page applicationMapPage) {
	t.Helper()
	mapClick(t, page.ctx, "#zoom-in-button")
	const snapshot = `JSON.stringify({nodes:[...document.querySelectorAll("#graph-nodes .service-node")].map(node => [node.dataset.service,node.getAttribute("transform")]).sort(),viewBox:document.querySelector("#service-map").getAttribute("viewBox"),scope:document.querySelector("#scope-select").value,group:document.querySelector("#component-select").value,selection:new URL(location.href).searchParams.get("service")})`
	before := evaluateString(t, page.ctx, snapshot)
	labels := evaluateString(t, page.ctx, `document.querySelector("#graph-edges").textContent`)
	source := evaluateString(t, page.ctx, `document.querySelector("#graph-edges .graph-edge").dataset.source`)
	target := evaluateString(t, page.ctx, `document.querySelector("#graph-edges .graph-edge").dataset.target`)
	beforeGraph := waitApplicationMap(t, page.app.baseURL(), page.fixture)
	var calls int64
	for _, edge := range beforeGraph.Edges {
		if edge.Source == source && edge.Target == target {
			calls = edge.CallCount
		}
	}
	injectApplicationMap(t, page.app.baseURL(), "connected")
	// The real API caches hot-poll responses for 10s. Wait for fresh evidence
	// before the user refresh; an immediate request can correctly return 304.
	waitApplicationMap(t, page.app.baseURL(), page.fixture, func(graph applicationMapGraph) bool {
		for _, edge := range graph.Edges {
			if edge.Source == source && edge.Target == target {
				return edge.CallCount > calls
			}
		}
		return false
	})
	mapClick(t, page.ctx, "#refresh-button")
	requireJS(t, page.ctx, fmt.Sprintf(`document.querySelector("#graph-edges").textContent !== %q && !document.querySelector("#refresh-button").disabled`, labels), 10*time.Second)
	after := evaluateString(t, page.ctx, snapshot)
	if before != after {
		t.Fatalf("metric-only refresh moved layout, viewport, selection, or scope\nbefore: %s\nafter: %s", before, after)
	}
	page.smoke.screenshot("stable-metric-refresh")
}

func verifyDisconnectedApplicationMap(t *testing.T, page applicationMapPage) {
	t.Helper()
	ctx := page.ctx
	openDisconnectedInventory(t, ctx)
	requireJS(t, ctx, `!document.querySelector("#disconnected-inventory").hidden && document.querySelectorAll("#disconnected-list [data-service]").length === 150 && document.querySelectorAll("#graph-nodes .service-node").length === 0 && document.querySelectorAll("#graph-edges .graph-edge").length === 0 && document.querySelector("#disconnected-inventory").textContent.includes("No observed dependencies")`, 10*time.Second)
	name := page.fixture.ServiceNames[149]
	mapSearch(t, ctx, name)
	requireJS(t, ctx, `document.querySelectorAll("#disconnected-list [data-service]").length === 1`, 5*time.Second)
	mapClick(t, ctx, "#disconnected-list [data-service]")
	requireJS(t, ctx, fmt.Sprintf(`!document.querySelector("#inspector").inert && document.querySelector("#inspector-title").textContent.trim() === %q`, name), 5*time.Second)
	page.smoke.screenshot("isolated-service-inspector")
	mapClick(t, ctx, "#close-inspector-button")
	mapSearch(t, ctx, "")
	requireJS(t, ctx, `document.querySelectorAll("#disconnected-list [data-service]").length === 150`, 5*time.Second)
	mapThemesAndMobile(t, page)
}

func verifyMixedApplicationMap(t *testing.T, page applicationMapPage) {
	t.Helper()
	ctx := page.ctx
	requireJS(t, ctx, `[...document.querySelector("#component-select").options].filter(option => option.value !== "all").length === 16 && document.querySelectorAll("#graph-nodes .service-node").length > 0`, 10*time.Second)
	openDisconnectedInventory(t, ctx)
	requireJS(t, ctx, `document.querySelectorAll("#disconnected-list [data-service]").length === 30`, 5*time.Second)
	isolated := page.fixture.ServiceNames[149]
	mapSearch(t, ctx, isolated)
	mapClick(t, ctx, "#disconnected-list [data-service]")
	requireJS(t, ctx, fmt.Sprintf(`document.querySelector("#inspector-title").textContent.trim() === %q`, isolated), 5*time.Second)
	if err := chromedp.Run(ctx, chromedp.Navigate(page.app.baseURL()+"/?service="+url.QueryEscape(isolated))); err != nil {
		t.Fatal(err)
	}
	requireJS(t, ctx, fmt.Sprintf(`!document.querySelector("#disconnected-inventory").hidden && document.querySelector("#inspector-title").textContent.trim() === %q`, isolated), 10*time.Second)
	mapClick(t, ctx, "#close-inspector-button")
	connected := page.fixture.ServiceNames[0]
	mapSearch(t, ctx, connected)
	mapClick(t, ctx, fmt.Sprintf(`#service-list [data-service=%q]`, connected))
	requireJS(t, ctx, fmt.Sprintf(`document.querySelector("#inspector-title").textContent.trim() === %q && !!document.querySelector('#graph-nodes [data-service="%s"]') && document.querySelector("#disconnected-inventory").hidden`, connected, connected), 5*time.Second)
	mapNoOverflow(t, ctx)
	page.smoke.screenshot("search-across-coverage")
}

func verifyLargeApplicationMap(t *testing.T, page applicationMapPage) {
	t.Helper()
	ctx := page.ctx
	requireJS(t, ctx, `(() => { const count = document.querySelectorAll("#graph-nodes .service-node").length; return count > 0 && count <= 8 && [...document.querySelector("#component-select").options].filter(option => option.value !== "all").length === 19 && document.querySelector("#scope-select").value === "neighbors" && /Showing \d+ services/i.test(document.querySelector("#map-summary").textContent) && document.querySelector("#map-summary").textContent.includes("150"); })()`, 10*time.Second)
	page.smoke.screenshot("bounded-large-graph")
	mapClick(t, ctx, "#reset-scope-button")
	requireJS(t, ctx, `(() => { const nodes = [...document.querySelectorAll("#graph-nodes .service-node")].map(node => node.dataset.service).sort(); const members = [...document.querySelectorAll("#map-member-list [data-service]")].map(node => node.dataset.service).sort(); return nodes.length > 0 && nodes.length <= 8 && JSON.stringify(nodes) === JSON.stringify(members); })() && document.querySelector("#scope-select").value === "all"`, 10*time.Second)
	verifyNeighborhoodEvidence(t, page)
	verifyBoundaryConnectionAndBack(t, page)
	verifyApplicationMapOverview(t, page, 19)
	mapNoOverflow(t, ctx)
	page.smoke.screenshot("system-overview-large")
	verifyLayoutModule(t, page, 1)
}

// The atlas must expose the complete inventory at desktop size without zooming
// or scrolling away a tile. Smaller screens may scroll inside the overview.
func verifyApplicationMapOverview(t *testing.T, page applicationMapPage, count int) {
	t.Helper()
	ctx := page.ctx
	if err := chromedp.Run(ctx, chromedp.EmulateViewport(1440, 900)); err != nil {
		t.Fatal(err)
	}
	mapClick(t, ctx, "#system-overview-button")
	want := make([]string, 0, page.fixture.ConnectedServices)
	connected := make(map[string]bool)
	for _, edge := range page.graph.Edges {
		connected[edge.Source] = true
		connected[edge.Target] = true
	}
	for _, name := range page.fixture.ServiceNames {
		if connected[name] {
			want = append(want, name)
		}
	}
	encoded, _ := json.Marshal(want)
	requireJS(t, ctx, fmt.Sprintf(`(() => {
		const cards = [...document.querySelectorAll("#overview-grid .overview-card[data-neighborhood]")];
		const members = [...document.querySelectorAll("#overview-grid .overview-member[data-service]")].map(node => node.dataset.service);
		const want = %s;
		return !document.querySelector("#system-overview").hidden && document.querySelector("#canvas-wrap").hidden &&
			cards.length === %d && members.length === want.length && new Set(members).size === want.length && want.every(id => members.includes(id)) &&
			cards.every(card => { const rect = card.getBoundingClientRect(); return rect.top >= 0 && rect.left >= 0 && rect.right <= innerWidth && rect.bottom <= innerHeight; }) &&
			document.querySelector("#map-summary").textContent.includes("150");
	})()`, encoded, count), 5*time.Second)
	page.smoke.screenshot("complete-system-overview")
	mapNoOverflow(t, ctx)
	mapClick(t, ctx, "#overview-grid .overview-card[data-neighborhood] .overview-open")
	requireJS(t, ctx, `document.querySelector("#system-overview").hidden && !document.querySelector("#canvas-wrap").hidden && document.querySelectorAll("#graph-nodes .service-node").length <= 8 && !!document.querySelector('#neighborhood-list .neighborhood-row[aria-current="true"]')`, 5*time.Second)
	mapClick(t, ctx, "#fit-button")
	requireJS(t, ctx, `[...document.querySelectorAll("#graph-nodes .node-label")].every(label => parseFloat(getComputedStyle(label).fontSize) * Math.hypot(label.getScreenCTM().a,label.getScreenCTM().b) >= 13)`, 5*time.Second)
}

func verifyNeighborhoodEvidence(t *testing.T, page applicationMapPage) {
	t.Helper()
	edges, _ := json.Marshal(page.graph.Edges)
	requireJS(t, page.ctx, fmt.Sprintf(`(() => {
		const nodes = [...document.querySelectorAll("#graph-nodes .service-node")].map(node => node.dataset.service);
		const members = [...document.querySelectorAll("#map-member-list [data-service]")].map(node => node.dataset.service);
		const want = %s.filter(edge => members.includes(edge.source) || members.includes(edge.target)).map(edge => edge.source+">"+edge.target).sort();
		const actual = [...document.querySelectorAll("#map-connection-list .map-connection-row[data-source][data-target]")].map(row => row.dataset.source+">"+row.dataset.target).sort();
		return document.querySelector("#map-summary").textContent.includes(String(members.length)) && /neighborhood/i.test(document.querySelector("#map-summary").textContent) && members.length === nodes.length && new Set(members).size === members.length && nodes.every(id => members.includes(id)) && JSON.stringify(actual) === JSON.stringify(want);
	})()`, edges), 5*time.Second)
}

func verifyBoundaryConnectionAndBack(t *testing.T, page applicationMapPage) {
	t.Helper()
	ctx := page.ctx
	mapEvaluate(t, ctx, `document.querySelector("#map-evidence").open = true`)
	mapClick(t, ctx, "#zoom-in-button")
	const snapshot = `JSON.stringify({group:document.querySelector("#component-select").value,scope:document.querySelector("#scope-select").value,selection:new URL(location.href).searchParams.get("service"),viewBox:document.querySelector("#service-map").getAttribute("viewBox"),nodes:[...document.querySelectorAll("#graph-nodes .service-node")].map(node => node.dataset.service).sort()})`
	before := evaluateString(t, ctx, snapshot)
	boundary := evaluateString(t, ctx, `(() => { const members = new Set([...document.querySelectorAll("#map-member-list [data-service]")].map(node => node.dataset.service)); const row = [...document.querySelectorAll("#map-connection-list .map-connection-row")].find(row => !members.has(row.dataset.source) || !members.has(row.dataset.target)); return row ? JSON.stringify([row.dataset.source,row.dataset.target]) : ""; })()`)
	if boundary == "" {
		t.Fatal("fixture neighborhood has no inspectable boundary connection")
	}
	var pair []string
	if err := json.Unmarshal([]byte(boundary), &pair); err != nil {
		t.Fatal(err)
	}
	mapClick(t, ctx, fmt.Sprintf(`#map-connection-list .map-connection-row[data-source=%q][data-target=%q]`, pair[0], pair[1]))
	requireJS(t, ctx, fmt.Sprintf(`(() => { const nodes = [...document.querySelectorAll("#graph-nodes .service-node")].map(node => node.dataset.service); const text = document.querySelector("#inspector").textContent; return nodes.length === 2 && nodes.includes(%q) && nodes.includes(%q) && !document.querySelector("#inspector").inert && /calls/i.test(text) && /latency/i.test(text) && /error/i.test(text); })()`, pair[0], pair[1]), 5*time.Second)
	mapClick(t, ctx, "#map-back-button")
	requireJS(t, ctx, fmt.Sprintf(`%s === %q`, snapshot, before), 5*time.Second)
}

func verifyLayoutModule(t *testing.T, page applicationMapPage, expectedComponents int) {
	t.Helper()
	var result struct {
		StableNeighborhoods bool     `json:"stable_neighborhoods"`
		ExactNeighborhoods  bool     `json:"exact_neighborhoods"`
		StableKey           bool     `json:"stable_key"`
		StablePositions     bool     `json:"stable_positions"`
		FiniteLayout        bool     `json:"finite_layout"`
		Components          int      `json:"components"`
		Disconnected        int      `json:"disconnected"`
		DurationMS          float64  `json:"duration_ms"`
		ReversedDuration    float64  `json:"reversed_duration_ms"`
		CardCrossings       []string `json:"card_crossings"`
	}
	expression := `(async () => {
		const map = await import("/static/map-layout.js");
		const graph = await (await fetch("/api/system/graph")).json();
		const reversed = graph.nodes.toReversed().map(node => ({...node,metrics:{...node.metrics,error_rate:0.91,total_traces:999999},health_score:4}));
		const edges = graph.edges.toReversed().map(edge => ({...edge,call_count:999999,error_rate:0.91,avg_latency_ms:4321}));
		const first = map.layoutGraph(graph.nodes,graph.edges);
		const second = map.layoutGraph(reversed,edges);
		const partition = map.partitionGraph(graph.nodes,graph.edges);
		const neighborhoods = map.partitionNeighborhoods(graph.nodes,graph.edges);
		const changedNeighborhoods = map.partitionNeighborhoods(reversed,edges);
		const membership = partition => JSON.stringify(partition.components.map(group => [group.id,group.nodeIds]));
		const allMembers = neighborhoods.components.flatMap(group => group.nodeIds);
		const keys = edges => edges.map(edge => edge.source+">"+edge.target).sort().join("|");
		const exactNeighborhoods = allMembers.length === graph.nodes.length && new Set(allMembers).size === graph.nodes.length && neighborhoods.components.every(group => {
			const members = new Set(group.nodeIds);
			return members.size > 0 && members.size <= 8 && keys(group.internalEdges) === keys(graph.edges.filter(edge => members.has(edge.source) && members.has(edge.target))) &&
				keys(group.incomingEdges) === keys(graph.edges.filter(edge => !members.has(edge.source) && members.has(edge.target))) &&
				keys(group.outgoingEdges) === keys(graph.edges.filter(edge => members.has(edge.source) && !members.has(edge.target)));
		});
		const positions = layout => JSON.stringify([...layout.positions].sort(([a],[b]) => a.localeCompare(b)));
		// Slab clipping detects positive card-interior overlap, excluding a route
		// that only touches a boundary and excluding its own endpoint cards.
		const intersects = (a,b,center) => {
			let low = 0, high = 1;
			for (const [axis,half] of [["x",map.CARD_WIDTH/2],["y",map.CARD_HEIGHT/2]]) {
				const delta = b[axis]-a[axis], min = center[axis]-half, max = center[axis]+half;
				if (!delta) { if (a[axis] <= min || a[axis] >= max) return false; continue; }
				let from = (min-a[axis])/delta, to = (max-a[axis])/delta;
				if (from > to) [from,to] = [to,from];
				low = Math.max(low,from); high = Math.min(high,to);
				if (low >= high) return false;
			}
			return low < high;
		};
		const crossings = new Set();
		for (const edge of graph.edges) {
			const route = first.routes.get(edge.source+">"+edge.target) || [];
			for (const node of graph.nodes) {
				if (node.id === edge.source || node.id === edge.target) continue;
				const center = first.positions.get(node.id);
				for (let i = 1; i < route.length; i++) {
					if (intersects(route[i-1],route[i],center)) {
						crossings.add(edge.source+">"+edge.target+" through "+node.id); break;
					}
				}
			}
		}
		return {
			stable_neighborhoods:membership(neighborhoods) === membership(changedNeighborhoods),exact_neighborhoods:exactNeighborhoods,
			stable_key:map.topologyKey(graph.nodes,graph.edges) === map.topologyKey(reversed,edges),
			stable_positions:positions(first) === positions(second),
			finite_layout:[...first.positions.values()].every(point => Number.isFinite(point.x) && Number.isFinite(point.y)) && first.routes.size === graph.edges.length,
			components:partition.components.length,disconnected:partition.disconnected.length,
			duration_ms:first.durationMs,reversed_duration_ms:second.durationMs,card_crossings:[...crossings]
		};
	})()`
	if err := chromedp.Run(page.ctx, chromedp.Evaluate(expression, &result, func(params *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return params.WithAwaitPromise(true)
	})); err != nil {
		t.Fatal(err)
	}
	if !result.StableNeighborhoods || !result.ExactNeighborhoods || !result.StableKey || !result.StablePositions || !result.FiniteLayout || result.Components != expectedComponents || result.Disconnected != 0 || len(result.CardCrossings) != 0 {
		t.Fatalf("layout module contract: %+v", result)
	}
	t.Logf("150-service directed layout on this host: %.1f ms, reordered metric refresh %.1f ms", result.DurationMS, result.ReversedDuration)
	writeJSONFile(t, filepath.Join(page.smoke.artifacts, "layout-measurement.json"), result)
}

import { CARD_WIDTH, CARD_HEIGHT, layoutGraph } from "./map-layout.js";

const $ = (id) => document.getElementById(id);
const NS = "http://www.w3.org/2000/svg";
const state = {
  mode: "services", environment: undefined, minutes: 15, focus: "", hops: 1,
  start: "", end: "", catalog: null, graph: null, selected: null,
  request: 0, controller: null, refreshController: null, refreshRequest: 0,
  hosts: false, layout: null, view: null, dragging: null,
};
const number = (n, digits = 0) => Number.isFinite(n) ? n.toLocaleString(undefined, { maximumFractionDigits: digits }) : "—";
const envLabel = (env) => env === undefined ? "All environments" : env || "Unknown";
const bytes = (n) => {
  if (!Number.isFinite(n) || n < 0) return "Unavailable";
  if (n >= 1024 ** 3) return number(n / 1024 ** 3, 2) + " GiB";
  if (n >= 1024 ** 2) return number(n / 1024 ** 2, 2) + " MiB";
  return n >= 1024 ? number(n / 1024, 2) + " KiB" : number(n) + " B";
};
const compact = (s, n = 24) => s.length > n ? s.slice(0, n - 1) + "…" : s;
const put = (id, text) => { $(id).textContent = text; };
const launch = (promise) => { promise.catch(showError); };

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function svgElement(tag, attrs = {}, text) {
  const node = document.createElementNS(NS, tag);
  for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, String(value));
  if (text !== undefined) node.textContent = text;
  return node;
}

function svgText(parent, x, y, text, size = 12, fill = "#e1ebf6") {
  parent.append(svgElement("text", { x, y, "font-size": size, fill }, text));
}

function action(node, label, callback) {
  node.setAttribute("role", "button");
  node.setAttribute("tabindex", "0");
  node.setAttribute("aria-label", label);
  node.addEventListener("click", callback);
  node.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      callback();
    }
  });
}

function showError(error) {
  if (error?.name === "AbortError") return;
  $("error").hidden = false;
  put("error", error?.message || "The request could not complete. Refresh to try again.");
  put("connection", "Read failed");
}

async function fetchJSON(path, signal) {
  const response = await fetch(path, {
    signal, headers: { Accept: "application/json" }, cache: "no-store", credentials: "same-origin",
  });
  if (!response.ok) {
    const message = (await response.text()).slice(0, 500).trim();
    throw new Error(message || "HTTP " + response.status);
  }
  return response.json();
}

function rangeParams() {
  return new URLSearchParams({ start: state.start, end: state.end });
}

function newRange() {
  const end = new Date();
  state.end = end.toISOString();
  state.start = new Date(end.getTime() - state.minutes * 60000).toISOString();
}

function validNodes(nodes, cap) {
  return Array.isArray(nodes) && nodes.length <= cap && nodes.every((n) =>
    n && /^[a-f0-9]{64}$/.test(n.id) && typeof n.name === "string" && n.name.length <= 255 &&
    typeof n.environment === "string" && Array.isArray(n.hosts) && n.hosts.length <= 20 &&
    n.hosts.every((h) => typeof h === "string" && h.length <= 255));
}

async function refresh() {
  const generation = ++state.refreshRequest;
  state.refreshController?.abort();
  state.refreshController = new AbortController();
  const refreshController = state.refreshController;
  const signal = refreshController.signal;
  state.controller?.abort();
  state.request++;
  $("refresh").disabled = true;
  $("error").hidden = true;
  newRange();
  const timer = setTimeout(() => refreshController.abort(), 8000);
  try {
    const [catalogResult, runtimeResult] = await Promise.allSettled([
      fetchJSON("/api/topology/catalog?" + rangeParams(), signal),
      fetchJSON("/api/stats/runtime", signal),
    ]);
    if (generation !== state.refreshRequest) return;
    if (runtimeResult.status === "fulfilled") renderRuntime(runtimeResult.value);
    else {
      renderRuntimeUnavailable();
      showError(runtimeResult.reason);
    }
    if (catalogResult.status !== "fulfilled") throw catalogResult.reason;
    const catalog = catalogResult.value;
    if (!validNodes(catalog.services, 1000) || !Array.isArray(catalog.environments) ||
        !catalog.environments.every((e) => typeof e === "string" && e.length <= 255)) {
      throw new Error("Invalid topology catalog response.");
    }
    if (!state.catalog && catalog.environments.length) state.environment = catalog.environments[0];
    state.catalog = catalog;
    renderCatalog();
    dirtyFilters();
    await requestGraph();
    if (generation === state.refreshRequest) put("refresh-time", "Refreshed " + new Date().toLocaleTimeString() + " · manual refresh");
  } catch (error) {
    if (generation !== state.refreshRequest) return;
    showError(error.name === "AbortError" ? new Error("Refresh timed out. Try again.") : error);
    if (!state.graph) emptyMap("Topology unavailable", "Refresh after checking the server status.");
  } finally {
    clearTimeout(timer);
    if (generation === state.refreshRequest) $("refresh").disabled = false;
  }
}

function renderCatalog() {
  const c = state.catalog;
  put("total-services", number(c.service_names));
  put("total-instances", number(c.instances));
  put("total-hosts", number(c.hosts));
  put("identity-count-note", c.unresolved ? number(c.unresolved) + " additional identities unresolved" : "Known instance identities");
  $("env").replaceChildren(new Option("All environments", "*"));
  for (const env of c.environments) $("env").append(new Option(envLabel(env), JSON.stringify(env)));
  $("env").value = state.environment === undefined ? "*" : JSON.stringify(state.environment);
  if ($("env").selectedIndex < 0) {
    $("env").append(new Option(envLabel(state.environment), JSON.stringify(state.environment)));
    $("env").value = JSON.stringify(state.environment);
  }
  renderList();
}

function renderList() {
  const query = $("search").value.trim().toLowerCase();
  const items = (state.catalog?.services || []).filter((s) =>
    (state.environment === undefined || s.environment === state.environment) &&
    (s.name + " " + s.namespace).toLowerCase().includes(query));
  $("service-list").replaceChildren();
  put("catalog-count", items.length + " services");
  for (const service of items.slice(0, 100)) {
    const button = element("button", "service-row" + (state.focus === service.id ? " selected" : ""));
    button.append(element("span", "name", service.name));
    button.append(element("span", "count", number(service.instance_count) + " inst."));
    button.title = [service.namespace, service.name, envLabel(service.environment)].filter(Boolean).join(" · ");
    button.addEventListener("click", () => {
      state.focus = service.id;
      state.selected = null;
      launch(requestGraph());
    });
    $("service-list").append(button);
  }
  if (!items.length) $("service-list").append(element("p", "no-list", "No matching services"));
  put("feedback", items.length > 100 ? "First 100 matches. Refine your search." :
    state.catalog?.truncated ? "Catalog limited to 1,000 service groups." : "");
}

function dirtyFilters() {
  const value = $("env").value;
  const environment = value === "*" ? undefined : JSON.parse(value);
  const changed = environment !== state.environment || Number($("time").value) !== state.minutes;
  $("apply").disabled = !changed;
  put("query-state", changed ? "Filters changed · apply to update" :
    envLabel(state.environment) + " · " + $("time").selectedOptions[0].text.toLowerCase());
  $("query-state").className = "query-state" + (changed ? " dirty" : "");
}

async function requestGraph() {
  const generation = ++state.request;
  state.controller?.abort();
  const controller = new AbortController();
  state.controller = controller;
  const params = rangeParams();
  params.set("mode", state.mode);
  params.set("hops", String(state.hops));
  params.set("limit", "40");
  if (state.environment !== undefined) params.set("environment", state.environment);
  if (state.focus) params.set("service_id", state.focus);
  $("error").hidden = true;
  $("canvas").setAttribute("aria-busy", "true");
  $("services").setAttribute("aria-pressed", String(state.mode === "services"));
  $("instances").setAttribute("aria-pressed", String(state.mode === "instances"));
  put("env-badge", envLabel(state.environment));
  const focused = state.catalog?.services.find((s) => s.id === state.focus);
  put("scope", focused ? focused.name + " · " + state.hops + "-hop neighborhood" : "Environment overview");
  renderList();
  emptyMap("Loading scoped topology…", "Only summaries for this environment and neighborhood are requested.");
  const timer = setTimeout(() => controller.abort(), 8000);
  try {
    const graph = await fetchJSON("/api/topology/graph?" + params, controller.signal);
    if (generation !== state.request) return;
    if (!validNodes(graph.nodes, 100) || !Array.isArray(graph.edges) || graph.edges.length > 200 || !graph.meta) {
      throw new Error("Invalid topology graph response.");
    }
    const ids = new Set(graph.nodes.map((n) => n.id));
    if (graph.edges.some((e) => !e || !ids.has(e.source) || !ids.has(e.target))) throw new Error("Graph contains an unresolved endpoint.");
    state.graph = graph;
    state.layout = layoutGraph(graph.nodes, graph.edges, { ranker: "network-simplex", ranksep: 52, nodesep: 38 });
    state.view = null;
    if (!graph.nodes.some((n) => n.id === state.selected?.id) && !graph.edges.some((e) => e.id === state.selected?.id)) {
      const incoming = new Set(graph.edges.map((edge) => edge.target));
      const initial = graph.nodes.find((node) => node.service_id === state.focus) ||
        graph.nodes.find((node) => !incoming.has(node.id)) || graph.nodes[0];
      state.selected = initial ? { kind: "node", id: initial.id } : null;
    }
    put("connection", "Connected");
    renderGraph();
    renderInspector();
  } catch (error) {
    if (generation !== state.request) return;
    state.graph = null;
    state.selected = null;
    renderInspector();
    emptyMap("Topology could not load", error.name === "AbortError" ? "The request timed out. Refresh to retry." : error.message);
    showError(error.name === "AbortError" ? new Error("Topology request timed out.") : error);
  } finally {
    clearTimeout(timer);
    if (generation === state.request) $("canvas").removeAttribute("aria-busy");
  }
}

function emptyMap(title, description) {
  const empty = element("div", "empty");
  empty.append(element("b", "", title), element("p", "", description));
  $("canvas").replaceChildren(empty);
}

function fitView() {
  if (!state.layout) return;
  const bounds = state.layout.bounds;
  state.view = { x: -15, y: -15, width: Math.max(350, bounds.width + 30), height: Math.max(180, bounds.height + 30) };
  updateView();
}

function updateView() {
  const svg = $("canvas").querySelector("svg");
  if (svg && state.view) {
    const v = state.view;
    svg.setAttribute("viewBox", [v.x, v.y, v.width, v.height].join(" "));
  }
}

function zoom(factor) {
  if (!state.view) return;
  const v = state.view;
  const width = Math.max(100, Math.min(20000, v.width * factor));
  const height = v.height * width / v.width;
  state.view = { x: v.x + (v.width - width) / 2, y: v.y + (v.height - height) / 2, width, height };
  updateView();
}

function renderGraph() {
  const graph = state.graph;
  if (!graph) return;
  put("graph-count", graph.nodes.length + " nodes · " + graph.edges.length + " call edges");
  put("bounds", graph.nodes.length + " of " + graph.meta.available_nodes + " scoped nodes shown. " +
    (graph.meta.truncated ? "Result limited; focus a service or shorten the range." : "Select a node or edge to inspect."));
  put("coverage-label", (graph.meta.coverage === "limited" ? "Limited" : "Observed") + " topology · five-minute resolution");
  const range = new Date(graph.meta.start).toLocaleString() + " – " + new Date(graph.meta.end).toLocaleString();
  put("coverage-note", graph.meta.note + " Effective buckets: " + range + ". " +
    number(graph.meta.pending_parent_spans) + " parent joins pending process-wide; " +
    number(graph.meta.process_dropped) + " observations limited and " + number(graph.meta.process_unmatched) + " joins expired process-wide. " +
    (graph.meta.flush_failed ? "Last persistence attempt failed." : ""));
  $("coverage-label").classList.toggle("identity-warning", graph.meta.coverage === "limited");
  if (!graph.nodes.length) {
    emptyMap("No resources in this scope", "Send telemetry with service.name and service.instance.id. Missing environment attributes appear under Unknown. Existing pre-upgrade history has no instance identity.");
    return;
  }
  const svg = svgElement("svg", { role: "group", "aria-label": "Observed " + state.mode + " calls in " + envLabel(state.environment) });
  const defs = svgElement("defs");
  const marker = svgElement("marker", { id: "call-arrow", viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 6, markerHeight: 6, orient: "auto" });
  marker.append(svgElement("path", { d: "M0 0L10 5L0 10Z", fill: "#8db8e3" }));
  defs.append(marker);
  svg.append(defs);
  const byID = new Map(graph.nodes.map((n) => [n.id, n]));
  for (const edge of graph.edges) {
    const points = state.layout.routes.get(edge.source + ">" + edge.target);
    if (!points?.length) continue;
    const d = points.map((p, i) => (i ? "L" : "M") + p.x + " " + p.y).join(" ");
    const group = svgElement("g", { class: "graph-edge" + (state.selected?.id === edge.id ? " selected" : ""), "data-id": edge.id });
    group.append(svgElement("path", { d, fill: "none", stroke: "transparent", "stroke-width": 18 }));
    group.append(svgElement("path", { class: "visible-edge", d, fill: "none", stroke: "#6f9ec9", "stroke-width": 2, "marker-end": "url(#call-arrow)" }));
    const middle = points[Math.floor(points.length / 2)];
    group.append(svgElement("rect", { x: middle.x - 29, y: middle.y - 20, width: 60, height: 18, rx: 4, fill: "#142434" }));
    svgText(group, middle.x - 25, middle.y - 7, number(edge.request_rate_rps, 2) + "/s", 13, "#b4d1eb");
    action(group, byID.get(edge.source).name + " calls " + byID.get(edge.target).name, () => select("edge", edge.id));
    svg.append(group);
  }
  for (const node of graph.nodes) {
    const pos = state.layout.positions.get(node.id);
    const group = svgElement("g", { class: "graph-node" + (state.selected?.id === node.id ? " selected" : ""), "data-id": node.id,
      transform: "translate(" + (pos.x - CARD_WIDTH / 2) + " " + (pos.y - CARD_HEIGHT / 2) + ")" });
    group.append(svgElement("rect", { class: "box", width: CARD_WIDTH, height: CARD_HEIGHT, rx: 7, fill: state.selected?.id === node.id ? "#233d56" : "#1c2c3d", stroke: "#46647f" }));
    group.append(svgElement("circle", { cx: 14, cy: 19, r: 3, fill: node.errors ? "#efb655" : node.requests ? "#6ed3b1" : "#94a8ba" }));
    svgText(group, 24, 23, compact(node.name, 18), 18);
    const label = state.mode === "instances" ? node.instance || (node.identity === "host_workload" ? "Host + workload identity" :
      node.hosts.length ? "Host: " + node.hosts[0] + " (unresolved)" : "Unresolved instance") :
      number(node.instance_count) + (node.instance_count === 1 ? " instance" : " instances") +
        (node.unresolved_count ? " · " + node.unresolved_count + " unresolved" : "");
    svgText(group, 12, 43, compact(label, 26), 13, "#aec5dc");
    svgText(group, 12, 61, state.hosts ? compact(node.hosts.join(", ") || "Host unknown", 30) :
      compact([node.namespace, envLabel(node.environment)].filter(Boolean).join(" · "), 26), 13, "#94afc7");
    svgText(group, 12, 77, node.requests ? number(node.request_rate_rps, 2) + "/s · " + number(node.error_rate * 100, 2) + "% err" : "No entry spans observed", 13, "#bdcfe1");
    group.append(svgElement("title", {}, [node.name, node.instance, node.namespace, envLabel(node.environment), ...node.hosts].filter(Boolean).join(" · ")));
    action(group, node.name + (node.instance ? " instance " + node.instance : "") + " in " + envLabel(node.environment), () => select("node", node.id));
    svg.append(group);
  }
  $("canvas").replaceChildren(svg);
  if (!state.view) fitView();
  else updateView();
}

function select(kind, id) {
  const restoreFocus = document.activeElement?.getAttribute("data-id") === id;
  state.selected = { kind, id };
  renderGraph();
  renderInspector();
  if (restoreFocus) $("canvas").querySelector('[data-id="' + id + '"]')?.focus();
}

function renderInspector() {
  const graph = state.graph;
  const node = state.selected?.kind === "node" ? graph?.nodes.find((n) => n.id === state.selected.id) : null;
  const edge = state.selected?.kind === "edge" ? graph?.edges.find((e) => e.id === state.selected.id) : null;
  const source = edge ? graph.nodes.find((n) => n.id === edge.source) : null;
  const target = edge ? graph.nodes.find((n) => n.id === edge.target) : null;
  const selected = node || edge;
  put("inspect-kind", edge ? "Selected call edge" : state.mode === "instances" ? "Selected instance" : "Selected service");
  put("inspect-name", edge ? source.name + " → " + target.name : node?.name || "Select a node or edge");
  put("inspect-env", node ? envLabel(node.environment) : edge ? envLabel(source.environment) + " → " + envLabel(target.environment) : "—");
  put("inspect-id", edge ? (source.instance || source.name) + " → " + (target.instance || target.name) :
    node ? node.instance || number(node.instance_count) + (node.instance_count === 1 ? " known instance" : " known instances") : "");
  const requests = node?.requests ?? edge?.calls ?? 0;
  const uncertain = node?.unresolved_count > 0 || graph?.meta.coverage === "limited";
  put("inspect-status", !selected ? "No selection" : uncertain ? "Limited evidence" : selected.errors ? "Errors observed" : requests ? "No observed errors" : "No entry spans");
  $("inspect-status").className = "pill" + (uncertain || selected?.errors ? " warn" : "");
  put("attr-label", edge ? "Call count" : "Hosts · latest placement");
  put("inspect-host", edge ? number(edge.calls) : node?.hosts.join(", ") || "Unknown");
  put("inspect-coverage", node?.unresolved_count ? "Instance identity incomplete" : graph?.meta.coverage || "—");
  put("rate-label", edge ? "Calls / sec" : "Entry spans / sec");
  put("inspect-rate", requests ? number(selected.request_rate_rps, 2) : "—");
  put("inspect-lat", requests ? number(selected.avg_latency_ms, 2) + " ms" : "—");
  put("inspect-err", requests ? number(selected.error_rate * 100, 2) + "%" : "—");
  put("relation", edge ? "Observed trace parent–child relationship. Sharing a host never creates a call edge." :
    node?.unresolved_count ? "Supply service.instance.id, or host identity plus pod/container/process identity, to distinguish copies reliably." :
    "Node rates use server/consumer spans and root spans. Select a call edge for its latency and errors.");
  $("drill").disabled = !selected || !!edge && state.mode === "instances";
  put("drill", state.mode === "instances" && node ? "Back to service view →" : "Inspect instances →");
  const attrs = node ? {
    "service.name": node.name, "service.namespace": node.namespace || null,
    "deployment.environment.name": node.environment || null,
    "service.instance.id": node.instance || null, "identity": node.identity,
    "latest_hosts": node.hosts, "known_instances": node.instance_count, "unresolved_identities": node.unresolved_count,
  } : edge ? { source: source.id, target: target.id, calls: edge.calls, errors: edge.errors, evidence: "trace parent-child" } : {};
  put("attributes", JSON.stringify(attrs, null, 2));
}

function renderRuntime(data) {
  const s = data.storage, e = data.exports;
  if (!s || !e) throw new Error("Invalid runtime stats response.");
  put("mode", data.mode ? data.mode + " mode" : "Mode unavailable");
  put("retention", data.retention_days ? "Retention configured: " + data.retention_days + " days" : "Retention unavailable");
  put("total-exports", number(e.trace_exports + e.metric_exports + e.log_exports));
  put("total-storage", s.complete ? bytes(s.total_bytes) : "Unavailable");
  put("storage-total", s.complete ? bytes(s.total_bytes) + " on disk" : "Incomplete measurement");
  put("storage-main", bytes(s.main_bytes));
  put("storage-aggregate", bytes(s.aggregate_bytes));
  put("storage-sidecars", bytes(s.sidecar_bytes));
  put("disk-free", Number.isFinite(s.free_bytes) ? bytes(s.free_bytes) + " free · main filesystem" : "Free space unavailable");
  put("storage-note", s.note);
  $("storage-bar").replaceChildren();
  if (s.complete && s.total_bytes > 0) {
    for (const [size, color] of [[s.main_bytes, "#76b6ff"], [s.aggregate_bytes, "#66c7b4"], [s.sidecar_bytes, "#d7ad68"]]) {
      const part = element("i");
      part.style.width = Math.max(0, Math.min(100, size / s.total_bytes * 100)) + "%";
      part.style.background = color;
      $("storage-bar").append(part);
    }
  }
  put("trace-exports", number(e.trace_exports));
  put("metric-exports", number(e.metric_exports));
  put("log-exports", number(e.log_exports));
  put("failed-exports", number(e.rejected_exports));
  put("ack-p99", e.latency_samples ? number(e.ack_p99_ms, 2) + " ms" : "Unavailable");
  put("queue-usage", data.queue_ratio === null ? "Unavailable" : number(data.queue_ratio * 100, 1) + "%");
  put("app-memory", bytes(data.heap_bytes));
  put("latency-note", e.latency_note + " Samples: " + number(e.latency_samples) + ".");
}

function renderRuntimeUnavailable() {
  for (const id of ["total-exports", "total-storage", "storage-total", "storage-main", "storage-aggregate", "storage-sidecars", "disk-free",
    "trace-exports", "metric-exports", "log-exports", "failed-exports", "ack-p99", "queue-usage", "app-memory"]) put(id, "Unavailable");
  $("storage-bar").replaceChildren();
}

$("env").addEventListener("change", dirtyFilters);
$("time").addEventListener("change", dirtyFilters);
$("search").addEventListener("input", renderList);
$("apply").addEventListener("click", () => {
  state.environment = $("env").value === "*" ? undefined : JSON.parse($("env").value);
  state.minutes = Number($("time").value);
  state.focus = "";
  state.selected = null;
  dirtyFilters();
  launch(refresh());
});
for (const mode of ["services", "instances"]) $(mode).addEventListener("click", () => {
  if (state.mode === mode) return;
  state.mode = mode;
  state.selected = null;
  launch(requestGraph());
});
$("depth").addEventListener("change", () => { state.hops = Number($("depth").value); launch(requestGraph()); });
$("hosts").addEventListener("change", () => { state.hosts = $("hosts").checked; renderGraph(); });
$("show-group").addEventListener("click", () => {
  state.focus = "";
  $("search").value = "";
  state.selected = null;
  launch(requestGraph());
});
$("drill").addEventListener("click", () => {
  const node = state.graph?.nodes.find((n) => n.id === state.selected?.id);
  const edge = state.graph?.edges.find((e) => e.id === state.selected?.id);
  const target = node || state.graph?.nodes.find((n) => n.id === edge?.target);
  if (!target) return;
  state.focus = target.service_id;
  state.mode = state.mode === "instances" && node ? "services" : "instances";
  state.selected = null;
  launch(requestGraph());
});
$("refresh").addEventListener("click", () => launch(refresh()));
$("zoom-fit").addEventListener("click", fitView);
$("zoom-in").addEventListener("click", () => zoom(.8));
$("zoom-out").addEventListener("click", () => zoom(1.25));
$("canvas").addEventListener("pointerdown", (event) => {
  if (!state.view || event.target.closest("[role=button]") || event.button !== 0) return;
  state.dragging = { x: event.clientX, y: event.clientY, view: { ...state.view } };
  $("canvas").setPointerCapture(event.pointerId);
});
$("canvas").addEventListener("pointermove", (event) => {
  if (!state.dragging) return;
  const d = state.dragging;
  const rect = $("canvas").getBoundingClientRect();
  const scale = Math.max(d.view.width / rect.width, d.view.height / rect.height);
  state.view = { ...d.view, x: d.view.x - (event.clientX - d.x) * scale, y: d.view.y - (event.clientY - d.y) * scale };
  updateView();
});
for (const type of ["pointerup", "pointercancel", "lostpointercapture"]) $("canvas").addEventListener(type, () => { state.dragging = null; });
$("canvas").addEventListener("keydown", (event) => {
  if (!state.view || event.target !== $("canvas")) return;
  const directions = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1] };
  const direction = directions[event.key];
  if (!direction) return;
  event.preventDefault();
  state.view.x += direction[0] * state.view.width * .1;
  state.view.y += direction[1] * state.view.height * .1;
  updateView();
});
launch(refresh());

const SVG_NS = "http://www.w3.org/2000/svg";
const GOLDEN_ANGLE = 2.399963229728653;
const POLL_INTERVAL_MS = 30000;
const TOOL_CACHE_MS = 300000;

const byId = (id) => document.getElementById(id);
const dom = {
  connection: byId("connection-status"),
  connectionLabel: byId("connection-label"),
  pulseHealth: byId("pulse-health"),
  pulseErrors: byId("pulse-errors"),
  pulseP99: byId("pulse-p99"),
  pulseServices: byId("pulse-services"),
  pulseUptime: byId("pulse-uptime"),
  pulseDB: byId("pulse-db"),
  coverage: byId("coverage-badge"),
  commandButton: byId("command-button"),
  commandDialog: byId("command-dialog"),
  commandSearch: byId("command-search"),
  commandResults: byId("command-results"),
  closeCommand: byId("close-command-button"),
  shortcutDialog: byId("shortcut-dialog"),
  closeShortcuts: byId("close-shortcut-button"),
  connectEndpoints: byId("connect-endpoints"),
  emptyEndpoints: byId("empty-endpoints"),
  refresh: byId("refresh-button"),
  theme: byId("theme-button"),
  search: byId("service-search"),
  searchCount: byId("search-count"),
  loading: byId("loading-state"),
  error: byId("error-state"),
  errorMessage: byId("error-message"),
  empty: byId("empty-state"),
  canvas: byId("canvas-wrap"),
  mobileList: byId("mobile-list"),
  retry: byId("retry-button"),
  mapView: byId("map-view-button"),
  listView: byId("list-view-button"),
  map: byId("service-map"),
  rings: byId("graph-rings"),
  edges: byId("graph-edges"),
  nodes: byId("graph-nodes"),
  minimapButton: byId("minimap-button"),
  minimap: byId("service-minimap"),
  minimapEdges: byId("minimap-edges"),
  minimapNodes: byId("minimap-nodes"),
  minimapViewport: byId("minimap-viewport"),
  zoomIn: byId("zoom-in-button"),
  zoomOut: byId("zoom-out-button"),
  fit: byId("fit-button"),
  serviceList: byId("service-list"),
  serviceCount: byId("service-count"),
  severity: byId("severity-summary"),
  impactBanner: byId("impact-banner"),
  impactLabel: byId("impact-label"),
  clearImpact: byId("clear-impact-button"),
  inspector: byId("inspector"),
  inspectorScrim: byId("inspector-scrim"),
  inspectorTitle: byId("inspector-title"),
  inspectorStatus: byId("inspector-status"),
  inspectorHealth: byId("inspector-health"),
  inspectorBody: byId("inspector-body"),
  closeInspector: byId("close-inspector-button"),
  tabs: Array.from(document.querySelectorAll(".inspector-tabs [role=tab]")),
  toast: byId("toast"),
};

const state = {
  graph: null,
  dashboard: null,
  stats: null,
  loading: true,
  error: "",
  refreshing: false,
  selected: null,
  activeTab: "overview",
  impactRoot: null,
  query: "",
  mobileMode: "map",
  mobileModeChosen: false,
  anomalies: new Set(),
  tools: new Map(),
  apiCache: new Map(),
  viewBox: { x: 0, y: 0, width: 1000, height: 1000 },
  uptimeBase: null,
  uptimeObservedAt: 0,
  uptimeAuthoritative: null,
  commandMode: null,
};

function textElement(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  node.textContent = text;
  return node;
}

function svgElement(tag, attrs) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const entry of Object.entries(attrs || {})) {
    node.setAttribute(entry[0], String(entry[1]));
  }
  return node;
}

function finite(value, fallback) {
  return Number.isFinite(Number(value)) ? Number(value) : fallback;
}

function trimFixed(value, digits) {
  const raw = value.toFixed(digits);
  if (!raw.includes(".")) return raw;
  return raw.replace(/\.?0+$/, "");
}

function formatRatio(value, digits) {
  if (!Number.isFinite(Number(value))) return "—";
  return trimFixed(Number(value) * 100, digits === undefined ? 1 : digits) + "%";
}

function formatPercent(value, digits) {
  if (!Number.isFinite(Number(value))) return "—";
  return trimFixed(Number(value), digits === undefined ? 1 : digits) + "%";
}

function formatMs(value) {
  const ms = Number(value);
  if (!Number.isFinite(ms)) return "—";
  if (ms < 1) return trimFixed(ms, 1) + "ms";
  if (ms < 1000) return trimFixed(ms, 0) + "ms";
  if (ms < 60000) return trimFixed(ms / 1000, 1) + "s";
  return trimFixed(ms / 60000, 1) + "m";
}

function formatCount(value) {
  const count = Number(value);
  if (!Number.isFinite(count)) return "—";
  if (Math.abs(count) >= 1000000) return trimFixed(count / 1000000, 1) + "M";
  if (Math.abs(count) >= 1000) return trimFixed(count / 1000, 1) + "K";
  return trimFixed(count, 0);
}

function formatMB(value) {
  const size = Number(value);
  if (!Number.isFinite(size)) return "—";
  if (size >= 1024) return trimFixed(size / 1024, 1) + "GB";
  return trimFixed(size, size < 10 ? 1 : 0) + "MB";
}

function normalizeStatus(node) {
  const raw = String(node && node.status || "").toLowerCase();
  const score = finite(node && node.health_score, 1);
  if (raw.includes("critical") || raw.includes("unhealthy") || raw.includes("error") || score < 0.5) {
    return "critical";
  }
  if (raw.includes("degraded") || raw.includes("warn") || score < 0.85) {
    return "degraded";
  }
  if (raw.includes("healthy") || raw.includes("ok") || score >= 0.85) {
    return "healthy";
  }
  return "unknown";
}

function statusColor(status) {
  return "var(--" + status + ")";
}

function statusRank(node) {
  const order = { critical: 0, degraded: 1, unknown: 2, healthy: 3 };
  return order[normalizeStatus(node)] === undefined ? 2 : order[normalizeStatus(node)];
}

function sortedNodes() {
  const nodes = state.graph && Array.isArray(state.graph.nodes) ? state.graph.nodes.slice() : [];
  nodes.sort((a, b) => {
    const statusDiff = statusRank(a) - statusRank(b);
    if (statusDiff !== 0) return statusDiff;
    const healthDiff = finite(a.health_score, 1) - finite(b.health_score, 1);
    if (healthDiff !== 0) return healthDiff;
    const errorDiff = finite(b.metrics && b.metrics.error_rate, 0) - finite(a.metrics && a.metrics.error_rate, 0);
    if (errorDiff !== 0) return errorDiff;
    return String(a.id).localeCompare(String(b.id));
  });
  return nodes;
}

function currentNode() {
  if (!state.selected || !state.graph) return null;
  return state.graph.nodes.find((node) => node.id === state.selected) || null;
}

function updateURL(changes) {
  const url = new URL(window.location.href);
  if (url.pathname !== "/") url.pathname = "/";
  for (const entry of Object.entries(changes)) {
    if (entry[1] === null || entry[1] === undefined || entry[1] === "") {
      url.searchParams.delete(entry[0]);
    } else {
      url.searchParams.set(entry[0], String(entry[1]));
    }
  }
  window.history.replaceState(null, "", url.pathname + url.search);
}

function readURL() {
  const params = new URLSearchParams(window.location.search);
  state.selected = params.get("service");
  const tab = params.get("tab");
  state.activeTab = ["overview", "why", "impact", "dependencies"].includes(tab) ? tab : "overview";
  state.impactRoot = params.get("impact");
}

function setTheme(theme, persist) {
  document.documentElement.dataset.theme = theme;
  const meta = document.querySelector("meta[name=theme-color]");
  if (meta) meta.setAttribute("content", theme === "light" ? "#f4f6f9" : "#090b10");
  dom.theme.setAttribute("aria-label", theme === "light" ? "Switch to dark theme" : "Switch to light theme");
  if (persist) {
    try {
      window.localStorage.setItem("oc-theme", theme);
    } catch (_) {
      // The theme still works when storage is blocked.
    }
  }
}

function initializeTheme() {
  let stored = null;
  try {
    stored = window.localStorage.getItem("oc-theme");
  } catch (_) {
    stored = null;
  }
  const system = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  setTheme(stored === "light" || stored === "dark" ? stored : system, false);
}

async function fetchJSON(path) {
  const cached = state.apiCache.get(path);
  const headers = { Accept: "application/json" };
  if (cached && cached.etag) headers["If-None-Match"] = cached.etag;
  const response = await fetch(path, { headers: headers, cache: "no-cache" });
  if (response.status === 304 && cached) return cached.data;
  if (!response.ok) {
    const message = (await response.text()).trim();
    throw new Error(message || "HTTP " + response.status);
  }
  const data = await response.json();
  state.apiCache.set(path, { etag: response.headers.get("ETag"), data: data });
  return data;
}

function setConnection(status, label) {
  dom.connection.dataset.state = status;
  dom.connectionLabel.textContent = label;
}

function showToast(message) {
  dom.toast.textContent = message;
  dom.toast.hidden = false;
  window.clearTimeout(showToast.timer);
  showToast.timer = window.setTimeout(() => {
    dom.toast.hidden = true;
  }, 3600);
}

function connectionEndpoints() {
  return [
    { key: "mcp", label: "MCP URL", value: window.location.origin + "/mcp" },
    { key: "grpc", label: "OTLP gRPC", value: window.location.hostname + ":4317" },
    { key: "http", label: "OTLP HTTP", value: window.location.origin + "/v1/" },
  ];
}

async function copyEndpoint(button, value) {
  window.clearTimeout(button.resetTimer);
  try {
    await navigator.clipboard.writeText(value);
    button.textContent = "Copied";
  } catch (_) {
    const code = button.parentElement.querySelector("code");
    const selection = window.getSelection();
    if (code && selection) {
      const range = document.createRange();
      range.selectNodeContents(code);
      selection.removeAllRanges();
      selection.addRange(range);
      button.textContent = "Selected";
    }
  }
  button.resetTimer = window.setTimeout(() => {
    button.textContent = "Copy";
  }, 1500);
}

function endpointRow(endpoint) {
  const row = document.createElement("div");
  row.className = "endpoint-row";
  row.append(
    textElement("span", "endpoint-label", endpoint.label),
    textElement("code", "endpoint-value", endpoint.value)
  );
  const copy = textElement("button", "endpoint-copy", "Copy");
  copy.type = "button";
  copy.dataset.copyEndpoint = endpoint.key;
  copy.setAttribute("aria-label", "Copy " + endpoint.label);
  copy.addEventListener("click", () => copyEndpoint(copy, endpoint.value));
  row.appendChild(copy);
  return row;
}

function renderConnectionEndpoints() {
  for (const container of [dom.connectEndpoints, dom.emptyEndpoints]) {
    const fragment = document.createDocumentFragment();
    for (const endpoint of connectionEndpoints()) fragment.appendChild(endpointRow(endpoint));
    container.replaceChildren(fragment);
  }
}

function commandItem(label, hint, attribute, value, run) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "command-item";
  button.dataset.commandLabel = (label + " " + hint).toLowerCase();
  if (attribute) button.dataset[attribute] = value;
  button.append(textElement("strong", "", label), textElement("span", "", hint));
  button.addEventListener("click", run);
  return button;
}

function commandGroup(label, items) {
  if (!items.length) return null;
  const section = document.createElement("section");
  section.className = "command-group";
  section.setAttribute("aria-label", label);
  section.appendChild(textElement("h3", "", label));
  for (const item of items) section.appendChild(item);
  return section;
}

function commandMatches(label, hint) {
  const query = dom.commandSearch.value.trim().toLowerCase();
  return !query || (label + " " + hint).toLowerCase().includes(query);
}

function chooseCommandService(service) {
  const mode = state.commandMode;
  dom.commandDialog.close();
  openInspector(service);
  if (mode === "root-cause" || mode === "impact") {
    const tab = mode === "root-cause" ? "why" : "impact";
    const tool = mode === "root-cause" ? "root_cause_analysis" : "impact_analysis";
    selectTab(tab, false);
    runTool(tool, service);
  }
}

async function copyMCPURL() {
  const value = window.location.origin + "/mcp";
  try {
    await navigator.clipboard.writeText(value);
    showToast("MCP URL copied.");
  } catch (_) {
    showToast("Copy unavailable. MCP URL: " + value);
  }
}

function renderCommandMenu() {
  const fragment = document.createDocumentFragment();
  if (state.commandMode) {
    const label = state.commandMode === "root-cause" ? "Root-cause analysis" : "Impact analysis";
    const services = [];
    for (const node of sortedNodes()) {
      if (!commandMatches(node.id, label)) continue;
      services.push(commandItem(node.id, label, "commandService", node.id, () => chooseCommandService(node.id)));
    }
    const group = commandGroup("Choose a service for " + label.toLowerCase(), services);
    if (group) fragment.appendChild(group);
  } else {
    const actions = [
      ["Root-cause analysis", "Choose a service and open Why", "root-cause"],
      ["Impact analysis", "Choose a service and map its blast radius", "impact"],
    ].filter((item) => commandMatches(item[0], item[1])).map((item) =>
      commandItem(item[0], item[1], "commandAction", item[2], () => {
        state.commandMode = item[2];
        dom.commandSearch.value = "";
        dom.commandSearch.placeholder = "Choose a service…";
        renderCommandMenu();
        dom.commandSearch.focus();
      })
    );
    const services = [];
    for (const node of sortedNodes()) {
      if (!commandMatches(node.id, "Open service inspector")) continue;
      services.push(commandItem(node.id, "Open service inspector", "commandService", node.id, () => chooseCommandService(node.id)));
    }
    const utilities = [
      ["Toggle theme", "Switch between light and dark", "toggle-theme", () => {
        dom.commandDialog.close();
        setTheme(document.documentElement.dataset.theme === "light" ? "dark" : "light", true);
      }],
      ["Copy MCP URL", window.location.origin + "/mcp", "copy-mcp", () => {
        dom.commandDialog.close();
        copyMCPURL();
      }],
    ].filter((item) => commandMatches(item[0], item[1])).map((item) =>
      commandItem(item[0], item[1], "command", item[2], item[3])
    );
    for (const group of [commandGroup("Investigate", actions), commandGroup("Services", services), commandGroup("Utilities", utilities)]) {
      if (group) fragment.appendChild(group);
    }
  }
  if (!fragment.childNodes.length) {
    fragment.appendChild(textElement("p", "command-empty", "No matching commands or services."));
  }
  dom.commandResults.replaceChildren(fragment);
}

function openCommandMenu() {
  if (dom.shortcutDialog.open) dom.shortcutDialog.close();
  if (dom.commandDialog.open) {
    dom.commandDialog.close();
    return;
  }
  state.commandMode = null;
  dom.commandSearch.value = "";
  dom.commandSearch.placeholder = "Search commands and services…";
  renderCommandMenu();
  dom.commandDialog.showModal();
  window.setTimeout(() => dom.commandSearch.focus(), 0);
}

function openShortcutSheet() {
  if (dom.commandDialog.open) dom.commandDialog.close();
  if (!dom.shortcutDialog.open) dom.shortcutDialog.showModal();
}

async function refresh(options) {
  const silent = options && options.silent;
  if (state.refreshing) return;
  state.refreshing = true;
  dom.refresh.disabled = true;
  if (!silent && !state.graph) {
    state.loading = true;
    state.error = "";
    renderStates();
  }

  const results = await Promise.allSettled([
    fetchJSON("/api/system/graph"),
    fetchJSON("/api/metrics/dashboard"),
    fetchJSON("/api/stats"),
  ]);

  if (results[0].status === "fulfilled") {
    state.graph = normalizeGraph(results[0].value);
    state.error = "";
  } else if (!state.graph || (!silent && state.graph.nodes.length === 0)) {
    state.error = results[0].reason && results[0].reason.message ? results[0].reason.message : "Unknown response";
  } else if (!silent) {
    showToast("Refresh failed; keeping the last service graph.");
  }
  if (results[1].status === "fulfilled") state.dashboard = results[1].value;
  if (results[2].status === "fulfilled") state.stats = results[2].value;

  state.loading = false;
  state.refreshing = false;
  dom.refresh.disabled = false;
  chooseInitialMobileMode();
  renderAll();
}

function normalizeGraph(graph) {
  const value = graph && typeof graph === "object" ? graph : {};
  value.system = value.system && typeof value.system === "object" ? value.system : {};
  value.nodes = Array.isArray(value.nodes) ? value.nodes.filter((node) => node && typeof node.id === "string") : [];
  value.edges = Array.isArray(value.edges) ? value.edges.filter((edge) => edge && typeof edge.source === "string" && typeof edge.target === "string") : [];
  for (const node of value.nodes) {
    node.metrics = node.metrics && typeof node.metrics === "object" ? node.metrics : {};
    node.alerts = Array.isArray(node.alerts) ? node.alerts : [];
  }
  return value;
}

function renderStates() {
  const nodeCount = state.graph && state.graph.nodes ? state.graph.nodes.length : 0;
  dom.loading.hidden = !state.loading;
  dom.error.hidden = state.loading || !state.error;
  dom.empty.hidden = state.loading || Boolean(state.error) || nodeCount > 0;
  dom.canvas.hidden = state.loading || Boolean(state.error) || nodeCount === 0 || (isMobile() && state.mobileMode === "list");
  dom.mobileList.hidden = state.loading || Boolean(state.error) || nodeCount === 0 || !isMobile() || state.mobileMode !== "list";
  dom.errorMessage.textContent = state.error;
}

function renderPulse() {
  const summary = state.graph && state.graph.system ? state.graph.system : {};
  const dashboard = state.dashboard || {};
  const stats = state.stats || {};
  dom.pulseHealth.textContent = formatRatio(summary.overall_health_score, 0);
  dom.pulseHealth.style.color = statusColor(normalizeStatus({
    status: summary.critical > 0 ? "critical" : summary.degraded > 0 ? "degraded" : "healthy",
    health_score: summary.overall_health_score,
  }));
  dom.pulseErrors.textContent = Number.isFinite(Number(dashboard.error_rate))
    ? formatPercent(dashboard.error_rate, 1)
    : formatRatio(summary.total_error_rate, 1);
  dom.pulseP99.textContent = Number.isFinite(Number(dashboard.p99_latency_ms))
    ? formatMs(dashboard.p99_latency_ms)
    : formatMs(summary.avg_latency_ms);
  dom.pulseServices.textContent = formatCount(summary.total_services);
  const uptime = Number(summary.uptime_seconds);
  if (Number.isFinite(uptime) && uptime !== state.uptimeAuthoritative) {
    state.uptimeAuthoritative = uptime;
    state.uptimeBase = uptime;
    state.uptimeObservedAt = Date.now();
  }
  renderUptime();
  const dbSize = stats.DBSizeMB !== undefined ? stats.DBSizeMB : stats.db_size_mb;
  dom.pulseDB.textContent = formatMB(dbSize);
  if (dashboard.coverage) {
    dom.coverage.hidden = false;
    dom.coverage.textContent = dashboard.coverage + " coverage";
    dom.coverage.title = dashboard.coverage_note || "";
  } else {
    dom.coverage.hidden = true;
  }
}

function renderUptime() {
  if (!Number.isFinite(state.uptimeBase)) {
    dom.pulseUptime.textContent = "—";
    return;
  }
  const seconds = Math.max(0, Math.floor(state.uptimeBase + (Date.now() - state.uptimeObservedAt) / 1000));
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor(seconds % 86400 / 3600);
  const minutes = Math.floor(seconds % 3600 / 60);
  const remainder = seconds % 60;
  dom.pulseUptime.textContent = days + "d "
    + String(hours).padStart(2, "0") + ":"
    + String(minutes).padStart(2, "0") + ":"
    + String(remainder).padStart(2, "0");
  dom.pulseUptime.setAttribute("aria-label", "Process uptime " + dom.pulseUptime.textContent);
}

function summaryItem(status, value, label) {
  const item = document.createElement("div");
  item.className = "summary-" + status;
  item.append(textElement("strong", "", String(value)), textElement("span", "", label));
  return item;
}

function renderSeverity() {
  const counts = { healthy: 0, degraded: 0, critical: 0 };
  for (const node of state.graph ? state.graph.nodes : []) {
    const status = normalizeStatus(node);
    if (counts[status] !== undefined) counts[status] += 1;
  }
  dom.severity.replaceChildren(
    summaryItem("critical", counts.critical, "Critical"),
    summaryItem("degraded", counts.degraded, "Degraded"),
    summaryItem("healthy", counts.healthy, "Healthy")
  );
}

function serviceButton(node) {
  const status = normalizeStatus(node);
  const button = document.createElement("button");
  button.type = "button";
  button.className = "service-row";
  button.dataset.service = node.id;
  button.setAttribute("aria-current", node.id === state.selected ? "true" : "false");
  button.setAttribute("aria-label", node.id + ", " + status + ", health " + formatRatio(node.health_score, 0));

  const dot = document.createElement("span");
  dot.className = "status-dot " + status;
  dot.setAttribute("aria-hidden", "true");
  const main = document.createElement("span");
  main.className = "service-row-main";
  main.appendChild(textElement("span", "service-name", node.id));
  const metrics = node.metrics || {};
  let detail = formatMs(metrics.p99_latency_ms) + " p99 · " + formatRatio(metrics.error_rate, 1) + " error";
  if (state.anomalies.has(node.id)) detail += " · anomaly";
  main.appendChild(textElement("span", "service-meta", detail));
  const health = textElement("span", "service-health", formatRatio(node.health_score, 0));
  health.style.color = statusColor(status);
  button.append(dot, main, health);
  button.addEventListener("click", () => openInspector(node.id));
  return button;
}

function filteredSortedNodes() {
  const query = state.query.trim().toLowerCase();
  return sortedNodes().filter((node) => !query || node.id.toLowerCase().includes(query));
}

function renderServiceLists() {
  const all = sortedNodes();
  const filtered = filteredSortedNodes();
  const railFragment = document.createDocumentFragment();
  const mobileFragment = document.createDocumentFragment();
  for (const node of filtered) {
    railFragment.appendChild(serviceButton(node));
    mobileFragment.appendChild(serviceButton(node));
  }
  if (filtered.length === 0 && all.length > 0) {
    railFragment.appendChild(textElement("p", "quiet", "No services match this search."));
    mobileFragment.appendChild(textElement("p", "quiet", "No services match this search."));
  }
  dom.serviceList.replaceChildren(railFragment);
  dom.mobileList.replaceChildren(mobileFragment);
  dom.serviceCount.textContent = String(all.length);
  dom.searchCount.textContent = state.query ? String(filtered.length) + " found" : "";
}

function cleanEdges(nodes, edges) {
  const ids = new Set(nodes.map((node) => node.id));
  const seen = new Set();
  const output = [];
  for (const edge of edges) {
    if (!ids.has(edge.source) || !ids.has(edge.target) || edge.source === edge.target) continue;
    const key = edge.source + ">" + edge.target;
    if (seen.has(key)) continue;
    seen.add(key);
    output.push(edge);
  }
  output.sort((a, b) => (a.source + ">" + a.target).localeCompare(b.source + ">" + b.target));
  return output;
}

function graphLayout(nodes, edges) {
  const ids = nodes.map((node) => node.id).sort((a, b) => a.localeCompare(b));
  const callers = new Map();
  const degree = new Map();
  for (const id of ids) {
    callers.set(id, new Set());
    degree.set(id, 0);
  }
  for (const edge of edges) {
    callers.get(edge.target).add(edge.source);
    degree.set(edge.source, degree.get(edge.source) + 1);
    degree.set(edge.target, degree.get(edge.target) + 1);
  }
  let maxCallers = 0;
  let maxLogDegree = 0;
  for (const id of ids) {
    maxCallers = Math.max(maxCallers, callers.get(id).size);
    maxLogDegree = Math.max(maxLogDegree, Math.log1p(degree.get(id)));
  }
  const scores = new Map();
  for (const id of ids) {
    const callerScore = maxCallers ? callers.get(id).size / maxCallers : 0;
    const volumeScore = maxLogDegree ? Math.log1p(degree.get(id)) / maxLogDegree : 0;
    scores.set(id, 0.6 * callerScore + 0.4 * volumeScore);
  }
  ids.sort((a, b) => scores.get(b) - scores.get(a) || a.localeCompare(b));

  const positions = new Map();
  const count = Math.max(ids.length, 1);
  const radius = ids.length === 1 ? 0 : 382;
  ids.forEach((id, index) => {
    const distance = radius * Math.sqrt((index + 0.4) / count);
    const angle = index * GOLDEN_ANGLE;
    positions.set(id, {
      x: 500 + distance * Math.cos(angle),
      y: 500 + distance * Math.sin(angle),
    });
  });
  return positions;
}

function downstreamDepths(root, edges, maxDepth) {
  const outgoing = new Map();
  for (const edge of edges) {
    if (!outgoing.has(edge.source)) outgoing.set(edge.source, []);
    outgoing.get(edge.source).push(edge.target);
  }
  const depths = new Map([[root, 0]]);
  let frontier = [root];
  for (let depth = 1; depth <= maxDepth && frontier.length; depth += 1) {
    const next = [];
    for (const source of frontier) {
      for (const target of outgoing.get(source) || []) {
        if (depths.has(target)) continue;
        depths.set(target, depth);
        next.push(target);
      }
    }
    frontier = next;
  }
  return depths;
}

function graphSearchMatches() {
  const query = state.query.trim().toLowerCase();
  if (!query || !state.graph) return null;
  return new Set(state.graph.nodes.filter((node) => node.id.toLowerCase().includes(query)).map((node) => node.id));
}

function renderGraph() {
  dom.rings.replaceChildren();
  dom.edges.replaceChildren();
  dom.nodes.replaceChildren();
  dom.minimapEdges.replaceChildren();
  dom.minimapNodes.replaceChildren();
  if (!state.graph || state.graph.nodes.length === 0) return;

  const edges = cleanEdges(state.graph.nodes, state.graph.edges);
  const positions = graphLayout(state.graph.nodes, edges);
  const searchMatches = graphSearchMatches();
  const impact = state.impactRoot ? downstreamDepths(state.impactRoot, edges, 5) : null;
  const selectedNeighbors = new Set();
  if (state.selected) {
    selectedNeighbors.add(state.selected);
    for (const edge of edges) {
      if (edge.source === state.selected) selectedNeighbors.add(edge.target);
      if (edge.target === state.selected) selectedNeighbors.add(edge.source);
    }
  }

  for (const radius of [100, 200, 300, 400]) {
    dom.rings.appendChild(svgElement("circle", { class: "graph-ring", cx: 500, cy: 500, r: radius }));
  }

  const edgeFragment = document.createDocumentFragment();
  for (const edge of edges) {
    const source = positions.get(edge.source);
    const target = positions.get(edge.target);
    if (!source || !target) continue;
    const line = svgElement("line", {
      class: "graph-edge",
      "data-source": edge.source,
      "data-target": edge.target,
      x1: source.x,
      y1: source.y,
      x2: target.x,
      y2: target.y,
    });
    const related = state.selected && (edge.source === state.selected || edge.target === state.selected);
    const inImpact = impact && impact.has(edge.source) && impact.has(edge.target)
      && impact.get(edge.target) === impact.get(edge.source) + 1;
    if (related) line.classList.add("is-related");
    if (inImpact) line.classList.add("is-impact");
    if ((related || inImpact) && edges.length < 600) line.setAttribute("marker-end", "url(#edge-arrow)");
    if (searchMatches && !searchMatches.has(edge.source) && !searchMatches.has(edge.target)) {
      line.style.opacity = "0.1";
    } else if (state.selected && !related) {
      line.style.opacity = "0.12";
    } else if (impact && !inImpact) {
      line.style.opacity = "0.08";
    }
    edgeFragment.appendChild(line);
    dom.minimapEdges.appendChild(svgElement("line", {
      class: "minimap-edge",
      x1: source.x,
      y1: source.y,
      x2: target.x,
      y2: target.y,
    }));
  }
  dom.edges.appendChild(edgeFragment);

  const count = state.graph.nodes.length;
  const nodeRadius = count > 140 ? 7 : count > 70 ? 9 : count > 30 ? 11 : 14;
  const showAllLabels = count <= 42;
  const nodeFragment = document.createDocumentFragment();
  for (const node of state.graph.nodes) {
    const point = positions.get(node.id);
    if (!point) continue;
    const status = normalizeStatus(node);
    const group = svgElement("g", {
      class: "service-node",
      transform: "translate(" + point.x + " " + point.y + ")",
      tabindex: "0",
      role: "button",
      "data-status": status,
      "aria-label": node.id + ", " + status + ", health " + formatRatio(node.health_score, 0),
    });
    group.dataset.service = node.id;
    if (node.id === state.selected) group.classList.add("is-selected");
    if (searchMatches && searchMatches.has(node.id)) group.classList.add("is-search-match");
    if (impact && impact.has(node.id)) group.classList.add("is-impact");
    const dimForSearch = searchMatches && !searchMatches.has(node.id);
    const dimForSelection = state.selected && !selectedNeighbors.has(node.id);
    const dimForImpact = impact && !impact.has(node.id);
    if (dimForSearch || dimForSelection || dimForImpact) group.classList.add("is-dim");

    group.appendChild(svgElement("circle", { class: "node-halo", r: nodeRadius + 8 }));
    group.appendChild(svgElement("circle", { class: "node-core", r: nodeRadius }));
    const showLabel = showAllLabels || node.id === state.selected || (searchMatches && searchMatches.has(node.id));
    if (showLabel) {
      const label = svgElement("text", {
        class: "node-label",
        x: nodeRadius + 9,
        y: 6,
      });
      label.textContent = node.id.length > 24 ? node.id.slice(0, 23) + "…" : node.id;
      group.appendChild(label);
    }
    const title = svgElement("title");
    title.textContent = node.id + " — " + status + ", p99 " + formatMs(node.metrics.p99_latency_ms);
    group.appendChild(title);
    group.addEventListener("click", (event) => {
      event.stopPropagation();
      openInspector(node.id);
    });
    group.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        openInspector(node.id);
      }
    });
    nodeFragment.appendChild(group);
    dom.minimapNodes.appendChild(svgElement("circle", {
      class: "minimap-node",
      cx: point.x,
      cy: point.y,
      r: count > 70 ? 16 : 22,
      "data-status": status,
    }));
  }
  dom.nodes.appendChild(nodeFragment);
  renderMinimapViewport();
}

function renderImpactBanner() {
  if (!state.impactRoot || !state.graph) {
    dom.impactBanner.hidden = true;
    return;
  }
  const depths = downstreamDepths(state.impactRoot, state.graph.edges, 5);
  dom.impactBanner.hidden = false;
  dom.impactLabel.replaceChildren();
  dom.impactLabel.append("Blast radius of ");
  dom.impactLabel.appendChild(textElement("strong", "", state.impactRoot));
  dom.impactLabel.append(" — " + Math.max(0, depths.size - 1) + " downstream");
}

function renderViewSwitch() {
  const mapMode = state.mobileMode === "map";
  dom.mapView.setAttribute("aria-pressed", mapMode ? "true" : "false");
  dom.listView.setAttribute("aria-pressed", mapMode ? "false" : "true");
}

function isMobile() {
  return window.matchMedia("(max-width: 767px)").matches;
}

function chooseInitialMobileMode() {
  if (state.mobileModeChosen || !state.graph) return;
  const requested = new URLSearchParams(window.location.search).get("flow");
  if (requested === "1") state.mobileMode = "map";
  else if (requested === "0") state.mobileMode = "list";
  else state.mobileMode = state.graph.nodes.length > 40 ? "list" : "map";
  state.mobileModeChosen = true;
}

function setMobileMode(mode) {
  state.mobileMode = mode;
  state.mobileModeChosen = true;
  updateURL({ flow: mode === "map" ? "1" : "0" });
  renderViewSwitch();
  renderStates();
}

function statCard(label, value, critical) {
  const card = document.createElement("div");
  card.className = "stat-card" + (critical ? " is-critical" : "");
  card.append(textElement("span", "stat-label", label), textElement("strong", "", value));
  return card;
}

function renderOverview(node) {
  const wrapper = document.createElement("div");
  const metrics = node.metrics || {};
  const stats = document.createElement("div");
  stats.className = "stat-grid";
  stats.append(
    statCard("RPS", formatCount(metrics.request_rate_rps) + "/s"),
    statCard("Error", formatRatio(metrics.error_rate, 1), finite(metrics.error_rate, 0) > 0),
    statCard("Average", formatMs(metrics.avg_latency_ms)),
    statCard("P99", formatMs(metrics.p99_latency_ms))
  );
  wrapper.appendChild(stats);

  const meter = document.createElement("div");
  meter.className = "health-meter";
  meter.appendChild(textElement("span", "stat-label", "Health"));
  const track = document.createElement("div");
  track.className = "meter-track";
  track.setAttribute("role", "meter");
  track.setAttribute("aria-label", "Health score");
  track.setAttribute("aria-valuemin", "0");
  track.setAttribute("aria-valuemax", "100");
  track.setAttribute("aria-valuenow", String(Math.round(finite(node.health_score, 0) * 100)));
  const fill = document.createElement("div");
  fill.className = "meter-fill";
  fill.style.width = Math.max(0, Math.min(100, finite(node.health_score, 0) * 100)) + "%";
  fill.style.background = statusColor(normalizeStatus(node));
  track.appendChild(fill);
  const health = textElement("strong", "", formatRatio(node.health_score, 0));
  health.style.color = statusColor(normalizeStatus(node));
  meter.append(track, health);
  wrapper.appendChild(meter);

  const section = document.createElement("section");
  section.className = "section";
  section.appendChild(textElement("h3", "section-title", "Alerts"));
  if (!node.alerts.length) {
    section.appendChild(textElement("p", "quiet", "No active alerts."));
  } else {
    const list = document.createElement("ul");
    list.className = "alert-list";
    for (const alert of node.alerts) list.appendChild(textElement("li", "", String(alert)));
    section.appendChild(list);
  }
  wrapper.appendChild(section);
  return wrapper;
}

function dependencyRow(id, edge) {
  const item = document.createElement("li");
  const button = document.createElement("button");
  button.type = "button";
  button.className = "dependency-row";
  button.append(
    textElement("strong", "", id),
    textElement("span", "", formatCount(edge.call_count) + " calls · " + formatRatio(edge.error_rate, 1) + " error")
  );
  button.addEventListener("click", () => openInspector(id));
  item.appendChild(button);
  return item;
}

function dependencySection(title, entries) {
  const section = document.createElement("section");
  section.className = "section";
  section.appendChild(textElement("h3", "section-title", title));
  if (!entries.length) {
    section.appendChild(textElement("p", "quiet", "None in the current graph."));
    return section;
  }
  const list = document.createElement("ul");
  list.className = "dependency-list";
  for (const entry of entries) list.appendChild(dependencyRow(entry.id, entry.edge));
  section.appendChild(list);
  return section;
}

function renderDependencies(node) {
  const wrapper = document.createElement("div");
  const edges = state.graph ? state.graph.edges : [];
  const upstream = edges.filter((edge) => edge.target === node.id).map((edge) => ({ id: edge.source, edge: edge }));
  const downstream = edges.filter((edge) => edge.source === node.id).map((edge) => ({ id: edge.target, edge: edge }));
  wrapper.append(
    dependencySection("Upstream callers", upstream),
    dependencySection("Downstream dependencies", downstream)
  );
  return wrapper;
}

function toolKey(tool, service) {
  return tool + ":" + service;
}

function toolEntry(tool, service) {
  const value = state.tools.get(toolKey(tool, service));
  if (!value) return null;
  if (value.status === "success" && Date.now() - value.completedAt > TOOL_CACHE_MS) return null;
  return value;
}

async function callMCPTool(name, args, signal) {
  const response = await fetch("/mcp", {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: Date.now(),
      method: "tools/call",
      params: { name: name, arguments: args },
    }),
    signal: signal,
  });
  if (!response.ok) throw new Error("MCP endpoint returned HTTP " + response.status);
  const envelope = await response.json();
  if (envelope.error) throw new Error(envelope.error.message || "MCP request failed");
  const result = envelope.result || {};
  const content = Array.isArray(result.content) ? result.content : [];
  const block = content.find((item) => item && (typeof item.text === "string" || item.resource && typeof item.resource.text === "string"));
  const raw = block ? (typeof block.text === "string" ? block.text : block.resource.text) : "";
  if (result.isError) throw new Error(raw || name + " failed");
  if (!raw) return null;
  try {
    return JSON.parse(raw);
  } catch (_) {
    throw new Error("The analysis returned an unreadable response.");
  }
}

async function runTool(tool, service) {
  const key = toolKey(tool, service);
  const existing = state.tools.get(key);
  if (existing && existing.controller) existing.controller.abort();
  const controller = new AbortController();
  state.tools.set(key, { status: "loading", controller: controller, data: null, error: "" });
  renderInspector();
  try {
    const data = await callMCPTool(tool, { service: service }, controller.signal);
    state.tools.set(key, {
      status: "success",
      data: data,
      error: "",
      controller: null,
      completedAt: Date.now(),
    });
  } catch (error) {
    if (error && error.name === "AbortError") {
      state.tools.delete(key);
    } else {
      state.tools.set(key, {
        status: "error",
        data: null,
        error: error && error.message ? error.message : "Analysis failed.",
        controller: null,
      });
    }
  }
  if (state.selected === service) renderInspector();
}

function cancelTool(tool, service) {
  const key = toolKey(tool, service);
  const entry = state.tools.get(key);
  if (entry && entry.controller) entry.controller.abort();
  state.tools.delete(key);
  renderInspector();
}

function verbFrame(hint, buttonLabel, run) {
  const frame = document.createElement("div");
  frame.className = "verb-frame";
  frame.appendChild(textElement("p", "verb-hint", hint));
  const button = textElement("button", "action-button", buttonLabel);
  button.type = "button";
  button.addEventListener("click", run);
  frame.appendChild(button);
  return frame;
}

function loadingTool(tool, service) {
  const frame = document.createElement("div");
  frame.className = "verb-frame";
  const panel = document.createElement("div");
  panel.className = "tool-state";
  panel.append(textElement("span", "spinner", ""), document.createTextNode(" Running analysis…"));
  const cancel = textElement("button", "secondary-button", "Cancel");
  cancel.type = "button";
  cancel.addEventListener("click", () => cancelTool(tool, service));
  frame.append(panel, cancel);
  return frame;
}

function errorTool(tool, service, message, label) {
  const frame = document.createElement("div");
  frame.className = "verb-frame";
  frame.appendChild(textElement("p", "tool-state", message));
  const retry = textElement("button", "action-button", label);
  retry.type = "button";
  retry.addEventListener("click", () => runTool(tool, service));
  frame.appendChild(retry);
  return frame;
}

function renderWhy(node) {
  const entry = toolEntry("root_cause_analysis", node.id);
  if (!entry) {
    return verbFrame(
      "Trace error chains upstream from " + node.id + " and rank probable causes with evidence.",
      "Run root-cause analysis",
      () => runTool("root_cause_analysis", node.id)
    );
  }
  if (entry.status === "loading") return loadingTool("root_cause_analysis", node.id);
  if (entry.status === "error") return errorTool("root_cause_analysis", node.id, entry.error, "Try again");

  const frame = document.createElement("div");
  const causes = Array.isArray(entry.data) ? entry.data.slice() : [];
  causes.sort((a, b) => finite(b.score, 0) - finite(a.score, 0));
  if (!causes.length) {
    frame.appendChild(textElement("p", "quiet", "No probable causes were found in the current window."));
  } else {
    const list = document.createElement("ol");
    list.className = "cause-list";
    const maxScore = Math.max.apply(null, causes.map((cause) => finite(cause.score, 0)).concat([0]));
    causes.forEach((cause, index) => {
      const card = document.createElement("li");
      card.className = "cause-card";
      const head = document.createElement("div");
      head.className = "cause-head";
      head.append(
        textElement("span", "cause-rank", "#" + (index + 1)),
        textElement("span", "cause-service", String(cause.service || "Unknown") + (cause.operation ? " · " + cause.operation : "")),
        textElement("span", "cause-score", trimFixed(finite(cause.score, 0), 1))
      );
      const track = document.createElement("div");
      track.className = "score-track";
      const fill = document.createElement("div");
      fill.className = "score-fill";
      fill.style.width = (maxScore > 0 ? Math.min(100, finite(cause.score, 0) / maxScore * 100) : 0) + "%";
      track.appendChild(fill);
      card.append(head, track);
      if (Array.isArray(cause.evidence) && cause.evidence.length) {
        const evidence = document.createElement("ul");
        evidence.className = "evidence-list";
        for (const line of cause.evidence) evidence.appendChild(textElement("li", "", String(line)));
        card.appendChild(evidence);
      }
      list.appendChild(card);
    });
    frame.appendChild(list);
  }
  const rerun = textElement("button", "secondary-button", "Re-run");
  rerun.type = "button";
  rerun.addEventListener("click", () => runTool("root_cause_analysis", node.id));
  frame.appendChild(rerun);
  return frame;
}

function showImpactOnMap(service) {
  state.impactRoot = service;
  state.mobileMode = "map";
  state.mobileModeChosen = true;
  updateURL({ impact: service, service: null, tab: null, flow: "1" });
  closeInspector();
  renderAll();
}

function renderImpact(node) {
  const entry = toolEntry("impact_analysis", node.id);
  if (!entry) {
    return verbFrame(
      "Walk the call graph downstream from " + node.id + " to find every service a failure here can reach.",
      "Map blast radius",
      () => runTool("impact_analysis", node.id)
    );
  }
  if (entry.status === "loading") return loadingTool("impact_analysis", node.id);
  if (entry.status === "error") return errorTool("impact_analysis", node.id, entry.error, "Try again");

  const frame = document.createElement("div");
  const affected = entry.data && Array.isArray(entry.data.affected_services) ? entry.data.affected_services.slice() : [];
  const header = document.createElement("div");
  header.className = "impact-result-header";
  header.appendChild(textElement("p", "quiet", affected.length + " downstream service" + (affected.length === 1 ? "" : "s") + " affected"));
  const show = textElement("button", "secondary-button", "Show on map");
  show.type = "button";
  show.addEventListener("click", () => showImpactOnMap(node.id));
  header.appendChild(show);
  frame.appendChild(header);
  if (!affected.length) {
    frame.appendChild(textElement("p", "quiet", "No downstream services. A failure here stays contained."));
  } else {
    affected.sort((a, b) => finite(a.depth, 0) - finite(b.depth, 0) || String(a.service).localeCompare(String(b.service)));
    const list = document.createElement("ul");
    list.className = "affected-list";
    for (const entryValue of affected) {
      const item = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.className = "dependency-row";
      button.append(
        textElement("strong", "", String(entryValue.service)),
        textElement("span", "", "depth " + formatCount(entryValue.depth) + " · impact " + formatRatio(entryValue.impact_score, 0))
      );
      button.addEventListener("click", () => openInspector(String(entryValue.service)));
      item.appendChild(button);
      list.appendChild(item);
    }
    frame.appendChild(list);
  }
  const rerun = textElement("button", "secondary-button", "Re-run");
  rerun.type = "button";
  rerun.addEventListener("click", () => runTool("impact_analysis", node.id));
  frame.appendChild(rerun);
  return frame;
}

function renderInspector() {
  const node = currentNode();
  if (!state.selected) {
    dom.inspector.setAttribute("aria-hidden", "true");
    dom.inspector.inert = true;
    dom.inspectorScrim.hidden = true;
    return;
  }
  dom.inspector.setAttribute("aria-hidden", "false");
  dom.inspector.inert = false;
  dom.inspectorScrim.hidden = !isMobile();
  dom.inspectorTitle.textContent = state.selected;
  dom.inspectorStatus.className = "service-status " + (node ? normalizeStatus(node) : "unknown");
  dom.inspectorHealth.textContent = node ? formatRatio(node.health_score, 0) : "Not reporting";
  dom.inspectorHealth.style.color = node ? statusColor(normalizeStatus(node)) : "var(--faint)";
  for (const tab of dom.tabs) {
    const active = tab.dataset.tab === state.activeTab;
    tab.setAttribute("aria-selected", active ? "true" : "false");
    tab.tabIndex = active ? 0 : -1;
  }

  if (!node) {
    const missing = document.createElement("div");
    missing.append(
      textElement("p", "state-title", "Service not found"),
      textElement("p", "quiet", state.selected + " is not in the current graph. It may have stopped reporting.")
    );
    dom.inspectorBody.replaceChildren(missing);
    return;
  }

  let content;
  if (state.activeTab === "why") content = renderWhy(node);
  else if (state.activeTab === "impact") content = renderImpact(node);
  else if (state.activeTab === "dependencies") content = renderDependencies(node);
  else content = renderOverview(node);
  dom.inspectorBody.replaceChildren(content);
}

function openInspector(service) {
  state.selected = service;
  updateURL({ service: service, tab: state.activeTab === "overview" ? null : state.activeTab });
  renderGraph();
  renderServiceLists();
  renderInspector();
  window.setTimeout(() => dom.closeInspector.focus(), 0);
}

function closeInspector() {
  const selected = state.selected;
  state.selected = null;
  updateURL({ service: null, tab: null });
  renderGraph();
  renderServiceLists();
  renderInspector();
  if (selected) {
    const source = document.querySelector("[data-service=" + CSS.escape(selected) + "]");
    if (source) source.focus();
  }
}

function selectTab(tabName, focus) {
  state.activeTab = tabName;
  updateURL({ tab: tabName === "overview" ? null : tabName });
  renderInspector();
  if (focus) {
    const tab = dom.tabs.find((item) => item.dataset.tab === tabName);
    if (tab) tab.focus();
  }
}

function renderAll() {
  renderStates();
  renderPulse();
  renderSeverity();
  renderServiceLists();
  renderGraph();
  renderImpactBanner();
  renderViewSwitch();
  renderInspector();
}

function setViewBox(next) {
  const minSize = 260;
  const maxSize = 1400;
  const width = Math.max(minSize, Math.min(maxSize, next.width));
  const height = Math.max(minSize, Math.min(maxSize, next.height));
  state.viewBox = { x: next.x, y: next.y, width: width, height: height };
  dom.map.setAttribute("viewBox", [state.viewBox.x, state.viewBox.y, width, height].join(" "));
  renderMinimapViewport();
}

function renderMinimapViewport() {
  dom.minimapViewport.setAttribute("x", String(state.viewBox.x));
  dom.minimapViewport.setAttribute("y", String(state.viewBox.y));
  dom.minimapViewport.setAttribute("width", String(state.viewBox.width));
  dom.minimapViewport.setAttribute("height", String(state.viewBox.height));
}

function zoomAt(factor, clientX, clientY) {
  const rect = dom.map.getBoundingClientRect();
  if (!rect.width || !rect.height) return;
  const view = state.viewBox;
  const px = clientX === undefined ? rect.left + rect.width / 2 : clientX;
  const py = clientY === undefined ? rect.top + rect.height / 2 : clientY;
  const focusX = view.x + (px - rect.left) / rect.width * view.width;
  const focusY = view.y + (py - rect.top) / rect.height * view.height;
  const width = Math.max(260, Math.min(1400, view.width * factor));
  const height = Math.max(260, Math.min(1400, view.height * factor));
  const ratioX = (focusX - view.x) / view.width;
  const ratioY = (focusY - view.y) / view.height;
  setViewBox({
    x: focusX - ratioX * width,
    y: focusY - ratioY * height,
    width: width,
    height: height,
  });
}

let pointerPan = null;
dom.map.addEventListener("pointerdown", (event) => {
  if (event.button !== 0 || event.target.closest(".service-node")) return;
  pointerPan = {
    id: event.pointerId,
    x: event.clientX,
    y: event.clientY,
    view: Object.assign({}, state.viewBox),
  };
  dom.map.setPointerCapture(event.pointerId);
});
dom.map.addEventListener("pointermove", (event) => {
  if (!pointerPan || pointerPan.id !== event.pointerId) return;
  const rect = dom.map.getBoundingClientRect();
  setViewBox({
    x: pointerPan.view.x - (event.clientX - pointerPan.x) / rect.width * pointerPan.view.width,
    y: pointerPan.view.y - (event.clientY - pointerPan.y) / rect.height * pointerPan.view.height,
    width: pointerPan.view.width,
    height: pointerPan.view.height,
  });
});
dom.map.addEventListener("pointerup", (event) => {
  if (pointerPan && pointerPan.id === event.pointerId) pointerPan = null;
});
dom.map.addEventListener("pointercancel", () => {
  pointerPan = null;
});
dom.map.addEventListener("wheel", (event) => {
  event.preventDefault();
  zoomAt(event.deltaY > 0 ? 1.12 : 0.89, event.clientX, event.clientY);
}, { passive: false });
dom.map.addEventListener("click", (event) => {
  if (event.target === dom.map || event.target.id === "graph-viewport" || event.target.id === "graph-rings") {
    closeInspector();
  }
});

let ws = null;
let wsRetry = 0;
let wsReconnectTimer = null;
let wsHeartbeat = null;
let wsWatchdog = null;
let wsRefreshTimer = null;

function clearWebSocketTimers() {
  window.clearInterval(wsHeartbeat);
  window.clearTimeout(wsWatchdog);
  wsHeartbeat = null;
  wsWatchdog = null;
}

function scheduleWebSocketRefresh() {
  if (document.visibilityState !== "visible" || wsRefreshTimer) return;
  wsRefreshTimer = window.setTimeout(() => {
    wsRefreshTimer = null;
    refresh({ silent: true });
  }, 1200);
}

function connectWebSocket() {
  window.clearTimeout(wsReconnectTimer);
  if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) return;
  setConnection(wsRetry ? "reconnecting" : "connecting", wsRetry ? "Reconnecting" : "Connecting");
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  try {
    ws = new WebSocket(protocol + "//" + window.location.host + "/ws");
  } catch (_) {
    scheduleWebSocketReconnect();
    return;
  }
  ws.addEventListener("open", () => {
    wsRetry = 0;
    setConnection("connected", "Live");
    clearWebSocketTimers();
    wsHeartbeat = window.setInterval(() => {
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      ws.send(JSON.stringify({ type: "ping" }));
      if (!wsWatchdog) {
        wsWatchdog = window.setTimeout(() => {
          wsWatchdog = null;
          if (ws) ws.close();
        }, 35000);
      }
    }, 30000);
  });
  ws.addEventListener("message", () => {
    window.clearTimeout(wsWatchdog);
    wsWatchdog = null;
    scheduleWebSocketRefresh();
  });
  ws.addEventListener("close", () => {
    clearWebSocketTimers();
    ws = null;
    setConnection("disconnected", "Offline");
    scheduleWebSocketReconnect();
  });
  ws.addEventListener("error", () => {
    // close owns recovery.
  });
}

function scheduleWebSocketReconnect() {
  const delay = Math.min(100 * Math.pow(2, wsRetry), 10000);
  wsRetry += 1;
  setConnection("reconnecting", "Reconnecting");
  wsReconnectTimer = window.setTimeout(connectWebSocket, delay + Math.random() * delay * 0.2);
}

async function refreshAnomalies() {
  try {
    const since = new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString();
    const payload = await callMCPTool("get_anomaly_timeline", { since: since });
    const anomalies = Array.isArray(payload) ? payload : [];
    state.anomalies = new Set(anomalies.map((item) => item && item.service).filter(Boolean));
    renderServiceLists();
  } catch (_) {
    // MCP is optional. The core topology remains useful without it.
  }
}

function isEditableTarget(target) {
  return Boolean(target && target.closest && target.closest("input, textarea, select, [contenteditable=true]"));
}

function bindEvents() {
  dom.commandButton.addEventListener("click", openCommandMenu);
  dom.closeCommand.addEventListener("click", () => dom.commandDialog.close());
  dom.closeShortcuts.addEventListener("click", () => dom.shortcutDialog.close());
  dom.commandSearch.addEventListener("input", renderCommandMenu);
  dom.commandDialog.addEventListener("close", () => {
    state.commandMode = null;
    dom.commandSearch.value = "";
    dom.commandSearch.placeholder = "Search commands and services…";
  });
  dom.commandDialog.addEventListener("cancel", (event) => {
    if (!state.commandMode) return;
    event.preventDefault();
    state.commandMode = null;
    dom.commandSearch.value = "";
    dom.commandSearch.placeholder = "Search commands and services…";
    renderCommandMenu();
    dom.commandSearch.focus();
  });
  for (const dialog of [dom.commandDialog, dom.shortcutDialog]) {
    dialog.addEventListener("click", (event) => {
      if (event.target === dialog) dialog.close();
    });
  }
  dom.commandDialog.addEventListener("keydown", (event) => {
    const items = Array.from(dom.commandResults.querySelectorAll(".command-item"));
    if (!items.length) return;
    if (event.key === "Enter" && event.target === dom.commandSearch) {
      event.preventDefault();
      items[0].click();
      return;
    }
    if (!["ArrowDown", "ArrowUp"].includes(event.key)) return;
    event.preventDefault();
    const current = items.indexOf(document.activeElement);
    const next = event.key === "ArrowDown"
      ? (current + 1) % items.length
      : (current <= 0 ? items.length - 1 : current - 1);
    items[next].focus();
  });
  dom.refresh.addEventListener("click", () => refresh({ silent: false }));
  dom.retry.addEventListener("click", () => refresh({ silent: false }));
  dom.theme.addEventListener("click", () => {
    setTheme(document.documentElement.dataset.theme === "light" ? "dark" : "light", true);
  });
  dom.search.addEventListener("input", () => {
    state.query = dom.search.value;
    renderServiceLists();
    renderGraph();
  });
  dom.mapView.addEventListener("click", () => setMobileMode("map"));
  dom.listView.addEventListener("click", () => setMobileMode("list"));
  dom.zoomIn.addEventListener("click", () => zoomAt(0.8));
  dom.zoomOut.addEventListener("click", () => zoomAt(1.25));
  dom.fit.addEventListener("click", () => setViewBox({ x: 0, y: 0, width: 1000, height: 1000 }));
  dom.minimapButton.addEventListener("click", (event) => {
    if (!event.detail) {
      setViewBox({ x: 0, y: 0, width: 1000, height: 1000 });
      return;
    }
    const rect = dom.minimap.getBoundingClientRect();
    const centerX = (event.clientX - rect.left) / rect.width * 1000;
    const centerY = (event.clientY - rect.top) / rect.height * 1000;
    setViewBox({
      x: centerX - state.viewBox.width / 2,
      y: centerY - state.viewBox.height / 2,
      width: state.viewBox.width,
      height: state.viewBox.height,
    });
  });
  dom.clearImpact.addEventListener("click", () => {
    state.impactRoot = null;
    updateURL({ impact: null });
    renderImpactBanner();
    renderGraph();
  });
  dom.closeInspector.addEventListener("click", closeInspector);
  dom.inspectorScrim.addEventListener("click", closeInspector);
  for (const tab of dom.tabs) {
    tab.addEventListener("click", () => selectTab(tab.dataset.tab, false));
    tab.addEventListener("keydown", (event) => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      const current = dom.tabs.indexOf(tab);
      let next = current;
      if (event.key === "ArrowLeft") next = (current - 1 + dom.tabs.length) % dom.tabs.length;
      if (event.key === "ArrowRight") next = (current + 1) % dom.tabs.length;
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = dom.tabs.length - 1;
      selectTab(dom.tabs[next].dataset.tab, true);
    });
  }
  document.addEventListener("keydown", (event) => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      openCommandMenu();
      return;
    }
    if (dom.commandDialog.open || dom.shortcutDialog.open) return;
    const editable = isEditableTarget(event.target) || isEditableTarget(document.activeElement);
    if (event.key === "?" && !editable) {
      event.preventDefault();
      openShortcutSheet();
      return;
    }
    if (event.key.toLowerCase() === "f" && !editable && !event.ctrlKey && !event.metaKey && !event.altKey) {
      event.preventDefault();
      setViewBox({ x: 0, y: 0, width: 1000, height: 1000 });
      return;
    }
    if (event.key === "Escape" && state.selected) {
      event.preventDefault();
      closeInspector();
    }
    if (event.key === "/" && !editable) {
      event.preventDefault();
      dom.search.focus();
    }
  });
  window.addEventListener("popstate", () => {
    readURL();
    renderAll();
  });
  window.addEventListener("resize", () => {
    renderStates();
    renderViewSwitch();
    renderInspector();
  });
  window.addEventListener("online", connectWebSocket);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") {
      refresh({ silent: true });
      connectWebSocket();
    }
  });
}

function startPolling() {
  window.setInterval(renderUptime, 1000);
  window.setInterval(() => {
    if (document.visibilityState === "visible") refresh({ silent: true });
  }, POLL_INTERVAL_MS);
  window.setInterval(() => {
    if (document.visibilityState === "visible") refreshAnomalies();
  }, 60000);
}

initializeTheme();
readURL();
renderConnectionEndpoints();
bindEvents();
setViewBox(state.viewBox);
renderAll();
refresh({ silent: false });
refreshAnomalies();
connectWebSocket();
startPolling();

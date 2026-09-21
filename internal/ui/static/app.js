import { CARD_WIDTH, CARD_HEIGHT, layoutGraph, partitionNeighborhoods, boundedNeighborhood, topologyKey } from "./map-layout.js";

const SVG_NS = "http://www.w3.org/2000/svg";
const POLL_INTERVAL_MS = 30000;
const TOOL_CACHE_MS = 300000;
const HOST_PREFIX = "host/";
// HOST_NAME_PATTERN bounds what the page accepts as a host name from the URL
// or a response before it builds a request URL from it.
const HOST_NAME_PATTERN = /^[A-Za-z0-9._:-]{1,255}$/;

function validHostName(value) {
  return typeof value === "string" && HOST_NAME_PATTERN.test(value) ? value : null;
}
const HOST_METRICS_WINDOW_MS = 3600000;
// Standard hostmetrics names. The first three always render; the usage pair
// renders only when the host reports it.
const HOST_METRICS = [
  { name: "system.cpu.utilization", label: "CPU", kind: "ratio", always: true },
  { name: "system.memory.utilization", label: "Memory", kind: "ratio", always: true },
  { name: "system.filesystem.utilization", label: "Disk", kind: "ratio", always: true },
  { name: "system.memory.usage", label: "Memory used", kind: "bytes" },
  { name: "system.filesystem.usage", label: "Disk used", kind: "bytes" },
];

const byId = (id) => document.getElementById(id);
const dom = {
  connection: byId("connection-status"),
  connectionLabel: byId("connection-label"),
  pulseHealth: byId("pulse-health"),
  pulseErrors: byId("pulse-errors"),
  pulseLatencyLabel: byId("pulse-latency-label"),
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
  hostGroup: byId("host-group-button"),
  mapSummary: byId("map-summary"),
  componentSelect: byId("component-select"),
  scopeSelect: byId("scope-select"),
  resetScope: byId("reset-scope-button"),
  disconnectedButton: byId("disconnected-button"),
  disconnectedInventory: byId("disconnected-inventory"),
  disconnectedList: byId("disconnected-list"),
  overview: byId("system-overview"),
  overviewButton: byId("system-overview-button"),
  overviewSummary: byId("overview-summary"),
  overviewGrid: byId("overview-grid"),
  mapBack: byId("map-back-button"),
  neighborhoodList: byId("neighborhood-list"),
  neighborhoodIndex: byId("neighborhood-index-button"),
  serviceIndex: byId("service-index-button"),
  mapEvidence: byId("map-evidence"),
  mapEvidenceSummary: byId("map-evidence-summary"),
  mapMembers: byId("map-member-list"),
  mapConnections: byId("map-connection-list"),
  map: byId("service-map"),
  graphDescription: byId("graph-description"),
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
  railEyebrow: byId("rail-eyebrow"),
  severity: byId("severity-summary"),
  impactBanner: byId("impact-banner"),
  impactLabel: byId("impact-label"),
  clearImpact: byId("clear-impact-button"),
  inspector: byId("inspector"),
  inspectorScrim: byId("inspector-scrim"),
  inspectorTitle: byId("inspector-title"),
  inspectorEyebrow: byId("inspector-eyebrow"),
  inspectorTabs: byId("inspector-tabs"),
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
  selectedHost: null,
  selectedEdge: null,
  component: null,
  scope: "neighbors",
  focusRoot: null,
  inventory: false,
  overview: false,
  indexMode: "neighborhoods",
  mapHistory: [],
  initializedComponents: new Set(),
  explicitFullScope: false,
  partition: null,
  layout: null,
  layoutKey: "",
  viewportScope: "",
  pendingFit: true,
  groupBy: "service",
  hosts: null,
  hostsError: "",
  hostMetrics: new Map(),
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

function formatP99(value, provenance) {
  const p99 = provenance && typeof provenance === "object" && provenance.p99 && typeof provenance.p99 === "object"
    ? provenance.p99
    : null;
  if (!p99) {
    return {
      label: "Reported p99",
      value: formatMs(value),
      explanation: "Server did not report percentile provenance",
    };
  }

  const samples = Math.max(0, finite(p99.sample_count, 0));
  const lowSample = p99.low_sample
    ? (samples ? formatCount(samples) + " samples; low sample" : "Low sample")
    : "";
  const explain = (text) => lowSample ? text + "; " + lowSample : text;
  switch (String(p99.status || "")) {
  case "measured":
    return {
      label: "P99",
      value: formatMs(value),
      explanation: explain("Measured from " + formatCount(samples) + " spans"),
    };
  case "approximate": {
    const bound = Number(p99.relative_error_bound);
    const accuracy = p99.degraded
      ? "DDSketch; degraded accuracy"
      : Number.isFinite(bound) && bound > 0
        ? "DDSketch, ±" + trimFixed(bound * 100, 1) + "%"
        : "DDSketch approximation";
    return { label: "Approx. p99", value: formatMs(value), explanation: explain(accuracy) };
  }
  case "estimated":
    return {
      label: "Estimated tail",
      value: Number.isFinite(Number(value)) ? "~" + formatMs(value) : "—",
      explanation: explain("Derived from average latency; not a measured percentile"),
    };
  case "bounded": {
    const population = Math.max(0, finite(p99.population_count, 0));
    return {
      label: "Sample p99",
      value: formatMs(value),
      explanation: explain(formatCount(samples) + " retained spans out of " + formatCount(population)),
    };
  }
  case "unavailable":
    return { label: "P99 unavailable", value: "—", explanation: "No percentile data" };
  default:
    return {
      label: "Reported p99",
      value: formatMs(value),
      explanation: "Server reported unknown percentile provenance",
    };
  }
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

function formatBytes(value) {
  const size = Number(value);
  if (!Number.isFinite(size)) return "—";
  if (size < 1048576) return trimFixed(size / 1024, 0) + "KB";
  return formatMB(size / 1048576);
}

function isHostNode(node) {
  return Boolean(node) && (node.kind === "host" || String(node.id).startsWith(HOST_PREFIX));
}

function hostsAvailable() {
  return Array.isArray(state.hosts) && state.hosts.length > 0;
}

function hostGroupReason() {
  if (hostsAvailable()) return "";
  if (state.hosts === null) return state.hostsError ? "Host list unavailable: " + state.hostsError : "Loading hosts…";
  return "No hosts reported yet.";
}

function hostByName(name) {
  return (state.hosts || []).find((host) => host.name === name) || null;
}

// nodeHosts lists the hosts a service was observed on: the node's own stamp
// when present, else the registry listing (the graph response is cached for
// a few seconds and can lag the host list).
function nodeHosts(node) {
  if (Array.isArray(node.hosts) && node.hosts.length) return node.hosts;
  const listed = [];
  for (const host of state.hosts || []) {
    if (Array.isArray(host.services) && host.services.includes(node.id)) listed.push(host.name);
  }
  return listed;
}

function nodeHostCount(node) {
  return Math.max(finite(node.host_count, 0), nodeHosts(node).length);
}

// hostOfNode is the cluster a node belongs to: its own host for a host
// entity, else the first host it was seen on.
function hostOfNode(node) {
  if (isHostNode(node)) {
    return Array.isArray(node.hosts) && node.hosts.length ? node.hosts[0] : node.id.slice(HOST_PREFIX.length);
  }
  return nodeHosts(node)[0];
}

// hostClusters folds the graph into one cluster per host in host order,
// with services that no host claims last under a null host.
function hostClusters() {
  const byHost = new Map();
  for (const host of state.hosts || []) byHost.set(host.name, { host: host, members: [] });
  const unassigned = { host: null, members: [] };
  for (const node of state.graph ? state.graph.nodes : []) {
    const cluster = byHost.get(hostOfNode(node));
    (cluster || unassigned).members.push(node);
  }
  const clusters = Array.from(byHost.values());
  if (unassigned.members.length) clusters.push(unassigned);
  return clusters;
}

function clusterServiceCount(cluster) {
  if (cluster.host) return finite(cluster.host.service_count, 0);
  return cluster.members.filter((node) => !isHostNode(node)).length;
}

function clusterLabel(cluster) {
  const count = clusterServiceCount(cluster);
  return (cluster.host ? cluster.host.name : "No host") + " · " + count + (count === 1 ? " service" : " services");
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

// sortedNodes is the worst-first service ranking. Host entities are not
// services and never enter it.
function sortedNodes() {
  const nodes = state.graph && Array.isArray(state.graph.nodes) ? state.graph.nodes.filter((node) => !isHostNode(node)) : [];
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
  state.selectedHost = validHostName(params.get("host"));
  state.selected = state.selectedHost ? null : params.get("service");
  state.groupBy = params.get("group") === "host" ? "host" : "service";
  const tab = params.get("tab");
  state.activeTab = ["overview", "why", "impact", "dependencies"].includes(tab) ? tab : "overview";
  state.impactRoot = params.get("impact");
  state.component = params.get("component");
  state.scope = ["all", "neighbors", "callers", "dependencies"].includes(params.get("scope")) ? params.get("scope") : "neighbors";
  state.explicitFullScope = params.get("scope") === "all";
  state.focusRoot = params.get("focus") || state.selected;
  state.inventory = params.get("view") === "unconnected";
  state.overview = state.groupBy !== "host" && (params.get("view") === "overview" || state.component === "all");
  state.selectedEdge = null;
  state.pendingFit = true;
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

  // The host list rides the existing poll only while something shows it; an
  // idle service view adds no request. Explicit refreshes and the first load
  // always fetch it so the toggle knows whether hosts exist.
  const wantHosts = !silent || state.hosts === null || state.groupBy === "host" || Boolean(state.selectedHost);
  const results = await Promise.allSettled([
    fetchJSON("/api/system/graph"),
    fetchJSON("/api/metrics/dashboard"),
    fetchJSON("/api/stats"),
    wantHosts ? fetchJSON("/api/hosts") : Promise.reject(new Error("skipped")),
  ]);

  if (wantHosts) {
    if (results[3].status === "fulfilled") {
      state.hosts = Array.isArray(results[3].value) ? results[3].value.filter((host) => host && typeof host.name === "string") : [];
      state.hostsError = "";
    } else if (state.hosts === null) {
      state.hostsError = results[3].reason && results[3].reason.message ? results[3].reason.message : "Unknown response";
    }
  }
  if (state.selectedHost) {
    // Silent refreshes arrive with every WebSocket snapshot; the panel's
    // five series follow the slower poll cadence unless asked explicitly.
    const loaded = state.hostMetrics.get(state.selectedHost);
    if (!silent || !loaded || Date.now() - loaded.loadedAt > POLL_INTERVAL_MS) loadHostMetrics(state.selectedHost);
  }

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

function formatPulseLatency(summary, dashboard) {
  if (Object.prototype.hasOwnProperty.call(dashboard, "p99_latency_ms") || dashboard.latency_provenance) {
    return formatP99(dashboard.p99_latency_ms, dashboard.latency_provenance);
  }
  if (Number.isFinite(Number(summary.avg_latency_ms))) {
    return { label: "Average", value: formatMs(summary.avg_latency_ms), explanation: "Arithmetic mean latency" };
  }
  return { label: "P99 unavailable", value: "—", explanation: "No percentile data" };
}

function renderStates() {
  const nodeCount = state.graph && state.graph.nodes ? state.graph.nodes.length : 0;
  dom.loading.hidden = !state.loading;
  dom.error.hidden = state.loading || !state.error;
  dom.empty.hidden = state.loading || Boolean(state.error) || nodeCount > 0;
  dom.canvas.hidden = state.loading || Boolean(state.error) || nodeCount === 0 || state.inventory || state.overview || (isMobile() && state.mobileMode === "list");
  dom.mobileList.hidden = state.loading || Boolean(state.error) || nodeCount === 0 || state.overview || !isMobile() || state.mobileMode !== "list";
  dom.disconnectedInventory.hidden = state.loading || Boolean(state.error) || nodeCount === 0 || !state.inventory || (isMobile() && state.mobileMode === "list");
  dom.overview.hidden = state.loading || Boolean(state.error) || nodeCount === 0 || !state.overview;
  dom.mapEvidence.hidden = dom.canvas.hidden || state.groupBy === "host";
  document.body.classList.toggle("is-system-overview", state.overview);
  document.body.classList.toggle("is-host-map", state.groupBy === "host");
  dom.errorMessage.textContent = state.error;
}

function renderPulse() {
  const summary = state.graph && state.graph.system ? state.graph.system : {};
  const dashboard = state.dashboard || {};
  const stats = state.stats || {};
  const services = sortedNodes();
  const healthy = services.filter((node) => normalizeStatus(node) === "healthy").length;
  dom.pulseHealth.textContent = state.graph ? healthy + " / " + services.length : "—";
  dom.pulseHealth.style.color = "var(--text)";
  dom.pulseHealth.setAttribute("aria-label", healthy + " of " + services.length + " services healthy");
  dom.pulseErrors.textContent = Number.isFinite(Number(dashboard.error_rate))
    ? formatPercent(dashboard.error_rate, 1)
    : formatRatio(summary.total_error_rate, 1);
  const tail = formatPulseLatency(summary, dashboard);
  dom.pulseLatencyLabel.textContent = tail.label;
  dom.pulseP99.textContent = tail.value;
  dom.pulseP99.title = tail.explanation;
  dom.pulseP99.setAttribute("aria-label", tail.label + ": " + tail.value + ". " + tail.explanation);
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
  for (const node of sortedNodes()) {
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
  const tail = formatP99(metrics.p99_latency_ms, metrics.latency_provenance);
  let detail = tail.value + " " + tail.label.toLowerCase() + " · " + formatRatio(metrics.error_rate, 1) + " error";
  if (state.anomalies.has(node.id)) detail += " · anomaly";
  if (state.groupBy === "host" && nodeHostCount(node) > 1) detail += " · " + nodeHostCount(node) + " hosts";
  main.appendChild(textElement("span", "service-meta", detail));
  main.title = tail.explanation;
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

function hostHeading(cluster) {
  const count = clusterServiceCount(cluster);
  const heading = document.createElement(cluster.host ? "button" : "p");
  heading.className = "host-heading";
  if (cluster.host) {
    heading.type = "button";
    heading.dataset.host = cluster.host.name;
    heading.setAttribute("aria-current", cluster.host.name === state.selectedHost ? "true" : "false");
    heading.setAttribute("aria-label", clusterLabel(cluster) + ", open host panel");
    heading.addEventListener("click", () => openHost(cluster.host.name));
  }
  const mark = document.createElement("i");
  mark.className = "host-mark";
  mark.setAttribute("aria-hidden", "true");
  heading.append(
    mark,
    textElement("span", "host-name", cluster.host ? cluster.host.name : "No host"),
    textElement("span", "host-count", count + (count === 1 ? " service" : " services"))
  );
  return heading;
}

// groupedRows renders the host-mode list: one heading per host, then the
// worst-first rows of the services whose first host it is.
function groupedRows(filtered) {
  const visible = new Set(filtered.map((node) => node.id));
  const fragment = document.createDocumentFragment();
  for (const cluster of hostClusters()) {
    const members = cluster.members.filter((node) => visible.has(node.id));
    if (state.query && !members.length) continue;
    fragment.appendChild(hostHeading(cluster));
    for (const node of filtered) {
      if (members.includes(node)) fragment.appendChild(serviceButton(node));
    }
  }
  return fragment;
}

function renderServiceLists() {
  const all = sortedNodes();
  const filtered = filteredSortedNodes();
  const hostMode = state.groupBy === "host";
  const fragments = [document.createDocumentFragment(), document.createDocumentFragment()];
  for (const fragment of fragments) {
    if (hostMode) fragment.appendChild(groupedRows(filtered));
    else for (const node of filtered) fragment.appendChild(serviceButton(node));
    if (filtered.length === 0 && all.length > 0) fragment.appendChild(textElement("p", "quiet", "No services match this search."));
  }
  dom.serviceList.replaceChildren(fragments[0]);
  dom.mobileList.replaceChildren(fragments[1]);
  dom.railEyebrow.textContent = hostMode ? "By host" : "Worst first";
  dom.serviceCount.textContent = String(all.length);
  dom.searchCount.textContent = state.query ? String(filtered.length) + " found" : "";
  const services = hostMode || state.indexMode === "services" || Boolean(state.query.trim()) || !state.partition?.components.length;
  dom.serviceList.hidden = !services;
  dom.neighborhoodList.hidden = services;
  dom.neighborhoodIndex.setAttribute("aria-pressed", String(!services));
  dom.serviceIndex.setAttribute("aria-pressed", String(services));
  dom.railEyebrow.textContent = hostMode ? "By host" : services ? "Worst first" : "All " + all.length + " services";
}

function cleanEdges(nodes, edges) {
  const ids = new Set(nodes.map((node) => node.id));
  const seen = new Set();
  const output = [];
  for (const edge of edges) {
    if (!ids.has(edge.source) || !ids.has(edge.target) || edge.kind === "runs_on") continue;
    const key = edge.source + ">" + edge.target;
    if (seen.has(key)) continue;
    seen.add(key);
    output.push(edge);
  }
  output.sort((a, b) => (a.source + ">" + a.target).localeCompare(b.source + ">" + b.target));
  return output;
}

// hostNodes synthesizes one host node per registry host the graph does not
// already carry as a host entity, so host mode draws every host once.
function hostNodes(nodes) {
  const present = new Set(nodes.filter(isHostNode).map(hostOfNode));
  const output = [];
  for (const host of state.hosts || []) {
    if (!present.has(host.name)) output.push({ id: HOST_PREFIX + host.name, kind: "host", hosts: [host.name] });
  }
  return output;
}

// runsOnEdges is one placement edge per (service, host) pair. They exist
// for drawing and layout only and never enter the call-edge traversals.
function runsOnEdges(nodes) {
  const hostIds = new Set(nodes.filter(isHostNode).map((node) => node.id));
  const output = [];
  for (const node of nodes) {
    if (isHostNode(node)) continue;
    for (const host of nodeHosts(node)) {
      const target = HOST_PREFIX + host;
      if (hostIds.has(target)) output.push({ source: node.id, target: target, kind: "runs_on" });
    }
  }
  return output;
}

function hostNodeLabel(name) {
  const host = hostByName(name);
  if (!host) return name + ", host";
  const count = finite(host.service_count, 0);
  return name + ", " + count + (count === 1 ? " service" : " services");
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

function connectionKey(edge) {
  return edge.source + ">" + edge.target;
}

function scopeNeighbors(root, edges, scope) {
  const ids = new Set([root]);
  for (const edge of edges) {
    if (edge.source === root && scope !== "callers") ids.add(edge.target);
    if (edge.target === root && scope !== "dependencies") ids.add(edge.source);
  }
  return ids;
}

function initialFocus(nodes, edges) {
  const degree = new Map(nodes.map((node) => [node.id, 0]));
  for (const edge of edges) {
    if (degree.has(edge.source)) degree.set(edge.source, degree.get(edge.source) + 1);
    if (degree.has(edge.target)) degree.set(edge.target, degree.get(edge.target) + 1);
  }
  const bounded = nodes.filter((node) => degree.get(node.id) < 30);
  return (bounded.length ? bounded : nodes).map((node) => node.id).sort((a, b) => degree.get(b) - degree.get(a) || a.localeCompare(b))[0];
}

function neighborhoodRoot(nodes, edges) {
  const critical = nodes.filter((node) => normalizeStatus(node) === "critical");
  return initialFocus(critical.length ? critical : nodes, edges);
}

function graphModel() {
  const services = state.graph.nodes.filter((node) => !isHostNode(node));
  const callEdges = cleanEdges(services, state.graph.edges);
  const partition = partitionNeighborhoods(services, callEdges);
  state.partition = partition;
  const component = partition.components.find((item) => item.nodeIds.includes(state.selected))
    || partition.components.find((item) => item.id === state.component)
    || partition.components.find((item) => item.nodeIds.includes(state.focusRoot))
    || partition.components.find((item) => item.nodeIds.includes(neighborhoodRoot(services, callEdges)))
    || partition.components[0];
  state.component = component ? component.id : null;
  const members = new Set(component ? component.nodeIds : []);
  const groupNodes = services.filter((node) => members.has(node.id));
  if (!services.some((node) => node.id === state.focusRoot) || !members.has(state.focusRoot)) {
    state.focusRoot = members.has(state.selected) ? state.selected : neighborhoodRoot(groupNodes, callEdges);
  }
  let nodes = groupNodes;
  let edges = callEdges.filter((edge) => members.has(edge.source) && members.has(edge.target));
  let remainingIds = [];
  if (state.groupBy === "host") {
    state.overview = false;
    nodes = services.concat(state.graph.nodes.filter(isHostNode), hostNodes(state.graph.nodes));
    edges = callEdges;
  } else if (state.selectedEdge) {
    const edge = callEdges.find((item) => connectionKey(item) === state.selectedEdge);
    if (edge) {
      nodes = services.filter((node) => node.id === edge.source || node.id === edge.target);
      edges = [edge];
    }
  } else if (state.scope !== "all" && state.focusRoot) {
    ({ nodes, edges, remainingIds } = boundedNeighborhood(services, callEdges, state.focusRoot, state.scope));
  }
  if (!partition.disconnected.length) state.inventory = false;
  if (state.groupBy !== "host" && partition.disconnected.some((node) => node.id === state.selected)) {
    state.inventory = true;
    state.overview = false;
  }
  if (!partition.components.length && state.groupBy !== "host" && !state.overview) state.inventory = true;
  return { nodes, edges, placements: state.groupBy === "host" ? runsOnEdges(nodes) : [], partition,
    component, services, callEdges, remainingIds, totalConnected: services.length - partition.disconnected.length,
    groupCount: groupNodes.length };
}

function updateMapURL() {
  updateURL({ component: state.component, scope: state.scope,
    focus: state.scope === "all" ? null : state.focusRoot,
    view: state.overview ? "overview" : state.inventory ? "unconnected" : null });
}

function rememberMap() {
  const saved = {};
  for (const key of ["component", "scope", "focusRoot", "inventory", "overview", "selected", "selectedHost", "selectedEdge", "activeTab", "groupBy", "indexMode", "query", "viewportScope"]) saved[key] = state[key];
  saved.viewBox = { ...state.viewBox };
  state.mapHistory.push(saved);
  if (state.mapHistory.length > 30) state.mapHistory.shift();
}

function openNeighborhood(id) {
  const component = state.partition?.components.find((item) => item.id === id);
  if (!component) return;
  rememberMap();
  state.component = id;
  state.selected = state.selectedHost = state.selectedEdge = null;
  state.focusRoot = neighborhoodRoot(state.graph.nodes.filter((node) => component.nodeIds.includes(node.id)), state.graph.edges);
  state.scope = "neighbors";
  state.overview = state.inventory = false;
  state.pendingFit = true;
  state.query = dom.search.value = "";
  state.mobileMode = "map";
  updateURL({ service: null, host: null, tab: null });
  updateMapURL();
  renderAll();
}

function showSystemOverview() {
  rememberMap();
  state.selected = state.selectedHost = state.selectedEdge = null;
  state.groupBy = "service";
  state.overview = true;
  state.inventory = false;
  updateURL({ service: null, host: null, tab: null, group: null });
  updateMapURL();
  renderAll();
}

function goBackOnMap() {
  const saved = state.mapHistory.pop();
  if (!saved) return;
  Object.assign(state, saved);
  state.pendingFit = false;
  dom.search.value = state.query;
  updateURL({ service: state.selected, host: state.selectedHost, tab: state.activeTab === "overview" ? null : state.activeTab,
    group: state.groupBy === "host" ? "host" : null });
  updateMapURL();
  renderAll();
  setViewBox(saved.viewBox);
}

function mapButton(label, className, action) {
  const button = textElement("button", className, label);
  button.type = "button";
  button.addEventListener("click", action);
  return button;
}

function groupHealth(group, byId) {
  const counts = { critical: 0, degraded: 0, unknown: 0, healthy: 0 };
  group.nodeIds.forEach((id) => { counts[normalizeStatus(byId.get(id))] += 1; });
  return Object.entries(counts).filter((entry) => entry[1]).map(([status, count]) => count + " " + status).join(" · ");
}

function renderMapScope(model) {
  const components = model.partition.components;
  const byId = new Map(model.services.map((node) => [node.id, node]));
  const signature = components.map((item) => item.id + ":" + item.nodeIds.length).join("|");
  if (dom.componentSelect.dataset.signature !== signature) {
    const options = components.map((item) => {
      const option = textElement("option", "", item.label + " · " + item.nodeIds.length + " services");
      option.value = item.id;
      return option;
    });
    if (components.length) {
      const all = textElement("option", "", "System overview · " + model.services.length + " services");
      all.value = "all";
      options.unshift(all);
    }
    dom.componentSelect.replaceChildren(...options);
    dom.componentSelect.dataset.signature = signature;
  }
  dom.componentSelect.value = state.overview ? "all" : state.component || "";
  dom.componentSelect.hidden = !components.length;
  dom.componentSelect.disabled = !components.length || state.groupBy === "host";
  dom.scopeSelect.value = state.scope;
  dom.scopeSelect.hidden = !components.length || state.overview;
  dom.scopeSelect.disabled = state.inventory || !model.nodes.length || state.groupBy === "host";
  dom.resetScope.hidden = !components.length || state.overview || state.scope === "all" && !state.inventory;
  dom.resetScope.disabled = state.groupBy === "host";
  dom.disconnectedButton.hidden = model.partition.disconnected.length === 0;
  dom.disconnectedButton.textContent = "No observed dependencies (" + model.partition.disconnected.length + ")";
  dom.disconnectedButton.setAttribute("aria-pressed", state.inventory ? "true" : "false");
  dom.disconnectedButton.disabled = model.partition.disconnected.length === 0;
  dom.overviewButton.setAttribute("aria-pressed", String(state.overview));
  dom.mapBack.disabled = state.mapHistory.length === 0;
  const shown = state.inventory ? model.partition.disconnected.length : model.nodes.filter((node) => !isHostNode(node)).length;
  dom.mapSummary.textContent = state.overview
    ? model.services.length + " services · " + model.callEdges.length + " observed dependencies · System overview"
    : state.inventory ? shown + " services with no observed dependencies · " + model.services.length + " services total"
    : "Showing " + shown + " services · " + model.services.length + " services total · "
      + (state.groupBy === "host" ? "By host" : state.selectedEdge ? "Selected connection" : (model.component?.label || "Service flow") + " · " + model.groupCount + " neighborhood members")
      + (model.remainingIds.length ? " · " + model.remainingIds.length + " more neighbors in evidence list" : "");
  dom.overviewSummary.textContent = model.services.length + " services · " + model.callEdges.length + " observed dependencies · "
    + model.partition.componentCount + " connected components · Navigation groups do not imply ownership";
  dom.overviewGrid.replaceChildren();
  dom.neighborhoodList.replaceChildren();
  for (const group of components) {
    const health = groupHealth(group, byId);
    const row = mapButton("", "neighborhood-row", () => openNeighborhood(group.id));
    row.dataset.neighborhood = group.id;
    row.setAttribute("aria-current", String(group.id === state.component));
    row.append(textElement("strong", "", group.label), textElement("small", "", group.nodeIds.length + " services · " + health));
    dom.neighborhoodList.append(row);
    const card = document.createElement("article");
    card.className = "overview-card";
    card.dataset.neighborhood = group.id;
    const heading = document.createElement("div");
    heading.className = "overview-heading";
    heading.append(textElement("h3", "overview-title", group.label), textElement("span", "overview-health", health));
    const marks = document.createElement("div");
    marks.className = "overview-members";
    group.nodeIds.forEach((id) => {
      const node = byId.get(id);
      const mark = mapButton("", "overview-member " + normalizeStatus(node), () => openInspector(id));
      mark.dataset.service = id;
      mark.style.backgroundColor = statusColor(normalizeStatus(node));
      mark.setAttribute("aria-label", id + ", " + normalizeStatus(node));
      mark.title = id + ", " + normalizeStatus(node);
      marks.append(mark);
    });
    card.append(heading, marks,
      textElement("span", "overview-counts", group.nodeIds.length + " services · " + group.incomingEdges.length + " incoming · " + group.outgoingEdges.length + " outgoing"),
      mapButton("Open map", "overview-open", () => openNeighborhood(group.id)));
    dom.overviewGrid.append(card);
  }
  if (model.partition.disconnected.length) {
    const card = document.createElement("article");
    card.className = "overview-card";
    card.append(textElement("h3", "overview-title", "No observed dependencies"),
      textElement("p", "overview-counts", model.partition.disconnected.length + " searchable services"),
      mapButton("Open services", "overview-open", () => dom.disconnectedButton.click()));
    dom.overviewGrid.append(card);
  }
  const group = model.component;
  const allEdges = group ? [...group.internalEdges, ...group.incomingEdges, ...group.outgoingEdges]
    .sort((a, b) => connectionKey(a).localeCompare(connectionKey(b))) : [];
  dom.mapMembers.replaceChildren(...(group ? group.nodeIds.map((id) => serviceButton(byId.get(id))) : []));
  dom.mapConnections.replaceChildren(...allEdges.map((edge) => {
    const row = mapButton("", "map-connection-row", () => openConnection(edge));
    row.dataset.source = edge.source;
    row.dataset.target = edge.target;
    const crossing = !group.nodeIds.includes(edge.source) || !group.nodeIds.includes(edge.target);
    row.append(textElement("strong", "", edge.source + " → " + edge.target),
      textElement("span", "", edgeEvidence(edge) + (crossing ? " · Cross-neighborhood" : "")));
    return row;
  }));
  dom.mapEvidenceSummary.textContent = (group?.nodeIds.length || 0) + " neighborhood members · " + allEdges.length + " observed dependencies"
    + (group ? " · " + (group.incomingEdges.length + group.outgoingEdges.length) + " cross-neighborhood" : "");
  const query = state.query.trim().toLowerCase();
  const isolated = model.partition.disconnected.filter((node) => !query || node.id.toLowerCase().includes(query));
  dom.disconnectedList.replaceChildren(...isolated.map(serviceButton));
  if (!isolated.length) dom.disconnectedList.appendChild(textElement("p", "quiet", "No services match this search."));
  renderServiceLists();
}

function routedPath(points) {
  points = (points || []).filter((point, index, route) => !index || point.x !== route[index - 1].x || point.y !== route[index - 1].y);
  if (points.length < 2) return "";
  let path = "M" + points[0].x + "," + points[0].y;
  for (let index = 1; index < points.length - 1; index += 1) {
    const previous = points[index - 1];
    const corner = points[index];
    const next = points[index + 1];
    const incoming = Math.hypot(corner.x - previous.x, corner.y - previous.y);
    const outgoing = Math.hypot(next.x - corner.x, next.y - corner.y);
    // Round within the existing route, keeping endpoints and arrow direction.
    const radius = Math.min(48, incoming / 2, outgoing / 2);
    if (!radius) continue;
    const entryX = corner.x + (previous.x - corner.x) * radius / incoming;
    const entryY = corner.y + (previous.y - corner.y) * radius / incoming;
    const exitX = corner.x + (next.x - corner.x) * radius / outgoing;
    const exitY = corner.y + (next.y - corner.y) * radius / outgoing;
    path += " L" + entryX + "," + entryY + " Q" + corner.x + "," + corner.y + " " + exitX + "," + exitY;
  }
  const end = points[points.length - 1];
  return path + " L" + end.x + "," + end.y;
}

function edgeEvidence(edge) {
  if (!(Number(edge.call_count) > 0)) return formatCount(edge.call_count) + " calls · latency and errors not reported";
  return formatCount(edge.call_count) + " calls · " + formatMs(edge.avg_latency_ms) + " average · " + formatRatio(edge.error_rate, 1) + " errors";
}

function activateWithKeyboard(element, activate) {
  element.addEventListener("click", (event) => {
    event.stopPropagation();
    activate();
  });
  element.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      activate();
    }
  });
}

function appendCardText(group, className, x, y, text) {
  const label = svgElement("text", { class: className, x, y });
  label.textContent = text;
  group.appendChild(label);
  return label;
}

function renderGraph() {
  const focus = document.activeElement;
  const restoreNode = focus && focus.closest("#graph-nodes") ? focus.dataset.service : null;
  const restoreEdge = focus && focus.closest("#graph-edges") ? focus.dataset.edge : null;
  dom.rings.replaceChildren();
  dom.edges.replaceChildren();
  dom.nodes.replaceChildren();
  dom.minimapEdges.replaceChildren();
  dom.minimapNodes.replaceChildren();
  if (!state.graph || state.graph.nodes.length === 0) {
    dom.disconnectedInventory.hidden = true;
    dom.mapSummary.textContent = "Waiting for telemetry";
    return;
  }
  const model = graphModel();
  renderMapScope(model);
  renderStates();
  if (state.inventory || state.overview) return;
  const { nodes, edges, placements } = model;
  const key = topologyKey(nodes, edges.concat(placements));
  if (key !== state.layoutKey || !state.layout) {
    state.layout = layoutGraph(nodes, edges.concat(placements));
    state.layoutKey = key;
    dom.map.dataset.layoutMs = String(state.layout.durationMs);
  }
  const { positions, routes, bounds } = state.layout;
  const scopeKey = [state.groupBy, state.component, state.scope, state.selectedEdge || "", state.scope === "all" ? "" : state.focusRoot].join(":");
  const fit = state.pendingFit || state.viewportScope !== scopeKey;
  state.viewportScope = scopeKey;
  state.pendingFit = false;
  dom.graphDescription.textContent = "Application call flow from left to right. Arrowheads point to downstream dependencies. Select a named service or a connection to inspect it."
    + (state.groupBy === "host" ? " Dashed lines show host placement, not application calls." : "");
  dom.minimap.setAttribute("viewBox", [bounds.x, bounds.y, bounds.width, bounds.height].join(" "));
  const searchMatches = graphSearchMatches();
  const impact = state.impactRoot ? downstreamDepths(state.impactRoot, state.graph.edges, 5) : null;
  const neighbors = state.selected ? scopeNeighbors(state.selected, state.graph.edges, "neighbors") : null;
  for (const edge of placements) {
    dom.edges.appendChild(svgElement("path", { class: "runs-on-edge", "data-kind": "runs_on",
      "data-source": edge.source, "data-target": edge.target, d: routedPath(routes.get(connectionKey(edge))) }));
  }
  for (const edge of edges) {
    const points = routes.get(connectionKey(edge));
    if (!points) continue;
    const id = connectionKey(edge);
    const selected = state.selectedEdge === id;
    const related = state.selected && (edge.source === state.selected || edge.target === state.selected);
    const inImpact = impact && impact.has(edge.source) && impact.has(edge.target);
    const line = svgElement("path", { class: "graph-edge", "data-source": edge.source,
      "data-target": edge.target, "data-edge": id, d: routedPath(points),
      "marker-end": "url(#edge-arrow)", role: "button", tabindex: "0",
      "aria-label": edge.source + " to " + edge.target + ": " + edgeEvidence(edge), "aria-pressed": String(selected) });
    if (selected) line.classList.add("is-selected");
    if (related) line.classList.add("is-related");
    if (inImpact) line.classList.add("is-impact");
    if (Number(edge.error_rate) > 0.05 || edge.status === "critical") line.classList.add("is-critical");
    if (searchMatches && !searchMatches.has(edge.source) && !searchMatches.has(edge.target)) line.classList.add("is-dim");
    else if (state.selected && !related) line.classList.add("is-dim");
    else if (impact && !inImpact) line.classList.add("is-dim");
    const title = svgElement("title");
    title.textContent = edge.source + " → " + edge.target + ": " + edgeEvidence(edge);
    line.appendChild(title);
    activateWithKeyboard(line, () => openConnection(edge));
    const hit = svgElement("path", { class: "edge-hit", d: routedPath(points), "aria-hidden": "true" });
    hit.addEventListener("click", (event) => { event.stopPropagation(); openConnection(edge); });
    dom.edges.append(hit, line);
    // Edge evidence stays accessible on every connection. Print labels only
    // where the map's spacing can support them without covering other nodes.
    if ((edges.length <= 20 || related || selected) && points.length > 1) {
      const middle = points[Math.floor(points.length / 2)];
      const label = svgElement("text", { class: "edge-label", x: middle.x, y: middle.y - 9, "text-anchor": "middle", "aria-hidden": "true" });
      label.textContent = formatCount(edge.call_count) + " calls · " + (Number(edge.call_count) > 0 ? formatMs(edge.avg_latency_ms) : "latency not reported");
      if (line.classList.contains("is-dim")) label.classList.add("is-dimmed");
      dom.edges.appendChild(label);
    }
    dom.minimapEdges.appendChild(svgElement("path", { class: "minimap-edge", d: routedPath(points) }));
  }
  for (const node of nodes) {
    const point = positions.get(node.id);
    if (!point) continue;
    const hostNode = isHostNode(node);
    const hostName = hostNode ? hostOfNode(node) : "";
    const status = hostNode ? "unknown" : normalizeStatus(node);
    const group = svgElement("g", { class: "service-node", transform: "translate(" + (point.x - CARD_WIDTH / 2) + " " + (point.y - CARD_HEIGHT / 2) + ")",
      tabindex: "0", role: "button", "data-status": status, "data-service": node.id,
      "aria-label": hostNode ? hostNodeLabel(hostName) : node.id + ", " + status + ", health " + formatRatio(node.health_score, 0) });
    if (hostNode) { group.dataset.kind = "host"; group.dataset.host = hostName; }
    if (node.id === state.selected || hostNode && hostName === state.selectedHost) group.classList.add("is-selected");
    if (searchMatches && searchMatches.has(node.id)) group.classList.add("is-search-match");
    if (impact && impact.has(node.id)) group.classList.add("is-impact");
    if (searchMatches && !searchMatches.has(node.id) || neighbors && !neighbors.has(node.id) && !hostNode || impact && !impact.has(node.id) && !hostNode) group.classList.add("is-dim");
    group.appendChild(svgElement("rect", { class: "node-card", width: CARD_WIDTH, height: CARD_HEIGHT, rx: 6 }));
    group.appendChild(svgElement("rect", { class: "node-status-bar", width: 4, height: CARD_HEIGHT, rx: 2 }));
    appendCardText(group, "node-kind", 14, 18, hostNode ? "HOST" : status.toUpperCase());
    const name = hostNode ? hostName : node.id;
    const label = appendCardText(group, "node-label", 14, name.length > 24 ? 36 : 41, "");
    if (name.length > 24) {
      for (let row = 0; row < 2; row += 1) {
        const part = svgElement("tspan", { x: 14, dy: row ? 15 : 0 });
        part.textContent = name.slice(row * 24, (row + 1) * 24) + (row === 1 && name.length > 48 ? "…" : "");
        label.appendChild(part);
      }
    } else label.textContent = name;
    const metrics = node.metrics || {};
    const tail = formatP99(metrics.p99_latency_ms, metrics.latency_provenance);
    appendCardText(group, "node-metrics", 14, 73, hostNode
      ? (hostByName(hostName) ? hostByName(hostName).service_count + " services" : "Host metrics")
      : formatMs(metrics.avg_latency_ms) + " avg · " + formatRatio(metrics.error_rate, 1) + " error");
    const title = svgElement("title");
    title.textContent = hostNode ? hostNodeLabel(hostName) : node.id + ", " + status + ", " + tail.label + " " + tail.value + ". " + tail.explanation;
    group.appendChild(title);
    if (hostNode) group.appendChild(svgElement("path", { class: "host-symbol", d: "M178 10 L185 17 L178 24 L171 17 Z" }));
    activateWithKeyboard(group, () => hostNode ? openHost(hostName) : openInspector(node.id));
    dom.nodes.appendChild(group);
    dom.minimapNodes.appendChild(svgElement("circle", { class: "minimap-node", cx: point.x, cy: point.y, r: 12, "data-status": status }));
  }
  if (fit) fitGraph();
  else renderMinimapViewport();
  if (restoreNode) dom.nodes.querySelector('[data-service="' + CSS.escape(restoreNode) + '"]')?.focus({ preventScroll: true });
  if (restoreEdge) dom.edges.querySelector('[data-edge="' + CSS.escape(restoreEdge) + '"]')?.focus({ preventScroll: true });
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
  const reason = hostGroupReason();
  dom.hostGroup.setAttribute("aria-pressed", state.groupBy === "host" ? "true" : "false");
  dom.hostGroup.setAttribute("aria-disabled", reason ? "true" : "false");
  dom.hostGroup.title = reason || "Group services by host";
}

async function setGroupBy(mode) {
  if (mode === "host" && !hostsAvailable()) {
    // Hosts may have appeared since the last explicit refresh; ask once
    // before refusing, so the toggle never needs a page reload to wake up.
    try {
      const hosts = await fetchJSON("/api/hosts");
      state.hosts = Array.isArray(hosts) ? hosts.filter((host) => host && typeof host.name === "string") : [];
      state.hostsError = "";
    } catch (error) {
      if (state.hosts === null) state.hostsError = error && error.message ? error.message : "Unknown response";
    }
    if (!hostsAvailable()) {
      renderViewSwitch();
      showToast(hostGroupReason());
      return;
    }
  }
  state.groupBy = mode;
  state.overview = false;
  if (mode === "host") state.scope = "all";
  state.inventory = false;
  state.pendingFit = true;
  updateMapURL();
  updateURL({ group: mode === "host" ? "host" : null });
  if (mode === "host" && isMobile()) {
    state.mobileMode = "list";
    state.mobileModeChosen = true;
    updateURL({ flow: "0" });
  }
  renderAll();
  showToast(mode === "host" ? "Grouped by host: " + state.hosts.length + (state.hosts.length === 1 ? " host" : " hosts") : "Grouped by service");
}

function isMobile() {
  return window.matchMedia("(max-width: 767px)").matches;
}

function chooseInitialMobileMode() {
  if (state.mobileModeChosen || !state.graph) return;
  const requested = new URLSearchParams(window.location.search).get("flow");
  if (requested === "1") state.mobileMode = "map";
  else if (requested === "0" || state.groupBy === "host") state.mobileMode = "list";
  else state.mobileMode = state.graph.nodes.length > 40 ? "list" : "map";
  state.mobileModeChosen = true;
}

function setMobileMode(mode) {
  state.mobileMode = mode;
  state.mobileModeChosen = true;
  updateURL({ flow: mode === "map" ? "1" : "0" });
  renderViewSwitch();
  renderStates();
  if (mode === "map") { state.pendingFit = true; renderGraph(); }
}

function statCard(label, value, critical, explanation) {
  const card = document.createElement("div");
  card.className = "stat-card" + (critical ? " is-critical" : "");
  if (explanation) card.title = explanation;
  card.append(textElement("span", "stat-label", label), textElement("strong", "", value));
  return card;
}

function renderOverview(node) {
  const wrapper = document.createElement("div");
  const metrics = node.metrics || {};
  const stats = document.createElement("div");
  stats.className = "stat-grid";
  const tail = formatP99(metrics.p99_latency_ms, metrics.latency_provenance);
  stats.append(
    statCard("RPS", formatCount(metrics.request_rate_rps) + "/s"),
    statCard("Error", formatRatio(metrics.error_rate, 1), finite(metrics.error_rate, 0) > 0),
    statCard("Average", formatMs(metrics.avg_latency_ms)),
    statCard(tail.label, tail.value, false, tail.explanation)
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

  const hosts = nodeHosts(node);
  if (hosts.length) {
    const hostSection = document.createElement("section");
    hostSection.className = "section";
    const count = nodeHostCount(node);
    hostSection.appendChild(textElement("h3", "section-title", "Hosts · " + count));
    const row = document.createElement("div");
    row.className = "chip-row";
    for (const host of hosts) {
      const chip = textElement("button", "host-chip", host);
      chip.type = "button";
      chip.dataset.host = host;
      chip.setAttribute("aria-label", "Open host " + host);
      chip.addEventListener("click", () => openHost(host));
      row.appendChild(chip);
    }
    if (count > hosts.length) row.appendChild(textElement("span", "quiet", "+" + (count - hosts.length) + " more"));
    hostSection.appendChild(row);
    wrapper.appendChild(hostSection);
  }
  return wrapper;
}

function sparkline(values) {
  const svg = svgElement("svg", { class: "spark", viewBox: "0 0 100 28", "aria-hidden": "true" });
  let min = Infinity;
  let max = -Infinity;
  for (const value of values) {
    min = Math.min(min, value);
    max = Math.max(max, value);
  }
  const span = max - min || 1;
  const points = values.map((value, index) => {
    const x = values.length === 1 ? 50 : index / (values.length - 1) * 100;
    return x.toFixed(1) + "," + (26 - (value - min) / span * 24).toFixed(1);
  });
  svg.appendChild(svgElement("polyline", { points: points.join(" ") }));
  // A lone sample is a point, not a line; make it visible.
  const last = points[points.length - 1].split(",");
  svg.appendChild(svgElement("circle", { cx: last[0], cy: last[1], r: 2 }));
  return svg;
}

function bucketValue(bucket) {
  const count = finite(bucket.count, 0);
  return count > 0 ? finite(bucket.sum, 0) / count : finite(bucket.max, NaN);
}

function hostMetricCard(spec, buckets) {
  const card = document.createElement("div");
  card.className = "stat-card";
  card.dataset.metric = spec.name;
  card.append(textElement("span", "stat-label", spec.label));
  const values = (buckets || []).map(bucketValue).filter(Number.isFinite);
  if (!buckets) {
    card.setAttribute("aria-busy", "true");
    card.append(textElement("strong", "", "…"));
    return card;
  }
  if (!values.length) {
    card.classList.add("is-empty");
    card.append(textElement("strong", "", "not reported"));
    card.title = spec.name + " has no samples in the last hour";
    return card;
  }
  const latest = values[values.length - 1];
  card.append(textElement("strong", "", spec.kind === "ratio" ? formatRatio(latest, 0) : formatBytes(latest)));
  card.appendChild(sparkline(values));
  card.title = spec.name + ": " + values.length + (values.length === 1 ? " bucket" : " buckets") + " in the last hour";
  return card;
}

function renderHostPanel(host) {
  const wrapper = document.createElement("div");
  const entry = state.hostMetrics.get(state.selectedHost) || null;
  const resources = document.createElement("section");
  resources.className = "section";
  const head = document.createElement("div");
  head.className = "section-head";
  head.appendChild(textElement("h3", "section-title", "Resources · last hour"));
  if (entry && entry.coverage) {
    const badge = textElement("span", "coverage-tag", entry.coverage + " coverage");
    badge.title = "Reported by the metrics API";
    head.appendChild(badge);
  }
  resources.appendChild(head);
  const grid = document.createElement("div");
  grid.className = "stat-grid resource-grid";
  for (const spec of HOST_METRICS) {
    const buckets = entry ? entry.series.get(spec.name) : null;
    if (!spec.always && !(buckets && buckets.length)) continue;
    grid.appendChild(hostMetricCard(spec, buckets === undefined ? null : buckets));
  }
  resources.appendChild(grid);
  if (entry && entry.error) resources.appendChild(textElement("p", "quiet", "Metrics unavailable: " + entry.error));
  wrapper.appendChild(resources);

  const services = document.createElement("section");
  services.className = "section";
  const listed = host && Array.isArray(host.services) ? host.services : [];
  const total = host ? finite(host.service_count, listed.length) : 0;
  services.appendChild(textElement("h3", "section-title", "Services · " + total));
  if (!host) {
    services.appendChild(textElement("p", "quiet", state.selectedHost + " is not in the current host list. It may have stopped reporting."));
  } else if (!listed.length) {
    services.appendChild(textElement("p", "quiet", "No services reported on this host."));
  } else {
    const list = document.createElement("ul");
    list.className = "dependency-list";
    for (const id of listed) {
      const node = state.graph ? state.graph.nodes.find((candidate) => candidate.id === id) : null;
      const item = document.createElement("li");
      const button = document.createElement("button");
      button.type = "button";
      button.className = "dependency-row";
      button.dataset.service = id;
      button.append(
        textElement("strong", "", id),
        textElement("span", "", node ? normalizeStatus(node) + " · health " + formatRatio(node.health_score, 0) : "not in the current graph")
      );
      button.addEventListener("click", () => openInspector(id));
      item.appendChild(button);
      list.appendChild(item);
    }
    services.appendChild(list);
    if (total > listed.length) services.appendChild(textElement("p", "quiet", "+" + (total - listed.length) + " more not listed"));
  }
  wrapper.appendChild(services);
  if (host) {
    const seen = new Date(host.last_seen);
    const signals = Array.isArray(host.signals) && host.signals.length ? host.signals.join(", ") : "none";
    wrapper.appendChild(textElement("p", "quiet", "Signals: " + signals + (Number.isNaN(seen.getTime()) ? "" : " · last seen " + seen.toLocaleString())));
  }
  return wrapper;
}

async function fetchSeries(path) {
  const response = await fetch(path, { headers: { Accept: "application/json" }, cache: "no-cache" });
  if (!response.ok) throw new Error((await response.text()).trim() || "HTTP " + response.status);
  const data = await response.json();
  return { buckets: Array.isArray(data) ? data : [], coverage: response.headers.get("OtelContext-Data-Coverage") || "" };
}

// loadHostMetrics reads the host's hostmetrics series through the ordinary
// metrics API. Previous samples stay on screen while a refresh is in flight.
async function loadHostMetrics(candidate) {
  const host = validHostName(candidate);
  if (!host) return;
  const previous = state.hostMetrics.get(host);
  const entry = {
    series: previous ? previous.series : new Map(),
    coverage: previous ? previous.coverage : "",
    error: "",
    loadedAt: Date.now(),
  };
  state.hostMetrics.set(host, entry);
  const end = new Date();
  const start = new Date(end.getTime() - HOST_METRICS_WINDOW_MS);
  const query = "&service_name=" + encodeURIComponent(HOST_PREFIX + host)
    + "&start=" + encodeURIComponent(start.toISOString()) + "&end=" + encodeURIComponent(end.toISOString());
  const results = await Promise.allSettled(HOST_METRICS.map((spec) => fetchSeries("/api/metrics?name=" + encodeURIComponent(spec.name) + query)));
  const series = new Map();
  let coverage = "";
  let error = "";
  results.forEach((result, index) => {
    if (result.status === "fulfilled") {
      series.set(HOST_METRICS[index].name, result.value.buckets);
      if (result.value.coverage && coverage !== "sampled") coverage = result.value.coverage;
    } else {
      series.set(HOST_METRICS[index].name, []);
      error = result.reason && result.reason.message ? result.reason.message : "request failed";
    }
  });
  if (state.hostMetrics.get(host) !== entry) return;
  entry.series = series;
  entry.coverage = coverage;
  entry.error = error;
  if (state.selectedHost === host) renderInspector();
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
  const owner = state.partition && state.partition.components.find((item) => item.nodeIds.includes(service));
  if (owner) state.component = owner.id;
  state.scope = "all";
  state.inventory = !owner;
  state.pendingFit = true;
  state.mobileMode = "map";
  state.mobileModeChosen = true;
  updateMapURL();
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

function renderConnection(edge) {
  const wrapper = document.createElement("div");
  wrapper.className = "connection-details";
  if (!edge) {
    wrapper.appendChild(textElement("p", "quiet", "This connection is no longer in the current graph."));
    return wrapper;
  }
  const endpoints = document.createElement("div");
  endpoints.className = "section";
  for (const [label, id] of [["Caller", edge.source], ["Dependency", edge.target]]) {
    endpoints.appendChild(textElement("h3", "section-title", label));
    const button = textElement("button", "dependency-row", id);
    button.type = "button";
    button.addEventListener("click", () => openInspector(id));
    endpoints.appendChild(button);
  }
  const stats = document.createElement("div");
  stats.className = "stat-grid";
  const measured = Number(edge.call_count) > 0;
  stats.append(statCard("Observed calls", formatCount(edge.call_count), false),
    statCard("Average latency", measured ? formatMs(edge.avg_latency_ms) : "Not reported", false),
    statCard("Errors", measured ? formatRatio(edge.error_rate, 1) : "Not reported", measured && Number(edge.error_rate) > 0.05));
  wrapper.append(endpoints, stats, textElement("p", "quiet", "Observed call evidence from the current graph. Counts are not a request rate."));
  return wrapper;
}

function openConnection(edge) {
  rememberMap();
  state.overview = state.inventory = false;
  state.pendingFit = true;
  state.selectedEdge = connectionKey(edge);
  state.selected = null;
  state.selectedHost = null;
  updateURL({ service: null, host: null, tab: null });
  renderInspector();
  renderGraph();
  renderServiceLists();
  window.setTimeout(() => dom.closeInspector.focus(), 0);
}

function renderInspector() {
  const node = currentNode();
  document.body.classList.toggle("has-inspector", Boolean(state.selected || state.selectedHost || state.selectedEdge));
  if (!state.selected && !state.selectedHost && !state.selectedEdge) {
    dom.inspector.setAttribute("aria-hidden", "true");
    dom.inspector.inert = true;
    dom.inspectorScrim.hidden = true;
    return;
  }
  dom.inspector.setAttribute("aria-hidden", "false");
  dom.inspector.inert = false;
  dom.inspectorScrim.hidden = !isMobile();
  dom.inspectorTabs.hidden = Boolean(state.selectedHost || state.selectedEdge);
  if (state.selectedEdge) {
    const edge = state.graph && state.graph.edges.find((item) => connectionKey(item) === state.selectedEdge);
    dom.inspectorEyebrow.textContent = "Connection details";
    dom.inspectorTitle.textContent = edge ? edge.source + " → " + edge.target : "Connection";
    dom.inspectorStatus.className = "service-status " + (edge && Number(edge.error_rate) > 0.05 ? "critical" : "unknown");
    dom.inspectorHealth.textContent = "";
    dom.inspectorBody.replaceChildren(renderConnection(edge));
    return;
  }
  dom.inspectorEyebrow.textContent = state.selectedHost ? "Host inspector" : "Service inspector";
  if (state.selectedHost) {
    const host = hostByName(state.selectedHost);
    dom.inspectorTitle.textContent = state.selectedHost;
    dom.inspectorStatus.className = "service-status host";
    dom.inspectorHealth.textContent = host ? formatCount(host.service_count) + (finite(host.service_count, 0) === 1 ? " service" : " services") : "Not reporting";
    dom.inspectorHealth.style.color = host ? "var(--muted)" : "var(--faint)";
    dom.inspectorBody.replaceChildren(renderHostPanel(host));
    return;
  }
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
  rememberMap();
  state.overview = false;
  const owner = state.partition && state.partition.components.find((item) => item.nodeIds.includes(service));
  if (state.groupBy !== "host") {
    if (owner) {
      state.inventory = false;
      if (state.component !== owner.id) {
        state.component = owner.id;
        state.pendingFit = true;
      }
      if (state.scope !== "all" && state.focusRoot !== service && (state.selectedEdge || !dom.nodes.querySelector('[data-service="' + CSS.escape(service) + '"]'))) {
        state.focusRoot = service;
        state.pendingFit = true;
      }
    } else if (state.partition && state.partition.disconnected.some((node) => node.id === service)) {
      state.inventory = true;
    }
  }
  state.selected = service;
  state.selectedHost = null;
  state.selectedEdge = null;
  updateURL({ service, host: null, tab: state.activeTab === "overview" ? null : state.activeTab });
  updateMapURL();
  renderInspector();
  renderGraph();
  renderServiceLists();
  window.setTimeout(() => dom.closeInspector.focus(), 0);
}

function openHost(candidate) {
  const host = validHostName(candidate);
  if (!host) return;
  state.selected = null;
  state.selectedHost = host;
  state.selectedEdge = null;
  updateURL({ host: host, service: null, tab: null });
  renderInspector();
  renderGraph();
  renderServiceLists();
  renderInspector();
  loadHostMetrics(host);
  showToast("Host " + host + " opened.");
  window.setTimeout(() => dom.closeInspector.focus(), 0);
}

function closeInspector() {
  const selected = state.selected;
  const selectedHost = state.selectedHost;
  const selectedEdge = state.selectedEdge;
  state.selectedEdge = null;
  state.selected = null;
  state.selectedHost = null;
  updateURL({ service: null, host: null, tab: null });
  renderInspector();
  renderGraph();
  renderServiceLists();
  renderInspector();
  const selector = selected ? "[data-service=" + CSS.escape(selected) + "]" : selectedHost ? "[data-host=" + CSS.escape(selectedHost) + "]" : selectedEdge ? '[data-edge="' + CSS.escape(selectedEdge) + '"]' : "";
  const source = selector ? document.querySelector(selector) : null;
  if (source) source.focus();
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
  const focused = document.activeElement;
  const focusList = focused?.closest("#neighborhood-list, #overview-grid, #map-member-list, #map-connection-list, #service-list, #disconnected-list, #mobile-list");
  const focusAttributes = focusList ? ["data-service", "data-neighborhood", "data-source", "data-target"]
    .filter((name) => focused.hasAttribute(name))
    .map((name) => "[" + name + "=\"" + CSS.escape(focused.getAttribute(name)) + "\"]").join("") : "";
  // Host mode cannot outlive its data: once the host list is known to be
  // empty or unreachable, fall back to the service view and say why.
  if (state.groupBy === "host" && !hostsAvailable() && (state.hosts !== null || state.hostsError)) {
    state.groupBy = "service";
    updateURL({ group: null });
    showToast("Host grouping turned off: " + hostGroupReason());
  }
  renderStates();
  renderPulse();
  renderSeverity();
  renderServiceLists();
  renderInspector();
  renderGraph();
  renderImpactBanner();
  renderViewSwitch();
  renderInspector();
  if (focusList && focusAttributes && !focused.isConnected) {
    const replacement = document.querySelector("#" + focusList.id + " " + focusAttributes);
    if (replacement?.getClientRects().length) replacement.focus({ preventScroll: true });
  }
}

function setViewBox(next) {
  const rect = dom.map.getBoundingClientRect();
  const aspect = rect.width && rect.height ? rect.width / rect.height : next.width / next.height;
  const bounds = state.layout ? state.layout.bounds : { width: 1000, height: 1000 };
  const maxWidth = state.groupBy === "host" ? Math.max(1400, bounds.width * 2, bounds.height * aspect * 2) : rect.width;
  const width = Math.max(260, Math.min(maxWidth, next.width));
  const height = width / aspect;
  state.viewBox = { x: next.x, y: next.y, width, height };
  dom.map.setAttribute("viewBox", [next.x, next.y, width, height].join(" "));
  renderMinimapViewport();
}

function fitGraph() {
  if (!state.layout) return;
  const rect = dom.map.getBoundingClientRect();
  if (!rect.width || !rect.height) { state.pendingFit = true; return; }
  const bounds = state.layout.bounds;
  const aspect = rect.width / rect.height;
  const fittedWidth = Math.max(bounds.width, bounds.height * aspect) * 1.06;
  // Full-system coverage belongs to the atlas. Keep names and edge evidence
  // at their CSS size; pan a larger neighborhood instead of shrinking text.
  const width = Math.min(fittedWidth, rect.width);
  const height = width / aspect;
  const center = !state.selectedEdge && state.layout.positions.get(state.selected || state.focusRoot);
  const origin = (start, size, viewport, focus) => size <= viewport || !center
    ? start + (size - viewport) / 2
    : Math.max(start, Math.min(start + size - viewport, focus - viewport / 2));
  setViewBox({ x: origin(bounds.x, bounds.width, width, center?.x),
    y: origin(bounds.y, bounds.height, height, center?.y), width, height });
}

function renderMinimapViewport() {
  dom.minimapViewport.setAttribute("x", String(state.viewBox.x));
  dom.minimapViewport.setAttribute("y", String(state.viewBox.y));
  dom.minimapViewport.setAttribute("width", String(state.viewBox.width));
  dom.minimapViewport.setAttribute("height", String(state.viewBox.height));
}

function svgPoint(svg, clientX, clientY) {
  const transform = svg.getScreenCTM();
  return transform ? new DOMPoint(clientX, clientY).matrixTransform(transform.inverse()) : null;
}

function zoomAt(factor, clientX, clientY) {
  const rect = dom.map.getBoundingClientRect();
  if (!rect.width || !rect.height) return;
  const view = state.viewBox;
  const point = svgPoint(dom.map, clientX === undefined ? rect.left + rect.width / 2 : clientX,
    clientY === undefined ? rect.top + rect.height / 2 : clientY);
  if (!point) return;
  const width = Math.max(260, view.width * factor);
  const ratio = width / view.width;
  setViewBox({ x: point.x - (point.x - view.x) * ratio,
    y: point.y - (point.y - view.y) * ratio, width, height: view.height * ratio });
}

let pointerPan = null;
dom.map.addEventListener("pointerdown", (event) => {
  if (event.button !== 0 || event.target.closest(".service-node, .graph-edge, .edge-hit")) return;
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
  const entropy = new Uint32Array(1);
  window.crypto.getRandomValues(entropy);
  const jitter = entropy[0] / 0x100000000 * delay * 0.2;
  wsReconnectTimer = window.setTimeout(connectWebSocket, delay + jitter);
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
  dom.overviewButton.addEventListener("click", showSystemOverview);
  dom.mapBack.addEventListener("click", goBackOnMap);
  dom.neighborhoodIndex.addEventListener("click", () => {
    state.indexMode = "neighborhoods";
    state.query = dom.search.value = "";
    renderServiceLists();
  });
  dom.serviceIndex.addEventListener("click", () => { state.indexMode = "services"; renderServiceLists(); });
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
  dom.hostGroup.addEventListener("click", () => setGroupBy(state.groupBy === "host" ? "service" : "host"));
  dom.zoomIn.addEventListener("click", () => zoomAt(0.8));
  dom.zoomOut.addEventListener("click", () => zoomAt(1.25));
  dom.fit.addEventListener("click", () => fitGraph());
  dom.minimapButton.addEventListener("click", (event) => {
    if (!event.detail) { fitGraph(); return; }
    const center = svgPoint(dom.minimap, event.clientX, event.clientY);
    if (!center) return;
    setViewBox({ x: center.x - state.viewBox.width / 2, y: center.y - state.viewBox.height / 2,
      width: state.viewBox.width, height: state.viewBox.height });
  });
  dom.componentSelect.addEventListener("change", () => {
    if (dom.componentSelect.value === "all") showSystemOverview();
    else openNeighborhood(dom.componentSelect.value);
  });
  dom.scopeSelect.addEventListener("change", () => {
    rememberMap();
    state.selectedEdge = null;
    state.scope = dom.scopeSelect.value;
    state.initializedComponents.add(state.groupBy + ":" + state.component);
    state.focusRoot = state.selected || state.focusRoot;
    state.inventory = false;
    state.pendingFit = true;
    renderInspector();
    renderGraph();
    updateMapURL();
  });
  dom.resetScope.addEventListener("click", () => {
    rememberMap();
    state.selectedEdge = null;
    state.scope = "all";
    state.initializedComponents.add(state.groupBy + ":" + state.component);
    state.inventory = false;
    state.pendingFit = true;
    renderInspector();
    renderGraph();
    updateMapURL();
  });
  dom.disconnectedButton.addEventListener("click", () => {
    rememberMap();
    state.overview = false;
    state.inventory = !state.inventory;
    state.pendingFit = true;
    closeInspector();
    updateMapURL();
    renderGraph();
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
      fitGraph();
      return;
    }
    if (event.key.toLowerCase() === "h" && !editable && !event.ctrlKey && !event.metaKey && !event.altKey) {
      event.preventDefault();
      setGroupBy(state.groupBy === "host" ? "service" : "host");
      return;
    }
    if (event.key === "Escape" && (state.selected || state.selectedHost || state.selectedEdge)) {
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
    if (state.selectedHost) loadHostMetrics(state.selectedHost);
  });
  let resizeFit = false;
  let resizeTimer;
  const queueResizeFit = () => {
    if (!resizeFit) return;
    window.clearTimeout(resizeTimer);
    // Media queries can change the grid in a second layout notification.
    // Fit once those notifications settle, without refitting on selection.
    resizeTimer = window.setTimeout(() => { resizeFit = false; fitGraph(); }, 50);
  };
  const mapResize = new ResizeObserver(() => {
    if (resizeFit) { queueResizeFit(); return; }
    if (!state.layout || dom.canvas.hidden || state.groupBy === "host") return;
    const rect = dom.map.getBoundingClientRect();
    if (!rect.width || !rect.height) return;
    const view = state.viewBox;
    const width = Math.min(view.width, rect.width);
    const height = width * rect.height / rect.width;
    let x = view.x + (view.width - width) / 2;
    let y = view.y + (view.height - height) / 2;
    const selected = state.layout.positions.get(state.selected);
    if (selected) {
      x = Math.min(selected.x - CARD_WIDTH / 2 - 16, Math.max(x, selected.x + CARD_WIDTH / 2 + 16 - width));
      y = Math.min(selected.y - CARD_HEIGHT / 2 - 16, Math.max(y, selected.y + CARD_HEIGHT / 2 + 16 - height));
    }
    // Docking an inspector or expanding evidence resizes the canvas without
    // resizing the window. Update both viewBox dimensions to keep text readable.
    setViewBox({ x, y, width, height });
  });
  mapResize.observe(dom.canvas);
  window.addEventListener("resize", () => {
    resizeFit = true;
    renderStates();
    renderViewSwitch();
    renderInspector();
    queueResizeFit();
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

export { formatP99, formatPulseLatency };

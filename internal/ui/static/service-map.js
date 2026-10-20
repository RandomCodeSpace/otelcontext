import { CARD_WIDTH, CARD_HEIGHT, layoutGraph } from "./map-layout.js";
import {
  $, element, svg, icon, TONE_ICON, TONE_LABEL, toneFromErrorRate, toneRank, toneIcon, badge,
  fmtNumber, fmtRate, fmtPercent, fmtMs, fmtAgo, sparkline, initShell,
} from "./shell.js";

const RANGES = { "15m": 15, "1h": 60, "6h": 360, "24h": 1440 };
const REFRESH_MS = 60000;
const HEX64 = /^[a-f0-9]{64}$/;
// Scene geometry in SVG user units, on the 4px grid.
const SW = 152, SH = 56, ROW_GAP = 12, COL_GAP = 28, COL_GAP_INST = 88;
const PW = 136, PH = 24, PILL_GAP = 4, GROUP_HEAD = 24, GROUP_PAD = 8, GROUP_GAP = 12;
const HW = 168, HOST_GAP = 12, HOST_ROW_GAP = 16, PIP = 8, PIP_ROW = 12;
const LANE_HEAD = 44, LANE_FOOT = 28, HINT_H = 32;
// Focused neighbourhoods sit in one tier: each service above its own instances.
const TW = 136, TGAP = 36, TPW = 128, TIER_MAX = 9, GUTTER_STEP = 6;
const RAIL_KEY = "oc-map-rail";
const FONT = { name: "500 13px Geist", sub: "11px Geist", mono: "11px 'Geist Mono'", small: "10px 'Geist Mono'" };
const TONES = ["ok", "warn", "crit", "unk"];

const state = {
  range: "15m", env: undefined, mode: "services", focus: "", focusName: "", hops: 1, namespace: "", query: "",
  layers: { services: true, instances: true, hosts: true },
  catalog: null, svc: null, inst: null, focusGraph: null, hosts: [],
  start: "", end: "", updated: 0, loading: false, loadingFull: false, failure: null, refreshFailed: false, notice: "",
  selected: null, hover: null, tab: "overview",
  scene: null, sceneKey: "", view: { x: 0, y: 0, k: 1 },
  gen: 0, controller: null, traffic: new Map(), trafficController: null,
  drawer: null, opener: null, railCollapsed: false,
};

const canvas = $("canvas");
const map = $("map");
const mqInspector = matchMedia("(max-width: 1279px)");
const mqRail = matchMedia("(max-width: 1023px)");
const mqCompact = matchMedia("(max-width: 560px)");

const unique = (items) => [...new Set(items)];
const clamp = (n, lo, hi) => Math.min(hi, Math.max(lo, n));
const finite = (n) => typeof n === "number" && Number.isFinite(n);
const toneOf = (node) => toneFromErrorRate(node?.error_rate, node?.requests);
const worst = (tones) => tones.filter((t) => t !== "unk").sort((a, b) => toneRank(a) - toneRank(b))[0] || "unk";
const envLabel = (env) => env === undefined ? "All environments" : env || "Unknown environment";
const nsLabel = (ns) => ns || "No namespace";
// Errors first, then error ratio, then volume: "which connection hurts most".
const byErrorContribution = (a, b) => (b.errors || 0) - (a.errors || 0) || (b.error_rate || 0) - (a.error_rate || 0) || (b.calls || 0) - (a.calls || 0);

// ---------- Reads ----------

const delay = (ms, signal) => new Promise((resolve, reject) => {
  const timer = setTimeout(resolve, ms);
  signal.addEventListener("abort", () => { clearTimeout(timer); reject(signal.reason); }, { once: true });
});

// fetchJSON in shell.js does not expose Retry-After, which topology reads need on 429.
// 502/504 are retried too: the server's 5s topology deadline can lapse under writer contention.
async function getJSON(path, signal) {
  for (let busy = 0, flaky = 0; ;) {
    const response = await fetch(path, {
      signal: AbortSignal.any([signal, AbortSignal.timeout(10000)]),
      cache: "no-store", credentials: "same-origin", headers: { Accept: "application/json" },
    });
    if (response.status === 429 && busy < 3) {
      busy++;
      await delay(clamp(Number(response.headers.get("Retry-After")) || 1, 1, 10) * 1000, signal);
      continue;
    }
    if ((response.status === 502 || response.status === 504) && flaky < 2) {
      flaky++;
      await delay(flaky * 1000, signal);
      continue;
    }
    if (!response.ok) {
      const error = new Error((await response.text()).slice(0, 300).trim() || "HTTP " + response.status);
      error.status = response.status;
      throw error;
    }
    return response.json();
  }
}

function validNodes(nodes, cap) {
  return Array.isArray(nodes) && nodes.length <= cap && nodes.every((n) =>
    n && HEX64.test(n.id) && HEX64.test(n.service_id) && typeof n.name === "string" && n.name.length <= 255 &&
    typeof n.environment === "string" && Array.isArray(n.hosts) && n.hosts.length <= 20 &&
    n.hosts.every((h) => typeof h === "string" && h.length <= 255));
}

function normalizeNodes(nodes) {
  for (const n of nodes) {
    n.namespace = typeof n.namespace === "string" ? n.namespace : "";
    n.instance = typeof n.instance === "string" ? n.instance : "";
    n.instances = Array.isArray(n.instances) ? n.instances : [];
  }
  return nodes;
}

function checkGraph(graph) {
  if (!graph || !validNodes(graph.nodes, 100) || !Array.isArray(graph.edges) || graph.edges.length > 200 ||
      !graph.meta || typeof graph.meta !== "object") throw new Error("Invalid topology graph response.");
  const ids = new Set(graph.nodes.map((n) => n.id));
  if (graph.edges.some((e) => !e || typeof e.id !== "string" || !ids.has(e.source) || !ids.has(e.target))) {
    throw new Error("Topology graph contains an unresolved endpoint.");
  }
  normalizeNodes(graph.nodes);
  return graph;
}

function checkCatalog(catalog) {
  if (!catalog || !validNodes(catalog.services, 1000) || !Array.isArray(catalog.environments) ||
      !catalog.environments.every((e) => typeof e === "string" && e.length <= 255)) {
    throw new Error("Invalid topology catalog response.");
  }
  normalizeNodes(catalog.services);
  return catalog;
}

const checkHosts = (hosts) => Array.isArray(hosts) ? hosts.filter((h) => h && typeof h.name === "string" &&
  h.name.length <= 255 && Array.isArray(h.services) && h.services.every((s) => typeof s === "string")).slice(0, 2000) : [];

function setRange() {
  const end = new Date();
  state.end = end.toISOString();
  state.start = new Date(end.getTime() - RANGES[state.range] * 60000).toISOString();
}

function graphURL(params) {
  const query = new URLSearchParams({ start: state.start, end: state.end, limit: "100", ...params });
  if (state.env !== undefined) query.set("environment", state.env);
  return "/api/topology/graph?" + query;
}

async function load({ full = true, refit = true } = {}) {
  const gen = ++state.gen;
  state.controller?.abort();
  const controller = new AbortController();
  state.controller = controller;
  const { signal } = controller;
  if (full || !state.start) setRange();
  state.notice = "";
  state.loading = true;
  state.loadingFull = full;
  if (full) state.traffic.clear();
  renderStatus();
  if (!state.scene) showState("loading");
  canvas.setAttribute("aria-busy", "true");
  try {
    if (full) {
      const range = new URLSearchParams({ start: state.start, end: state.end });
      const [catalog, svcGraph, instGraph, hosts] = await Promise.all([
        getJSON("/api/topology/catalog?" + range, signal),
        getJSON(graphURL({ mode: "services" }), signal),
        getJSON(graphURL({ mode: "instances" }), signal),
        getJSON("/api/hosts", signal).catch((error) => {
          if (signal.aborted) throw error;
          return [];
        }),
      ]);
      state.catalog = checkCatalog(catalog);
      state.svc = checkGraph(svcGraph);
      state.inst = checkGraph(instGraph);
      state.hosts = checkHosts(hosts);
    }
    resolveFocusName();
    state.focusGraph = state.focus ? checkGraph(await getJSON(graphURL({ mode: "instances", service_id: state.focus, hops: String(state.hops) }), signal)) : null;
    if (state.focusGraph && !state.focusGraph.nodes.length && !state.svc.nodes.some((n) => n.service_id === state.focus)) {
      state.notice = "That service has no topology in this range and environment.";
      state.focus = "";
      state.focusGraph = null;
    }
    if (gen !== state.gen) return;
    state.failure = null;
    state.refreshFailed = false;
    if (full) state.updated = Date.now();
  } catch (error) {
    if (gen !== state.gen || error.name === "AbortError") return;
    const failure = error.name === "TimeoutError" ? new Error("The topology request timed out.") : error;
    if (!full && failure.status !== 503) {
      // Keep the last good map; a half-focused view would mislead.
      state.notice = "The neighbourhood could not load: " + failure.message;
      state.focus = "";
      state.focusGraph = null;
    } else if (state.svc && failure.status !== 503) state.refreshFailed = true;
    else state.failure = failure;
  } finally {
    if (gen === state.gen) {
      state.loading = false;
      canvas.removeAttribute("aria-busy");
      renderAll({ refit });
    }
  }
}

function resolveFocusName() {
  if (!state.focusName || !state.catalog) return;
  const matches = state.catalog.services.filter((s) => s.name === state.focusName);
  const match = matches.find((s) => state.env === undefined || s.environment === state.env) || matches[0];
  if (match) state.focus = match.service_id;
  else state.notice = "No service named “" + state.focusName + "” in this range.";
  state.focusName = "";
}

// ---------- URL state ----------

function readURL() {
  const params = new URLSearchParams(location.search);
  if (RANGES[params.get("range")]) state.range = params.get("range");
  if (params.get("mode") === "instances") state.mode = "instances";
  if (params.get("hops") === "2") state.hops = 2;
  state.namespace = (params.get("namespace") || "").slice(0, 255);
  if (params.has("env")) {
    try {
      const env = JSON.parse(params.get("env"));
      if (typeof env === "string" && env.length <= 255) state.env = env;
    } catch { /* ignore a malformed shared link */ }
  }
  const service = params.get("service") || "";
  if (HEX64.test(service)) state.focus = service;
  else state.focusName = (params.get("service_name") || "").slice(0, 255);
  if ((state.focus || state.focusName) && !params.has("mode")) state.mode = "instances";
}

function writeURL() {
  const params = new URLSearchParams();
  if (state.focus) params.set("service", state.focus);
  if (state.namespace && !state.focus) params.set("namespace", state.namespace);
  if (state.mode !== "services") params.set("mode", state.mode);
  if (state.env !== undefined) params.set("env", JSON.stringify(state.env));
  if (state.range !== "15m") params.set("range", state.range);
  if (state.hops !== 1) params.set("hops", String(state.hops));
  const query = params.toString();
  history.replaceState(null, "", location.pathname + (query ? "?" + query : ""));
}

// ---------- Scope ----------

function serviceFromInstances(serviceId, nodes) {
  const members = nodes.filter((n) => n.service_id === serviceId);
  if (!members.length) return null;
  const requests = members.reduce((sum, n) => sum + (n.requests || 0), 0);
  const errors = members.reduce((sum, n) => sum + (n.errors || 0), 0);
  const weighted = members.reduce((sum, n) => sum + (n.avg_latency_ms || 0) * (n.requests || 0), 0);
  return {
    ...members[0], id: serviceId, instance: "", identity: "service", instances: members.map((n) => n.id),
    instance_count: members.length, unresolved_count: 0, hosts: unique(members.flatMap((n) => n.hosts)),
    requests, errors, request_rate_rps: members.reduce((sum, n) => sum + (n.request_rate_rps || 0), 0),
    avg_latency_ms: requests ? weighted / requests : 0, error_rate: requests ? errors / requests : 0,
  };
}

// Service-level edges keyed by service_id, from the services read or folded from instance calls.
function serviceEdges(ids, instNodes, instEdges) {
  if (state.svc) {
    const serviceOf = new Map(state.svc.nodes.map((n) => [n.id, n.service_id]));
    return state.svc.edges.map((e) => ({ ...e, source: serviceOf.get(e.source), target: serviceOf.get(e.target) }))
      .filter((e) => ids.has(e.source) && ids.has(e.target));
  }
  const serviceOf = new Map(instNodes.map((n) => [n.id, n.service_id]));
  const folded = new Map();
  for (const e of instEdges) {
    const key = serviceOf.get(e.source) + ">" + serviceOf.get(e.target);
    const edge = folded.get(key) || { id: key, source: serviceOf.get(e.source), target: serviceOf.get(e.target), calls: 0, errors: 0, request_rate_rps: 0, duration: 0 };
    edge.calls += e.calls || 0;
    edge.errors += e.errors || 0;
    edge.request_rate_rps += e.request_rate_rps || 0;
    edge.duration += (e.avg_latency_ms || 0) * (e.calls || 0);
    folded.set(key, edge);
  }
  return [...folded.values()].map((e) => ({ ...e, avg_latency_ms: e.calls ? e.duration / e.calls : 0, error_rate: e.calls ? e.errors / e.calls : 0 }));
}

function scope() {
  const svcNodes = state.svc?.nodes || [];
  const svcById = new Map(svcNodes.map((n) => [n.service_id, n]));
  const focused = Boolean(state.focus && state.focusGraph);
  let services, instNodes = [], instEdges = [], meta = state.svc?.meta;
  if (focused) {
    const graph = state.focusGraph;
    const ids = unique([state.focus, ...graph.nodes.map((n) => n.service_id)]);
    services = ids.map((id) => svcById.get(id) || serviceFromInstances(id, graph.nodes)).filter(Boolean);
    instNodes = graph.nodes;
    instEdges = graph.edges;
    if (state.mode === "instances") meta = graph.meta;
  } else {
    services = svcNodes.filter((n) => !state.namespace || n.namespace === state.namespace);
    const ids = new Set(services.map((s) => s.service_id));
    instNodes = (state.inst?.nodes || []).filter((n) => ids.has(n.service_id));
    const instIds = new Set(instNodes.map((n) => n.id));
    instEdges = (state.inst?.edges || []).filter((e) => instIds.has(e.source) && instIds.has(e.target));
    if (state.mode === "instances") meta = state.inst?.meta || meta;
  }
  const ids = new Set(services.map((s) => s.service_id));
  return {
    focused, services, instNodes, instEdges, meta,
    svcEdges: serviceEdges(ids, instNodes, instEdges),
    showInstances: state.mode === "instances",
  };
}

// ---------- Text fitting ----------

const ruler = document.createElement("canvas").getContext("2d");
function fit(text, width, font) {
  ruler.font = font;
  if (ruler.measureText(text).width <= width) return text;
  let lo = 0, hi = text.length;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (ruler.measureText(text.slice(0, mid) + "…").width <= width) lo = mid;
    else hi = mid - 1;
  }
  return text.slice(0, lo) + "…";
}
const textWidth = (text, font) => { ruler.font = font; return ruler.measureText(text).width; };

function shortInstance(node) {
  const id = node.instance;
  if (!id) return node.identity === "host_workload" ? node.hosts[0] || "host workload" : "unresolved";
  return id.startsWith(node.name + "-") && id.length > node.name.length + 1 ? id.slice(node.name.length + 1) : id;
}

// ---------- Layout ----------

let layoutMemo = { key: "", value: null };

// Dagre owns ranks and crossing order. Each rank column is then re-packed:
// cards and the long-edge waypoints between them keep dagre's order and a
// quarter of its slack, so the lane stays dense enough to read when fitted.
function serviceLayout(services, edges, gap) {
  const key = gap + "|" + services.map((s) => s.service_id).sort().join() + "|" + edges.map((e) => e.source + ">" + e.target).sort().join();
  if (layoutMemo.key === key) return layoutMemo.value;
  const out = layoutGraph(services.map((s) => ({ id: s.service_id })), edges.map((e) => ({ source: e.source, target: e.target })),
    { ranker: "network-simplex", ranksep: 64, nodesep: 8, edgesep: 8, marginx: 0, marginy: 0 });
  const xs = unique([...out.positions.values()].map((p) => Math.round(p.x))).sort((a, b) => a - b);
  const column = (x) => xs.findIndex((c) => Math.abs(x - c) < 1);
  const center = (i) => i * (SW + gap) + SW / 2;
  const scaleY = (SH + ROW_GAP) / (CARD_HEIGHT + 8);
  const columns = xs.map(() => []);
  const cards = new Map();
  for (const [id, p] of out.positions) {
    const item = { orig: p.y * scaleY, h: SH };
    columns[column(p.x)].push(item);
    cards.set(id, { col: column(p.x), item });
  }
  const routes = new Map();
  for (const e of edges) {
    const raw = out.routes.get(e.source + ">" + e.target) || [];
    // Waypoints in card columns route long edges between cards; dagre's label slots are dropped.
    const via = raw.slice(1, -1).filter((p) => column(p.x) >= 0).map((p) => {
      const item = { orig: p.y * scaleY, h: 0 };
      columns[column(p.x)].push(item);
      return { col: column(p.x), item };
    });
    routes.set(e.source + ">" + e.target, via);
  }
  for (const list of columns) {
    list.sort((a, b) => a.orig - b.orig);
    list.forEach((item, i) => {
      if (!i) { item.y = item.orig; return; }
      const prev = list[i - 1];
      const min = prev.h / 2 + (prev.h && item.h ? ROW_GAP : 8) + item.h / 2;
      item.y = prev.y + min + Math.max(0, item.orig - prev.orig - min) / 4;
    });
    const shift = list.reduce((sum, item) => sum + item.orig - item.y, 0) / Math.max(1, list.length);
    for (const item of list) item.y += shift;
  }
  const top = Math.min(...columns.flat().map((item) => item.y - item.h / 2));
  const placed = new Map([...cards].map(([id, c]) => [id, { cx: center(c.col), cy: c.item.y - top, col: c.col }]));
  const placedRoutes = new Map([...routes].map(([k, via]) => [k, via.map((v) => ({ x: center(v.col), y: v.item.y - top }))]));
  const height = Math.max(SH, ...columns.flat().map((item) => item.y - top + Math.max(item.h / 2, 8)));
  const value = { cards: placed, routes: placedRoutes, columns: Math.max(1, xs.length), center, height, width: center(Math.max(0, xs.length - 1)) + SW / 2 };
  layoutMemo = { key, value };
  return value;
}

// Horizontal-tangent cubic through each waypoint: reads as a calm left-to-right flow.
function flow(points) {
  let d = "M" + points[0].x + " " + points[0].y;
  for (let i = 1; i < points.length; i++) {
    const a = points[i - 1], b = points[i];
    const dx = (b.x - a.x) / 2;
    d += ` C${a.x + dx} ${a.y} ${b.x - dx} ${b.y} ${b.x} ${b.y}`;
  }
  return d;
}

// Route between two boxes {x, y, w, h}: forward, backward, or same column (loop right).
function link(from, to, via = []) {
  const fy = from.y + from.h / 2, ty = to.y + to.h / 2;
  if (to.x >= from.x + from.w) {
    const mid = via.flatMap((p) => [{ x: p.x - SW / 2 - 4, y: p.y }, { x: p.x + SW / 2 + 4, y: p.y }]);
    return flow([{ x: from.x + from.w, y: fy }, ...mid, { x: to.x, y: ty }]);
  }
  if (to.x + to.w <= from.x) {
    const mid = via.flatMap((p) => [{ x: p.x + SW / 2 + 4, y: p.y }, { x: p.x - SW / 2 - 4, y: p.y }]);
    return flow([{ x: from.x, y: fy }, ...mid, { x: to.x + to.w, y: ty }]);
  }
  const sx = from.x + from.w, ex = to.x + to.w, bulge = Math.max(sx, ex) + 32 + Math.abs(ty - fy) / 4;
  return `M${sx} ${fy} C${bulge} ${fy} ${bulge} ${ty} ${ex} ${ty}`;
}

// One row in dagre rank order (callers, focus, callees). Edges between
// non-neighbouring cards arc over the row instead of crossing other cards.
function tierLayout(services, edges) {
  const base = serviceLayout(services, edges, COL_GAP);
  const order = [...services].sort((a, b) => {
    const p = base.cards.get(a.service_id), q = base.cards.get(b.service_id);
    return p.col - q.col || p.cy - q.cy;
  });
  const slot = new Map(order.map((s, i) => [s.service_id, i]));
  const center = (i) => i * (TW + TGAP) + TW / 2;
  const span = (e) => Math.abs(slot.get(e.target) - slot.get(e.source));
  const arcHeight = (e) => Math.min(72, 20 + 12 * (span(e) - 1));
  const arcs = edges.filter((e) => e.source !== e.target && slot.get(e.target) - slot.get(e.source) !== 1);
  const lift = arcs.length ? Math.max(...arcs.map(arcHeight)) + 8 : 0;
  const cards = new Map(order.map((s, i) => [s.service_id, { cx: center(i), cy: lift + SH / 2, col: i }]));
  return { tier: true, cards, routes: new Map(), slot, center, arcHeight, height: lift + SH, width: center(order.length - 1) + TW / 2 };
}

function arc(from, to, height) {
  const forward = to.x > from.x;
  const sx = forward ? from.x + from.w - 20 : from.x + 20, ex = forward ? to.x + 20 : to.x + to.w - 20;
  const lift = from.y - height * 4 / 3;
  return `M${sx} ${from.y} C${sx} ${lift} ${ex} ${lift} ${ex} ${to.y}`;
}

// Orthogonal polyline with rounded corners.
function orth(points, r = 6) {
  let d = `M${points[0].x} ${points[0].y}`;
  for (let i = 1; i < points.length - 1; i++) {
    const a = points[i - 1], c = points[i], n = points[i + 1];
    const r1 = Math.min(r, Math.hypot(c.x - a.x, c.y - a.y) / 2), r2 = Math.min(r, Math.hypot(n.x - c.x, n.y - c.y) / 2);
    d += ` L${c.x - Math.sign(c.x - a.x) * r1} ${c.y - Math.sign(c.y - a.y) * r1} Q${c.x} ${c.y} ${c.x + Math.sign(n.x - c.x) * r2} ${c.y + Math.sign(n.y - c.y) * r2}`;
  }
  const last = points[points.length - 1];
  return d + ` L${last.x} ${last.y}`;
}

// Calls between non-neighbouring groups leave through the gap beside the
// source, run along a gutter under every group, and climb into the target.
function gutterRoute(bundle, from, to) {
  const fy = from.y + from.h / 2, ty = to.y + to.h / 2;
  const fx = bundle.forward ? from.x + from.w : from.x, tx = bundle.forward ? to.x : to.x + to.w;
  return orth([{ x: fx, y: fy }, { x: bundle.outX, y: fy }, { x: bundle.outX, y: bundle.gy }, { x: bundle.inX, y: bundle.gy }, { x: bundle.inX, y: ty }, { x: tx, y: ty }]);
}

// Place hosts at the mean x of what runs on them, then resolve overlaps; wrap when too wide.
function placeHosts(hosts, width) {
  const step = HW + HOST_GAP;
  const sorted = [...hosts].sort((a, b) => a.want - b.want || a.name.localeCompare(b.name));
  const perRow = Math.max(1, Math.floor((width + HOST_GAP) / step), Math.ceil(sorted.length / Math.ceil(sorted.length / 6)));
  if (sorted.length <= perRow) {
    let x = -Infinity;
    for (const h of sorted) { h.x = Math.max(h.want - HW / 2, x + step); x = h.x; }
    let limit = width - HW;
    for (let i = sorted.length - 1; i >= 0; i--) { sorted[i].x = Math.min(sorted[i].x, limit); limit = sorted[i].x - step; }
    const shift = Math.min(0, sorted[0]?.x ?? 0);
    for (const h of sorted) { h.x -= shift; h.row = 0; }
    return 1;
  }
  const rows = Math.ceil(sorted.length / perRow);
  sorted.forEach((h, i) => {
    const row = Math.floor(i / perRow);
    const count = Math.min(perRow, sorted.length - row * perRow);
    h.row = row;
    h.x = (width - (count * step - HOST_GAP)) / 2 + (i - row * perRow) * step;
  });
  return rows;
}

function buildScene(extras = {}, head = LANE_HEAD) {
  const sc = scope();
  const tonesBy = new Map();
  for (const n of sc.instNodes) tonesBy.set(n.service_id, [...(tonesBy.get(n.service_id) || []), toneOf(n)]);
  const tier = sc.focused && sc.services.length <= TIER_MAX;
  const layout = !sc.services.length ? null : tier ? tierLayout(sc.services, sc.svcEdges) :
    serviceLayout(sc.services, sc.svcEdges, sc.showInstances ? COL_GAP_INST : COL_GAP);
  const cw = tier ? TW : SW, pw = tier ? TPW : PW;
  const scene = { ...sc, tier, lanes: [], cards: [], groups: [], pills: [], hostChips: [], edges: [], iedges: [], bundles: [], runs: [], hint: null, pillOf: new Map() };
  let y = 0;
  const lane = (key, label, height) => {
    const top = y;
    const extra = extras[key] || 0;
    const contentTop = top + head + extra / 2;
    y = contentTop + height + LANE_FOOT + extra / 2;
    scene.lanes.push({ key, label, top, bottom: y, contentTop, contentBottom: contentTop + height });
    return contentTop;
  };

  if (layout && state.layers.services) {
    const top = lane("services", "Services", layout.height);
    for (const s of sc.services) {
      const c = layout.cards.get(s.service_id);
      const tones = tonesBy.get(s.service_id) || [];
      const count = Math.max(s.instance_count || 0, tones.length);
      scene.cards.push({ node: s, x: c.cx - cw / 2, y: top + c.cy - SH / 2, w: cw, h: SH, col: c.col, tone: toneOf(s),
        pips: tier ? null : [...tones, ...Array(Math.max(0, count - tones.length)).fill("unk")], focus: s.service_id === state.focus && sc.focused });
    }
    const cardOf = new Map(scene.cards.map((c) => [c.node.service_id, c]));
    const maxRate = Math.max(1e-9, ...sc.svcEdges.map((e) => e.request_rate_rps || 0));
    for (const e of sc.svcEdges) {
      const from = cardOf.get(e.source), to = cardOf.get(e.target);
      if (tier && from === to) continue;
      const d = !tier ? link(from, to, layout.routes.get(e.source + ">" + e.target).map((p) => ({ x: p.x, y: top + p.y }))) :
        to.col - from.col === 1 ? link(from, to) : arc(from, to, layout.arcHeight(e));
      scene.edges.push({ edge: e, d, tone: toneFromErrorRate(e.error_rate, e.calls), width: 1.25 + 2.25 * Math.sqrt((e.request_rate_rps || 0) / maxRate) });
    }
  }

  if (state.layers.instances && layout) {
    if (sc.showInstances && sc.instNodes.length) {
      const colOf = (serviceId) => layout.cards.get(serviceId)?.col;
      const serviceOf = new Map(sc.instNodes.map((n) => [n.id, n.service_id]));
      // Tier view: calls that would cross another group travel bundled along a gutter.
      const pairs = new Map();
      if (tier) {
        for (const e of sc.instEdges) {
          const a = serviceOf.get(e.source), b = serviceOf.get(e.target);
          if (a === b || colOf(b) - colOf(a) === 1) continue;
          const pair = pairs.get(a + ">" + b) || { pair: a + ">" + b, source: a, target: b, forward: colOf(b) > colOf(a), edges: [], calls: 0, errors: 0, rate: 0, duration: 0 };
          pair.edges.push(e);
          pair.calls += e.calls || 0;
          pair.errors += e.errors || 0;
          pair.rate += e.request_rate_rps || 0;
          pair.duration += (e.avg_latency_ms || 0) * (e.calls || 0);
          pairs.set(pair.pair, pair);
        }
      }
      const columns = new Map();
      for (const s of sc.services) {
        const members = sc.instNodes.filter((n) => n.service_id === s.service_id)
          .sort((a, b) => shortInstance(a).localeCompare(shortInstance(b), undefined, { numeric: true }));
        if (!members.length) continue;
        const c = layout.cards.get(s.service_id);
        columns.set(c.col, [...(columns.get(c.col) || []), { s, members, cy: c.cy }]);
      }
      const heights = [...columns.values()].map((list) => list.reduce((sum, g) =>
        sum + GROUP_HEAD + g.members.length * (PH + PILL_GAP) - PILL_GAP + GROUP_PAD + GROUP_GAP, -GROUP_GAP));
      const groupsHeight = Math.max(0, ...heights);
      const top = lane("instances", "Instances", groupsHeight + (pairs.size ? 12 + pairs.size * GUTTER_STEP : 0));
      for (const [col, list] of columns) {
        let gy = top;
        for (const { s, members } of list.sort((a, b) => a.cy - b.cy)) {
          const x = layout.center(col) - cw / 2;
          const h = GROUP_HEAD + members.length * (PH + PILL_GAP) - PILL_GAP + GROUP_PAD;
          scene.groups.push({ node: s, x, y: gy, w: cw, h, col, focus: s.service_id === state.focus && sc.focused,
            label: s.name, count: members.length + (tier ? " inst" : members.length === 1 ? " instance" : " instances") });
          members.forEach((n, i) => {
            const pill = { node: n, x: x + (cw - pw) / 2, y: gy + GROUP_HEAD + i * (PH + PILL_GAP), w: pw, h: PH, tone: toneOf(n), col };
            scene.pills.push(pill);
            scene.pillOf.set(n.id, pill);
          });
          gy += h + GROUP_GAP;
        }
      }
      const groupOf = new Map(scene.groups.map((g) => [g.node.service_id, g]));
      const bundleRate = Math.max(1e-9, ...[...pairs.values()].map((b) => b.rate));
      [...pairs.values()].sort((a, b) => Math.abs(colOf(a.target) - colOf(a.source)) - Math.abs(colOf(b.target) - colOf(b.source)) || colOf(a.source) - colOf(b.source))
        .forEach((b, j) => {
          const from = groupOf.get(b.source), to = groupOf.get(b.target);
          const edge = sc.svcEdges.find((e) => e.source === b.source && e.target === b.target);
          Object.assign(b, {
            key: edge ? "e:" + edge.id : "b:" + b.pair, gy: top + groupsHeight + 12 + j * GUTTER_STEP,
            outX: b.forward ? from.x + from.w + TGAP / 2 : from.x - TGAP / 2, inX: b.forward ? to.x - TGAP / 2 : to.x + to.w + TGAP / 2,
            tone: toneFromErrorRate(b.calls ? b.errors / b.calls : 0, b.calls), width: 1.5 + 2.5 * Math.sqrt(b.rate / bundleRate),
            summary: { request_rate_rps: b.rate, error_rate: b.calls ? b.errors / b.calls : 0, avg_latency_ms: b.calls ? b.duration / b.calls : 0 },
          });
          b.d = gutterRoute(b, from, to);
          scene.bundles.push(b);
        });
      const bundleOf = new Map(scene.bundles.flatMap((b) => b.edges.map((e) => [e.id, b])));
      const maxRate = Math.max(1e-9, ...sc.instEdges.map((e) => e.request_rate_rps || 0));
      for (const e of sc.instEdges) {
        const from = scene.pillOf.get(e.source), to = scene.pillOf.get(e.target);
        if (!from || !to) continue;
        const bundle = bundleOf.get(e.id);
        scene.iedges.push({ edge: e, bundle, d: bundle ? null : link(from, to), tone: toneFromErrorRate(e.error_rate, e.calls), width: 1 + Math.sqrt((e.request_rate_rps || 0) / maxRate) });
      }
    } else {
      const top = lane("instances", "Instances", HINT_H);
      const count = sc.instNodes.length;
      scene.hint = {
        y: top + HINT_H / 2,
        text: sc.showInstances ? "No instance identities reported in this scope" :
          sc.focused ? count + " instances in this neighbourhood · switch to Instances to expand them" :
          "Select a service to expand its instances",
      };
    }
  }

  const width = Math.max(layout?.width || 0, cw);
  if (state.layers.hosts) {
    const byName = new Map(state.hosts.map((h) => [h.name, h]));
    const linked = new Map();
    const want = (host, x, tone) => {
      const entry = linked.get(host) || { name: host, xs: [], tones: [] };
      entry.xs.push(x);
      if (tone) entry.tones.push(tone);
      linked.set(host, entry);
    };
    if (scene.pills.length) {
      for (const p of scene.pills) for (const h of p.node.hosts) want(h, p.x + p.w / 2, p.tone);
    } else {
      const names = new Set(sc.services.map((s) => s.name));
      const cardX = new Map(scene.cards.map((c) => [c.node.service_id, c.x + c.w / 2]));
      const fallbackX = width / 2;
      for (const s of sc.services) for (const h of s.hosts) want(h, cardX.get(s.service_id) ?? fallbackX);
      for (const h of state.hosts) {
        const on = sc.services.filter((s) => h.services.includes(s.name) && names.has(s.name));
        for (const s of on) if (!s.hosts.includes(h.name)) want(h.name, cardX.get(s.service_id) ?? fallbackX);
      }
      for (const n of sc.instNodes) for (const h of n.hosts) if (linked.has(h)) linked.get(h).tones.push(toneOf(n));
    }
    const chips = [...linked.values()].map((h) => ({ ...h, info: byName.get(h.name), want: h.xs.reduce((a, b) => a + b, 0) / h.xs.length }));
    if (chips.length) {
      const rows = placeHosts(chips, width);
      const pipCols = Math.floor((HW - 20) / PIP);
      const pipRows = clamp(Math.max(...chips.map((h) => Math.ceil(h.tones.length / pipCols))), 1, 2);
      const hh = 36 + pipRows * PIP_ROW;
      const top = lane("hosts", "Hosts", rows * hh + (rows - 1) * HOST_ROW_GAP);
      for (const h of chips) {
        scene.hostChips.push({ ...h, y: top + h.row * (hh + HOST_ROW_GAP), w: HW, h: hh, tone: h.tones.length ? worst(h.tones) : "unk", pipCols, pipRows });
      }
      if (scene.pills.length) bundleRuns(scene);
    } else {
      const top = lane("hosts", "Hosts", HINT_H);
      scene.hostHint = { y: top + HINT_H / 2, text: "No host placement reported for this scope" };
    }
  }

  const xs = [0, width, ...scene.hostChips.map((h) => h.x + HW)];
  scene.bounds = { x: Math.min(...xs), y: 0, width: Math.max(...xs) - Math.min(...xs), height: y };
  scene.width = width;
  return scene;
}

// One placement connector per (service group, host), weighted by how many of
// the group's instances run there. Ends spread along both boxes to avoid a knot.
function bundleRuns(scene) {
  const chipOf = new Map(scene.hostChips.map((c) => [c.name, c]));
  const groupOf = new Map(scene.groups.map((g) => [g.node.service_id, g]));
  scene.runsBend = scene.lanes.find((l) => l.key === "instances").contentBottom + 12;
  const pairs = new Map();
  for (const p of scene.pills) for (const host of p.node.hosts) {
    const chip = chipOf.get(host), group = groupOf.get(p.node.service_id);
    if (!chip || !group) continue;
    const key = "r:" + p.node.service_id + "|" + host;
    const pair = pairs.get(key) || { key, chip, group, count: 0 };
    pair.count++;
    pairs.set(key, pair);
  }
  const list = [...pairs.values()];
  const spread = (items, box, own) => {
    items.sort((a, b) => own(a) - own(b));
    items.forEach((item, i) => { item[box === "group" ? "sx" : "ex"] = item[box].x + item[box].w * (i + 1) / (items.length + 1); });
  };
  for (const g of scene.groups) spread(list.filter((r) => r.group === g), "group", (r) => r.chip.x);
  for (const c of scene.hostChips) spread(list.filter((r) => r.chip === c), "chip", (r) => r.group.x);
  for (const r of list) {
    const g = r.group;
    const [start, x] = scene.tier ? [`M${r.sx} ${g.y + g.h}`, r.sx] : [`M${g.x} ${g.y + GROUP_HEAD / 2} H${g.x - 8}`, g.x - 8];
    scene.runs.push({ key: r.key, d: runsPath(start, x, r.ex, r.chip.y, scene.runsBend), width: clamp(0.5 + 0.5 * r.count, 1, 4) });
  }
}

function runsPath(start, x, chipX, chipY, bend) {
  const mid = bend + (chipY - bend) / 2;
  return `${start} V${bend} C${x} ${mid} ${chipX} ${mid} ${chipX} ${chipY}`;
}

// ---------- Scene rendering ----------

// Nested <svg> icons ignore CSS sizing inside an SVG scene; size them explicitly.
function place(node, x, y, size = 14) {
  for (const [k, v] of Object.entries({ x, y, width: size, height: size, viewBox: "0 0 24 24" })) node.setAttribute(k, v);
  return node;
}

function pips(list, x, y, cap) {
  const g = svg("g", { class: "pip-row", transform: `translate(${x} ${y})` });
  const shown = list.length > cap ? list.slice(0, cap - 1) : list;
  shown.forEach((tone, i) => g.append(svg("rect", { class: "pip p-" + tone, x: i * PIP, y: 0, width: 6, height: 10, rx: 1 })));
  if (list.length > cap) g.append(svg("text", { class: "t-small", x: shown.length * PIP + 2, y: 9 }, "+" + (list.length - shown.length)));
  return g;
}

function nodeGroup(key, x, y, label, className) {
  return svg("g", { class: "node " + className, transform: `translate(${x} ${y})`, "data-key": key, tabindex: 0, role: "button", "aria-label": label });
}

function drawCard(c) {
  const s = c.node, w = c.w;
  const metric = s.requests ? fmtRate(s.request_rate_rps) + " · " + fmtPercent(s.error_rate) : "no traffic";
  const count = c.pips ? c.pips.length : s.instance_count || 0;
  const label = [s.name, nsLabel(s.namespace), TONE_LABEL[c.tone], s.requests ? fmtRate(s.request_rate_rps) + ", " + fmtPercent(s.error_rate) + " errors" : "no traffic",
    count + (count === 1 ? " instance" : " instances")].join(", ");
  const g = nodeGroup("s:" + s.service_id, c.x, c.y, label, "svc t-" + c.tone + (c.focus ? " is-focus" : ""));
  if (c.tone === "crit") g.append(svg("rect", { class: "halo", x: -4, y: -4, width: w + 8, height: SH + 8, rx: 6 }));
  g.append(svg("rect", { class: "box", width: w, height: SH, rx: 4 }));
  g.append(place(icon(TONE_ICON[c.tone], "ic ic-sm tone-" + c.tone), 10, 8));
  g.append(svg("text", { class: "t-name", x: 30, y: 20 }, fit(s.name, w - 40, FONT.name)));
  g.append(svg("text", { class: "t-sub", x: 30, y: 33 }, fit(nsLabel(s.namespace), w - 40, FONT.sub)));
  const metricClass = "t-small t-metric" + (c.tone === "crit" || c.tone === "warn" ? " tone-" + c.tone : "");
  if (c.pips) {
    const metricWidth = textWidth(metric, FONT.small);
    g.append(svg("text", { class: metricClass, x: w - 10, y: 47, "text-anchor": "end" }, metric));
    g.append(pips(c.pips, 10, 38, clamp(Math.floor((w - 28 - metricWidth) / PIP), 2, 12)));
  } else {
    // In a tier the instances sit right below the card; the pips would repeat them.
    g.append(svg("text", { class: metricClass, x: 30, y: 47 }, fit(metric, w - 40, FONT.small)));
  }
  g.append(svg("title", {}, [s.name, nsLabel(s.namespace), envLabel(s.environment)].join(" · ")));
  return g;
}

function drawPill(p) {
  const n = p.node;
  const label = "Instance " + (n.instance || shortInstance(n)) + " of " + n.name + ", " + TONE_LABEL[p.tone] + ", average " + fmtMs(n.avg_latency_ms) + (n.hosts[0] ? ", on " + n.hosts[0] : "");
  const g = nodeGroup("i:" + n.id, p.x, p.y, label, "inst t-" + p.tone);
  g.append(svg("rect", { class: "box", width: p.w, height: PH, rx: 4 }));
  g.append(place(icon(TONE_ICON[p.tone], "ic ic-xs tone-" + p.tone), 8, 6, 12));
  const latency = n.requests ? fmtMs(n.avg_latency_ms) : "—";
  g.append(svg("text", { class: "t-small t-metric", x: p.w - 8, y: 16, "text-anchor": "end" }, latency));
  g.append(svg("text", { class: "t-mono", x: 26, y: 16 }, fit(shortInstance(n), p.w - 42 - textWidth(latency, FONT.small), FONT.mono)));
  g.append(svg("title", {}, [n.instance || shortInstance(n), n.hosts.join(", ") || "host unknown", "avg " + latency].join(" · ")));
  return g;
}

function drawHost(h) {
  const count = h.info?.service_count;
  const label = "Host " + h.name + ", " + TONE_LABEL[h.tone] + (finite(count) ? ", " + count + " services" : "") + ", " + h.tones.length + " instances in view";
  const g = nodeGroup("h:" + h.name, h.x, h.y, label, "host t-" + h.tone);
  if (h.tone === "crit") g.append(svg("rect", { class: "halo", x: -4, y: -4, width: h.w + 8, height: h.h + 8, rx: 6 }));
  g.append(svg("rect", { class: "box", width: h.w, height: h.h, rx: 4 }));
  g.append(place(icon(TONE_ICON[h.tone], "ic ic-sm tone-" + h.tone), 10, 9));
  const side = finite(count) ? count + " svc" : "";
  const sideWidth = side ? textWidth(side, FONT.small) + 8 : 0;
  g.append(svg("text", { class: "t-mono t-host", x: 30, y: 20 }, fit(h.name, h.w - 40 - sideWidth, FONT.mono)));
  if (side) g.append(svg("text", { class: "t-small t-metric", x: h.w - 10, y: 20, "text-anchor": "end" }, side));
  const matrix = svg("g", { class: "pip-row", transform: "translate(10 30)" });
  const cap = h.pipCols * h.pipRows;
  const tones = [...h.tones].sort((a, b) => toneRank(a) - toneRank(b));
  const shown = tones.length > cap ? tones.slice(0, cap - 1) : tones;
  shown.forEach((tone, i) => matrix.append(svg("rect", { class: "pip p-" + tone, x: (i % h.pipCols) * PIP, y: Math.floor(i / h.pipCols) * PIP_ROW, width: 6, height: 10, rx: 1 })));
  if (tones.length > cap) matrix.append(svg("text", { class: "t-small", x: (shown.length % h.pipCols) * PIP + 2, y: Math.floor(shown.length / h.pipCols) * PIP_ROW + 9 }, "+" + (tones.length - shown.length)));
  if (!tones.length) matrix.append(svg("text", { class: "t-small t-faint", x: 0, y: 9 }, "no instance data"));
  g.append(matrix);
  g.append(svg("title", {}, [h.name, h.info?.last_seen ? "seen " + fmtAgo(h.info.last_seen) : ""].filter(Boolean).join(" · ")));
  return g;
}

function drawEdge(key, item, name) {
  const g = svg("g", { class: "edge t-" + item.tone, "data-key": key });
  g.append(svg("path", { class: "hit", d: item.d }));
  g.append(svg("path", { class: "line" + (item.tone === "crit" && key.startsWith("e:") && !item.pair ? " edge-hot" : ""), d: item.d, "stroke-width": item.width.toFixed(2), "marker-end": "url(#mk-" + item.tone + ")" }));
  g.append(svg("title", {}, name + " · " + fmtRate(item.edge.request_rate_rps) + " · " + fmtPercent(item.edge.error_rate) + " errors · avg " + fmtMs(item.edge.avg_latency_ms)));
  return g;
}

function marker(tone) {
  const m = svg("marker", { id: "mk-" + tone, class: "mk mk-" + tone, viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 7, markerHeight: 7, markerUnits: "userSpaceOnUse", orient: "auto" });
  m.append(svg("path", { d: "M1 1L9 5L1 9Z" }));
  return m;
}

function renderScene(scene) {
  const defs = svg("defs");
  for (const tone of [...TONES, "lit", "strong"]) defs.append(marker(tone));
  const world = svg("g", { class: "world" });
  const lanes = svg("g", { class: "lanes" });
  scene.lanes.forEach((lane, i) => {
    if (i) lanes.append(svg("line", { class: "lane-rule", x1: -100000, x2: 100000, y1: lane.top, y2: lane.top }));
  });
  const nameOf = new Map(scene.instNodes.map((n) => [n.id, n.name + " " + shortInstance(n)]));
  for (const s of scene.services) nameOf.set(s.service_id, s.name);
  const groups = svg("g", { class: "groups" });
  for (const gr of scene.groups) {
    const g = svg("g", { class: "group" + (gr.focus ? " is-focus" : ""), "data-key": "g:" + gr.node.service_id });
    g.append(svg("rect", { x: gr.x - 0.5, y: gr.y - 0.5, width: gr.w + 1, height: gr.h + 1, rx: 4 }));
    const countWidth = textWidth(gr.count, FONT.small);
    g.append(svg("text", { class: "t-group-name", x: gr.x + 8, y: gr.y + 16 }, fit(gr.label, gr.w - 24 - countWidth, FONT.sub)));
    g.append(svg("text", { class: "t-group", x: gr.x + gr.w - 8, y: gr.y + 16, "text-anchor": "end" }, gr.count));
    groups.append(g);
  }
  const runs = svg("g", { class: "runs" });
  for (const r of scene.runs) runs.append(svg("path", { class: "run", d: r.d, "data-key": r.key, "stroke-width": r.width }));
  const live = svg("g", { class: "runs runs-live" });
  const links = svg("g", { class: "links" });
  const edges = svg("g", { class: "edges" });
  const order = (a, b) => toneRank(b.tone) - toneRank(a.tone);
  for (const e of [...scene.edges].sort(order)) edges.append(drawEdge("e:" + e.edge.id, e, nameOf.get(e.edge.source) + " → " + nameOf.get(e.edge.target)));
  for (const e of [...scene.iedges].filter((item) => item.d).sort(order)) edges.append(drawEdge("x:" + e.edge.id, e, nameOf.get(e.edge.source) + " → " + nameOf.get(e.edge.target)));
  for (const b of [...scene.bundles].sort(order)) {
    const el = drawEdge(b.key, { ...b, edge: b.summary }, nameOf.get(b.source) + " → " + nameOf.get(b.target) + " · " + b.edges.length + " instance calls");
    el.classList.add("bundle");
    el.dataset.bundle = b.pair;
    edges.append(el);
  }
  const expanded = svg("g", { class: "edges edges-live" });
  const nodes = svg("g", { class: "nodes" });
  for (const c of scene.cards) nodes.append(drawCard(c));
  for (const p of scene.pills) nodes.append(drawPill(p));
  for (const h of scene.hostChips) nodes.append(drawHost(h));
  const hints = svg("g", { class: "hints" });
  for (const hint of [scene.hint, scene.hostHint].filter(Boolean)) {
    hints.append(svg("text", { class: "t-hint", x: scene.width / 2, y: hint.y + 4, "text-anchor": "middle" }, hint.text));
  }
  world.append(lanes, groups, runs, live, links, edges, expanded, nodes, hints);
  map.replaceChildren(defs, world);
  Object.assign(scene, { world, live, links, expanded, nameOf });
  $("lane-labels").replaceChildren(...scene.lanes.map((lane) => element("span", "lane-label eyebrow", lane.label)));
}

// ---------- Highlight ----------

const keyOf = (prefix, id) => prefix + ":" + id;

function related(key) {
  const sc = state.scene;
  const lit = new Set([key]);
  const kind = key[0], id = key.slice(2);
  const pillsOf = (test) => sc.pills.filter((p) => test(p.node));
  const lightPill = (p) => {
    lit.add(keyOf("i", p.node.id));
    for (const h of p.node.hosts) { lit.add(keyOf("h", h)); lit.add("r:" + p.node.service_id + "|" + h); }
  };
  const lightIEdges = (test) => {
    for (const e of sc.iedges) if (test(e.edge)) {
      lit.add(keyOf("x", e.edge.id)); lit.add(keyOf("i", e.edge.source)); lit.add(keyOf("i", e.edge.target));
      if (e.bundle) lit.add(e.bundle.key);
    }
  };
  if (kind === "s") {
    lit.add(keyOf("g", id));
    for (const e of sc.edges) if (e.edge.source === id || e.edge.target === id) {
      lit.add(keyOf("e", e.edge.id)); lit.add(keyOf("s", e.edge.source)); lit.add(keyOf("s", e.edge.target));
    }
    const own = pillsOf((n) => n.service_id === id);
    own.forEach(lightPill);
    const ids = new Set(own.map((p) => p.node.id));
    lightIEdges((e) => ids.has(e.source) || ids.has(e.target));
    if (!sc.pills.length) for (const h of hostsOfService(id)) lit.add(keyOf("h", h));
  } else if (kind === "i") {
    const pill = sc.pills.find((p) => p.node.id === id);
    if (pill) { lightPill(pill); lit.add(keyOf("s", pill.node.service_id)); lit.add(keyOf("g", pill.node.service_id)); }
    lightIEdges((e) => e.source === id || e.target === id);
  } else if (kind === "h") {
    if (sc.pills.length) {
      for (const p of pillsOf((n) => n.hosts.includes(id))) { lightPill(p); lit.add(keyOf("s", p.node.service_id)); lit.add(keyOf("g", p.node.service_id)); }
    } else for (const s of servicesOnHost(id)) lit.add(keyOf("s", s.service_id));
  } else if (kind === "b") {
    const b = sc.bundles.find((item) => item.key === key);
    if (b) lightIEdges((x) => b.edges.includes(x));
  } else if (kind === "e") {
    const e = sc.edges.find((item) => item.edge.id === id)?.edge;
    if (e) {
      lit.add(keyOf("s", e.source)); lit.add(keyOf("s", e.target));
      const serviceOf = new Map(sc.pills.map((p) => [p.node.id, p.node.service_id]));
      lightIEdges((x) => serviceOf.get(x.source) === e.source && serviceOf.get(x.target) === e.target);
    }
  } else if (kind === "x") {
    const e = sc.iedges.find((item) => item.edge.id === id)?.edge;
    if (e) for (const end of [e.source, e.target]) {
      const pill = sc.pills.find((p) => p.node.id === end);
      if (pill) { lightPill(pill); lit.add(keyOf("s", pill.node.service_id)); }
    }
  }
  return lit;
}

function hostsOfService(serviceId) {
  const s = state.scene.services.find((item) => item.service_id === serviceId);
  if (!s) return [];
  return unique([...s.hosts, ...state.hosts.filter((h) => h.services.includes(s.name)).map((h) => h.name)])
    .filter((h) => state.scene.hostChips.some((c) => c.name === h));
}

function servicesOnHost(host) {
  const info = state.hosts.find((h) => h.name === host);
  return state.scene.services.filter((s) => s.hosts.includes(host) || info?.services.includes(s.name));
}

function paint() {
  const sc = state.scene;
  if (!sc) return;
  const selected = state.selected;
  const active = state.hover || selected;
  sc.live.replaceChildren();
  sc.links.replaceChildren();
  sc.expanded.replaceChildren();
  const expanded = active ? drawActive(sc, active) : new Set();
  const cardOf = new Map(sc.cards.map((c) => [c.node.service_id, c]));
  const activeService = active?.[0] === "s" ? active.slice(2) : active?.[0] === "i" ? sc.pillOf.get(active.slice(2))?.node.service_id : null;
  // In a tier every card sits on its own group; elsewhere only the focus and the active service are joined.
  for (const g of sc.groups) {
    const card = cardOf.get(g.node.service_id);
    if (card && (sc.tier || card.focus || g.node.service_id === activeService)) sc.links.append(svg("path", { class: "group-link", d: groupLink(card, g) }));
  }
  const lit = active ? related(active) : null;
  for (const el of map.querySelectorAll("[data-key]")) {
    const key = el.dataset.key;
    el.classList.toggle("is-dim", Boolean(lit) && !lit.has(key));
    el.classList.toggle("is-lit", Boolean(lit) && lit.has(key));
    el.classList.toggle("is-selected", key === selected);
    if (el.dataset.bundle) el.classList.toggle("is-expanded", expanded.has(el.dataset.bundle));
  }
}

// Detail that exists only for the active element: per-pill calls inside a
// bundle, per-pill placement, and service placement at service granularity.
function drawActive(sc, active) {
  const kind = active[0], id = active.slice(2);
  const chipOf = new Map(sc.hostChips.map((c) => [c.name, c]));
  const expanded = new Set();
  if (!sc.pills.length) {
    const cardOf = new Map(sc.cards.map((c) => [c.node.service_id, c]));
    const above = sc.lanes.find((l) => l.key === "instances") || sc.lanes.find((l) => l.key === "services");
    const bend = above ? above.contentBottom + 12 : 0;
    const pairs = kind === "s" ? hostsOfService(id).map((h) => [id, h]) : kind === "h" ? servicesOnHost(id).map((s) => [s.service_id, id]) : [];
    for (const [serviceId, host] of pairs) {
      const card = cardOf.get(serviceId), chip = chipOf.get(host);
      if (!card || !chip) continue;
      const x = card.x + card.w / 2;
      sc.live.append(svg("path", { class: "run is-lit", "stroke-width": 1, d: runsPath(`M${x} ${card.y + SH}`, x, chip.x + chip.w / 2, chip.y, Math.max(bend, card.y + SH + 8)) }));
    }
    return expanded;
  }
  const pill = (pid) => sc.pillOf.get(pid);
  const inBundle = kind === "i" ? (e) => e.source === id || e.target === id :
    kind === "s" ? (e) => pill(e.source)?.node.service_id === id || pill(e.target)?.node.service_id === id :
    kind === "x" ? (e) => e.id === id :
    (e) => sc.bundles.some((b) => b.key === active && b.edges.includes(e));
  for (const item of sc.iedges) {
    if (!item.bundle || !inBundle(item.edge)) continue;
    expanded.add(item.bundle.pair);
    const e = item.edge;
    sc.expanded.append(drawEdge("x:" + e.id, { ...item, d: gutterRoute(item.bundle, pill(e.source), pill(e.target)) }, sc.nameOf.get(e.source) + " → " + sc.nameOf.get(e.target)));
  }
  const placed = kind === "i" ? [pill(id)] : kind === "s" ? sc.pills.filter((p) => p.node.service_id === id) :
    kind === "h" ? sc.pills.filter((p) => p.node.hosts.includes(id)) :
    kind === "x" ? sc.iedges.filter((item) => item.edge.id === id).flatMap((item) => [pill(item.edge.source), pill(item.edge.target)]) : [];
  const groupOf = new Map(sc.groups.map((g) => [g.node.service_id, g]));
  for (const p of placed.filter(Boolean)) {
    const g = groupOf.get(p.node.service_id);
    for (const host of p.node.hosts) {
      const chip = chipOf.get(host);
      if (!chip || (kind === "h" && host !== id)) continue;
      const x = g.x - 6;
      sc.live.append(svg("path", { class: "run is-lit", "stroke-width": 1, d: runsPath(`M${p.x} ${p.y + p.h / 2} H${x}`, x, chip.x + chip.w / 2, chip.y, sc.runsBend) }));
    }
  }
  return expanded;
}

// A card's own group may sit below other cards of its column; route around them on the left.
function groupLink(card, group) {
  const blocked = state.scene.cards.some((c) => c !== card && c.col === card.col && c.y > card.y) ||
    state.scene.groups.some((g) => g !== group && g.x === group.x && g.y < group.y);
  const cx = card.x + card.w / 2;
  if (!blocked) return `M${cx} ${card.y + SH} V${group.y}`;
  const gutter = card.x - 12;
  return `M${card.x} ${card.y + SH / 2} H${gutter} V${group.y + GROUP_HEAD / 2} H${group.x}`;
}

// ---------- View ----------

function applyView() {
  const sc = state.scene;
  if (!sc) return;
  const { x, y, k } = state.view;
  sc.world.setAttribute("transform", `translate(${x} ${y}) scale(${k})`);
  const labels = $("lane-labels").children;
  sc.lanes.forEach((lane, i) => { labels[i].style.transform = `translateY(${Math.round(y + lane.top * k + 10)}px)`; });
}

function sceneKey(scene) {
  return [state.mode, state.focus, state.hops, state.namespace, state.env, JSON.stringify(state.layers),
    scene.cards.map((c) => c.node.service_id).join(), scene.pills.length, scene.hostChips.length].join("|");
}

// Lanes stretch to fill the canvas, in proportion to their content, so the
// bands never stop short of the bottom edge. Phones keep a legible floor scale
// and open on the most important card; the rest is a pan away.
function layoutAndFit(refit) {
  const rect = canvas.getBoundingClientRect();
  if (!rect.width || !rect.height) return;
  const phone = rect.width < 720;
  const pad = phone ? 12 : 16;
  const scaleFor = (b) => {
    const fit = clamp(Math.min((rect.width - pad * 2) / Math.max(1, b.width), rect.height / Math.max(1, b.height)), 0.2, 1);
    return phone ? Math.max(fit, 0.6) : fit;
  };
  let scene = buildScene();
  let k = refit || !state.scene ? scaleFor(scene.bounds) : state.view.k;
  const head = Math.max(LANE_HEAD, 36 / k);
  scene = buildScene({}, head);
  if (refit || !state.scene) k = scaleFor(scene.bounds);
  const spare = rect.height / k - scene.bounds.height;
  if (spare > 1 && scene.lanes.length) {
    const weights = scene.lanes.map((l) => [l.key, l.contentBottom - l.contentTop + 24]);
    const total = weights.reduce((sum, [, w]) => sum + w, 0);
    scene = buildScene(Object.fromEntries(weights.map(([key, w]) => [key, spare * w / total])), head);
  }
  renderScene(scene);
  state.scene = scene;
  if (refit) {
    const b = scene.bounds;
    let x = (rect.width - b.width * k) / 2 - b.x * k;
    if (b.width * k > rect.width - pad * 2) {
      const anchor = scene.cards.find((c) => c.focus) || [...scene.cards].sort((a, c) => toneRank(a.tone) - toneRank(c.tone))[0];
      if (anchor) x = clamp(rect.width / 2 - (anchor.x + anchor.w / 2) * k, rect.width - pad - (b.x + b.width) * k, pad - b.x * k);
    }
    state.view = { k, x: Math.round(x), y: 0 };
  }
  applyView();
  paint();
}

function zoomAt(factor, cx, cy) {
  const v = state.view;
  const k = clamp(v.k * factor, 0.15, 2.5);
  const f = k / v.k;
  state.view = { k, x: cx - (cx - v.x) * f, y: cy - (cy - v.y) * f };
  applyView();
}

function zoomCenter(factor) {
  const rect = canvas.getBoundingClientRect();
  zoomAt(factor, rect.width / 2, rect.height / 2);
}

// ---------- States ----------

function showState(kind, error) {
  const box = $("map-state");
  box.hidden = !kind;
  if (!kind) return box.replaceChildren();
  if (kind === "loading") {
    const skeleton = element("div", "skel");
    for (const label of ["Services", "Instances", "Hosts"]) {
      const lane = element("div", "skel-lane");
      const row = element("div", "skel-row");
      for (let i = 0; i < (label === "Instances" ? 7 : 5); i++) row.append(element("i", "skeleton"));
      lane.append(element("span", "eyebrow", label), row);
      skeleton.append(lane);
    }
    skeleton.setAttribute("aria-label", "Loading topology");
    skeleton.setAttribute("role", "status");
    return box.replaceChildren(skeleton);
  }
  const panel = element("div", "state");
  const actions = element("div", "state-actions");
  if (kind === "unavailable") {
    panel.append(icon("database", "ic state-ic"), element("b", "", "Instance topology is unavailable on this storage driver"),
      element("p", "", "The layered map needs SQLite or PostgreSQL. The Explorer still shows the service graph for every driver."));
    const link = element("a", "btn", "Open Explorer");
    link.href = "/explorer";
    link.append(icon("arrow-right", "ic ic-sm"));
    actions.append(link);
  } else if (kind === "empty") {
    panel.append(icon("radio", "ic state-ic"), element("b", "", "Waiting for telemetry"),
      element("p", "", "Send OTLP traces with service.name and service.instance.id. Services, instances and hosts appear here within a minute."));
  } else if (kind === "quiet") {
    panel.append(icon("clock", "ic state-ic"), element("b", "", "No traffic in this range"),
      element("p", "", "Services are known, but none reported spans in the selected range and environment. Widen the range or pick another environment."));
  } else {
    const message = error?.status === 429 ? "Every topology query slot stayed busy through several retries. Try again in a few seconds." :
      error?.message || "The request did not complete.";
    panel.append(icon("wifi-off", "ic state-ic tone-crit"), element("b", "", "The topology could not load"), element("p", "", message));
    const retry = element("button", "btn", "Retry");
    retry.type = "button";
    retry.prepend(icon("rotate-ccw", "ic ic-sm"));
    retry.addEventListener("click", () => load());
    actions.append(retry);
  }
  if (actions.childElementCount) panel.append(actions);
  panel.setAttribute("role", kind === "error" ? "alert" : "status");
  box.replaceChildren(panel);
}

// ---------- Top bar, toolbar and rail ----------

function renderStatus() {
  const label = $("updated");
  const seconds = Math.round((Date.now() - state.updated) / 1000);
  label.textContent = state.loading && !state.updated ? "Loading…" : state.refreshFailed ? "Refresh failed" :
    !state.updated ? "Not loaded" : seconds < 60 ? "Updated " + seconds + "s ago" : "Updated " + fmtAgo(state.updated);
  $("refresh").classList.toggle("is-spinning", state.loading);
  $("refresh").classList.toggle("tone-crit", state.refreshFailed);
}

function renderControls() {
  const env = $("env");
  env.replaceChildren(new Option("All environments", "*"));
  for (const e of state.catalog?.environments || []) env.append(new Option(e || "Unknown environment", JSON.stringify(e)));
  env.value = state.env === undefined ? "*" : JSON.stringify(state.env);
  if (env.selectedIndex < 0) {
    env.append(new Option(envLabel(state.env), JSON.stringify(state.env)));
    env.value = JSON.stringify(state.env);
  }
  for (const b of document.querySelectorAll("[data-range]")) b.setAttribute("aria-pressed", String(b.dataset.range === state.range));
  for (const b of document.querySelectorAll("[data-mode]")) b.setAttribute("aria-pressed", String(b.dataset.mode === state.mode));
  $("hops-field").hidden = !state.focus;
  $("hops").value = String(state.hops);
  const crumbs = element("ol");
  const focused = state.focus ? lookup().service(state.focus) : null;
  const namespace = focused ? focused.namespace : state.namespace;
  const step = (label, action, current) => {
    const li = element("li");
    if (current) {
      const span = element("span", "crumb-current", label);
      span.setAttribute("aria-current", "page");
      span.title = label;
      li.append(span);
    } else {
      const b = element("button", "crumb", label);
      b.type = "button";
      b.title = label;
      b.addEventListener("click", action);
      li.append(b);
    }
    return li;
  };
  crumbs.append(step("All services", () => { state.namespace = ""; unfocus(); }, !namespace && !focused));
  if (namespace) crumbs.append(step(namespace, () => { state.namespace = namespace; unfocus(); }, !focused));
  if (focused) crumbs.append(step(focused.name, null, true));
  $("crumbs").replaceChildren(crumbs);
}

function share(tones) {
  const total = tones.length || 1;
  const bar = element("span", "hbar");
  bar.setAttribute("aria-hidden", "true");
  for (const tone of ["ok", "unk", "warn", "crit"]) {
    const n = tones.filter((t) => t === tone).length;
    if (!n) continue;
    const part = element("i", "hb-" + tone);
    part.style.width = (n / total * 100) + "%";
    bar.append(part);
  }
  return bar;
}

function renderRail() {
  const sc = state.scene;
  const serviceTones = (sc?.services || []).map(toneOf);
  const instanceTones = sc?.pills.length ? sc.pills.map((p) => p.tone) : (sc?.cards || []).flatMap((c) => c.pips);
  const hostTones = (sc?.hostChips || []).map((h) => h.tone);
  const instanceCount = sc?.pills.length || (sc?.services || []).reduce((sum, s) => sum + (s.instance_count || 0), 0);
  const layers = [
    ["services", "Services", "boxes", sc?.services.length || 0, serviceTones],
    ["instances", "Instances", "box", instanceCount, instanceTones],
    ["hosts", "Hosts", "server", sc?.hostChips.length || 0, hostTones],
  ].map(([key, label, glyph, count, tones]) => {
    const b = element("button", "layer");
    b.type = "button";
    b.setAttribute("aria-pressed", String(state.layers[key]));
    const bad = tones.filter((t) => t === "crit").length || tones.filter((t) => t === "warn").length;
    const badTone = tones.includes("crit") ? "crit" : "warn";
    b.setAttribute("aria-label", `${label} lane, ${count}${bad ? ", " + bad + " " + TONE_LABEL[badTone].toLowerCase() : ""}, ${state.layers[key] ? "visible" : "hidden"}`);
    const row = element("span", "layer-row");
    row.append(icon(glyph, "ic ic-sm layer-ic"), element("span", "layer-name", label), element("span", "spacer"));
    if (bad) row.append(badge(badTone, String(bad)));
    row.append(element("span", "layer-count num", state.svc && !state.failure ? fmtNumber(count) : "—"));
    b.append(row, share(tones));
    b.addEventListener("click", () => {
      state.layers[key] = !state.layers[key];
      renderAll({ refit: true });
    });
    return b;
  });
  $("layers").replaceChildren(...layers);

  const all = state.svc?.nodes || [];
  const namespaces = new Map();
  for (const s of all) namespaces.set(s.namespace, [...(namespaces.get(s.namespace) || []), toneOf(s)]);
  const domains = [...namespaces].sort((a, b) => toneRank(worst(a[1])) - toneRank(worst(b[1])) || a[0].localeCompare(b[0])).map(([ns, tones]) => {
    const b = element("button", "row-btn");
    b.type = "button";
    b.setAttribute("aria-pressed", String(!state.focus && state.namespace === ns));
    b.append(toneIcon(worst(tones)), element("span", "row-name", nsLabel(ns)), element("span", "row-count num", tones.length));
    b.title = nsLabel(ns);
    b.addEventListener("click", () => {
      state.namespace = state.namespace === ns && !state.focus ? "" : ns;
      state.selected = null;
      unfocus();
    });
    const li = element("li");
    li.append(b);
    return li;
  });
  $("domains").replaceChildren(...(domains.length ? domains : [element("li", "list-empty", state.svc ? "No services yet" : "—")]));
  renderResults();
}

function renderResults() {
  const query = state.query.trim().toLowerCase();
  const matches = (state.svc?.nodes || []).filter((s) => (s.name + " " + s.namespace).toLowerCase().includes(query))
    .sort((a, b) => toneRank(toneOf(a)) - toneRank(toneOf(b)) || a.name.localeCompare(b.name));
  const items = matches.slice(0, 60).map((s) => {
    const b = element("button", "row-btn");
    b.type = "button";
    b.title = s.name + " · " + nsLabel(s.namespace);
    b.setAttribute("aria-pressed", String(state.focus === s.service_id));
    b.append(toneIcon(toneOf(s)), element("span", "row-name", s.name), element("span", "row-meta", s.namespace));
    b.addEventListener("click", () => focusService(s.service_id));
    const li = element("li");
    li.append(b);
    return li;
  });
  if (matches.length > items.length) items.push(element("li", "list-empty", (matches.length - items.length) + " more · refine the search"));
  $("results").replaceChildren(...(items.length ? items : [element("li", "list-empty", query ? "No matching services" : "—")]));
}

// ---------- Inspector ----------

function lookup() {
  const sc = state.scene;
  const instances = new Map([...(state.inst?.nodes || []), ...(state.focusGraph?.nodes || [])].map((n) => [n.id, n]));
  const iedges = new Map([...(state.inst?.edges || []), ...(state.focusGraph?.edges || [])].map((e) => [e.id, e]));
  return {
    service: (id) => sc?.services.find((s) => s.service_id === id) || state.svc?.nodes.find((s) => s.service_id === id),
    instance: (id) => instances.get(id),
    edge: (id) => sc?.svcEdges.find((e) => e.id === id),
    iedge: (id) => iedges.get(id),
    instancesOf: (serviceId) => [...instances.values()].filter((n) => n.service_id === serviceId)
      .sort((a, b) => shortInstance(a).localeCompare(shortInstance(b), undefined, { numeric: true })),
    allInstances: () => [...instances.values()],
    allIEdges: () => [...iedges.values()],
  };
}

function tile(label, value, tone, spark, note) {
  const t = element("div", "tile");
  t.append(element("span", "tile-label", label));
  const v = element("b", "tile-value num" + (tone === "warn" || tone === "crit" ? " tone-" + tone : ""), value);
  t.append(v);
  if (spark) t.append(spark);
  else if (note) t.append(element("span", "tile-note", note));
  return t;
}

function section(title, ...children) {
  const s = element("section", "insp-sec");
  s.append(element("h3", "eyebrow", title), ...children);
  return s;
}

function details(summary, pairs) {
  const d = element("details", "insp-details");
  d.append(element("summary", "", summary));
  const dl = element("dl", "kv");
  for (const [k, v] of pairs) {
    if (v === undefined || v === null || v === "") continue;
    dl.append(element("dt", "", k), element("dd", "", v));
  }
  d.append(dl);
  return d;
}

function selectable(node, key) {
  const drawn = map.querySelector(`[data-key="${CSS.escape(key)}"]`) || state.scene?.iedges.some((e) => "x:" + e.edge.id === key);
  if (!state.scene || !drawn) return node;
  node.setAttribute("role", "button");
  node.tabIndex = 0;
  const choose = () => {
    select(key);
    $("insp-body").focus({ preventScroll: true });
  };
  node.addEventListener("click", choose);
  node.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") { e.preventDefault(); choose(); }
  });
  return node;
}

function instanceTable(rows) {
  if (!rows.length) return element("p", "insp-empty", "No instance identities in view for this range.");
  const table = element("table", "table");
  const head = element("tr");
  for (const [label, right] of [["Instance"], ["Host"], ["Err", true], ["Avg", true]]) {
    const th = element("th", right ? "r" : "", label);
    th.scope = "col";
    head.append(th);
  }
  const thead = element("thead");
  thead.append(head);
  const body = element("tbody");
  for (const n of rows) {
    const tone = toneOf(n);
    const tr = element("tr");
    const name = element("span", "cell-name");
    name.append(toneIcon(tone), element("span", "mono", shortInstance(n)));
    name.title = n.instance || shortInstance(n);
    const first = element("td");
    first.append(name);
    const host = element("td", "mono cell-dim", n.hosts[0] || "—");
    host.title = n.hosts.join(", ");
    tr.append(first, host, element("td", "r" + (tone === "crit" || tone === "warn" ? " tone-" + tone : ""), n.requests ? fmtPercent(n.error_rate) : "—"),
      element("td", "r", n.requests ? fmtMs(n.avg_latency_ms) : "—"));
    body.append(selectable(tr, "i:" + n.id));
  }
  table.append(thead, body);
  const wrap = element("div", "table-wrap");
  wrap.append(table);
  return wrap;
}

function callList(edges, nameOf, prefix) {
  if (!edges.length) return element("p", "insp-empty", "No observed calls in this range.");
  const list = element("ul", "calls");
  for (const e of edges) {
    const tone = toneFromErrorRate(e.error_rate, e.calls);
    const li = element("li", "call");
    const names = element("span", "call-names");
    names.append(toneIcon(tone), element("span", "call-a", nameOf(e.source)), icon("arrow-right", "ic ic-xs call-arrow"), element("span", "call-b", nameOf(e.target)));
    names.title = nameOf(e.source) + " → " + nameOf(e.target);
    const stats = element("span", "call-stats num");
    stats.append(element("span", "", fmtRate(e.request_rate_rps)), element("span", tone === "crit" || tone === "warn" ? "tone-" + tone : "", fmtPercent(e.error_rate)));
    li.title = names.title + " · avg " + fmtMs(e.avg_latency_ms);
    li.append(names, stats);
    list.append(selectable(li, prefix + ":" + e.id));
  }
  return list;
}

function trafficTiles(s) {
  const tone = toneOf(s);
  const series = state.traffic.get(trafficKey(s.name));
  const counts = series?.map((p) => p.count);
  const errors = series?.map((p) => p.count ? p.error_count / p.count : 0);
  const tiles = element("div", "tiles");
  tiles.append(
    tile("Requests", s.requests ? fmtRate(s.request_rate_rps) : "—", "ok", counts ? sparkline(counts, { width: 88, height: 20, label: "Requests per minute" }) : null, "per second"),
    tile("Errors", s.requests ? fmtPercent(s.error_rate) : "—", tone, errors ? sparkline(errors, { width: 88, height: 20, tone, label: "Error ratio per minute" }) : null, "of requests"),
    tile("Avg latency", s.requests ? fmtMs(s.avg_latency_ms) : "—", "ok", null, "mean, not p99"),
  );
  return tiles;
}

function metricTiles(item, rateLabel = "Requests") {
  const tone = toneFromErrorRate(item.error_rate, item.requests ?? item.calls);
  const has = (item.requests ?? item.calls) > 0;
  const tiles = element("div", "tiles");
  tiles.append(
    tile(rateLabel, has ? fmtRate(item.request_rate_rps) : "—", "ok", null, "per second"),
    tile("Errors", has ? fmtPercent(item.error_rate) : "—", tone, null, fmtNumber(item.errors || 0) + " of " + fmtNumber(item.requests ?? item.calls ?? 0)),
    tile("Avg latency", has ? fmtMs(item.avg_latency_ms) : "—", "ok", null, "mean, not p99"),
  );
  return tiles;
}

function chipButton(label, key, glyph) {
  const b = element("button", "btn btn-sm chip");
  b.type = "button";
  b.append(icon(glyph, "ic ic-xs"), element("span", "", label));
  b.title = label;
  b.addEventListener("click", () => select(key));
  return b;
}

function focusButton(s) {
  const focused = state.focus === s.service_id;
  const b = element("button", "btn btn-sm", focused ? "Clear focus" : "Focus service");
  b.type = "button";
  b.prepend(icon(focused ? "x" : "focus", "ic ic-sm"));
  b.addEventListener("click", () => focused ? unfocus() : focusService(s.service_id));
  return b;
}

function inspectorHead(tone, kind, title, closable) {
  const h = $("insp-head");
  const titles = element("div", "insp-titles");
  titles.append(element("span", "eyebrow", kind));
  const h2 = element("h2", "insp-title", title);
  h2.id = "insp-title";
  h2.title = title;
  titles.append(h2);
  h.replaceChildren(tone ? toneIcon(tone, "ic insp-ic") : icon("layers", "ic insp-ic tone-faint"), titles);
  if (tone) h.append(badge(tone));
  const close = element("button", "btn btn-ghost btn-icon btn-sm insp-close");
  close.type = "button";
  close.setAttribute("aria-label", closable ? "Clear selection" : "Close inspector");
  close.append(icon("x", "ic"));
  close.addEventListener("click", () => closable ? clearSelection() : closeDrawer());
  close.classList.toggle("drawer-only", !closable);
  h.append(close);
}

function explorerLinks(name, host) {
  const links = [];
  const add = (label, href) => {
    const a = element("a", "insp-link", label);
    a.href = href;
    a.append(icon("arrow-right", "ic ic-xs"));
    links.push(a);
  };
  if (name) {
    add("Root cause in Explorer", "/explorer?" + new URLSearchParams({ service: name, tab: "why" }));
    add("Impact", "/explorer?" + new URLSearchParams({ service: name, tab: "impact" }));
  } else if (host) add("Host in Explorer", "/explorer?" + new URLSearchParams({ host }));
  else add("Open Explorer", "/explorer");
  $("insp-links").replaceChildren(...links);
}

function renderCoverage() {
  const meta = state.scene?.meta;
  const line = $("coverage");
  if (!meta) { line.textContent = ""; return; }
  const parts = [meta.coverage === "limited" ? "Limited coverage" : "Observed coverage"];
  if (finite(meta.resolution_seconds)) parts.push(Math.round(meta.resolution_seconds / 60) + "-min buckets");
  if (meta.truncated) parts.push("result truncated");
  if (meta.flush_failed) parts.push("last flush failed");
  line.textContent = parts.join(" · ");
  line.title = typeof meta.note === "string" ? meta.note : "";
  line.classList.toggle("tone-warn", meta.coverage === "limited" || Boolean(meta.truncated) || Boolean(meta.flush_failed));
}

function setTabs(enabled) {
  $("insp-tabs").hidden = !enabled;
  for (const tab of $("insp-tabs").querySelectorAll("[role=tab]")) {
    const on = tab.dataset.tab === state.tab;
    tab.setAttribute("aria-selected", String(on));
    tab.tabIndex = on ? 0 : -1;
    if (on) $("insp-body").setAttribute("aria-labelledby", tab.id);
  }
  if (!enabled) $("insp-body").setAttribute("aria-labelledby", "insp-title");
}

function renderInspector() {
  const body = $("insp-body");
  const find = lookup();
  // A focused service is the inspector's subject until something else is selected.
  const key = state.selected || (state.focus ? "s:" + state.focus : null);
  const kind = key?.[0], id = key?.slice(2);
  const nameOf = (nodeId) => {
    const n = find.instance(nodeId);
    return n ? shortInstance(n) : find.service(nodeId)?.name || "unknown";
  };
  const serviceName = (serviceId) => find.service(serviceId)?.name || "unknown";
  const parts = [];
  renderCoverage();

  if (kind === "s" && find.service(id)) {
    const s = find.service(id);
    const tone = toneOf(s);
    inspectorHead(tone, "Service", s.name, Boolean(state.selected));
    setTabs(true);
    explorerLinks(s.name);
    if (state.tab === "overview") {
      const actions = element("div", "insp-actions");
      actions.append(focusButton(s), element("span", "insp-meta", fmtNumber(s.instance_count) + (s.instance_count === 1 ? " instance" : " instances") + " · " + fmtNumber(s.hosts.length) + (s.hosts.length === 1 ? " host" : " hosts")));
      parts.push(trafficTiles(s), actions, section("Instances", instanceTable(find.instancesOf(id))), details("Identity & evidence", [
        ["Environment", envLabel(s.environment)], ["Namespace", nsLabel(s.namespace)], ["Service id", s.service_id],
        ["Hosts", s.hosts.join(", ") || "Unknown"], ["Identity", s.identity], ["Unresolved", s.unresolved_count ? fmtNumber(s.unresolved_count) + " identities" : "None"],
        ["Coverage", "Rates count server, consumer and root spans in five-minute buckets. Sparklines use per-minute trace counts."],
      ]));
    } else if (state.tab === "instances") {
      parts.push(instanceTable(find.instancesOf(id)));
    } else {
      parts.push(callList((state.scene?.svcEdges || []).filter((e) => e.source === id || e.target === id).sort(byErrorContribution), serviceName, "e"));
    }
  } else if (kind === "i" && find.instance(id)) {
    const n = find.instance(id);
    const tone = toneOf(n);
    inspectorHead(tone, "Instance · " + n.name, n.instance || shortInstance(n), true);
    setTabs(true);
    explorerLinks(n.name);
    if (state.tab === "overview") {
      const chips = element("div", "insp-actions");
      chips.append(chipButton(n.name, "s:" + n.service_id, "boxes"));
      for (const h of n.hosts) chips.append(chipButton(h, "h:" + h, "server"));
      parts.push(metricTiles(n), chips, details("Identity & evidence", [
        ["service.instance.id", n.instance || "Not reported"], ["Service", n.name], ["Namespace", nsLabel(n.namespace)],
        ["Environment", envLabel(n.environment)], ["Hosts", n.hosts.join(", ") || "Unknown"], ["Identity", n.identity],
        ["Coverage", "Host placement is the latest reported value."],
      ]));
    } else if (state.tab === "instances") {
      parts.push(instanceTable(find.instancesOf(n.service_id)));
    } else {
      parts.push(callList(find.allIEdges().filter((e) => e.source === id || e.target === id).sort(byErrorContribution), nameOf, "x"));
    }
  } else if (kind === "h" && state.scene?.hostChips.some((h) => h.name === id)) {
    const chip = state.scene.hostChips.find((h) => h.name === id);
    const info = chip.info;
    const onHost = find.allInstances().filter((n) => n.hosts.includes(id) && state.scene.services.some((s) => s.service_id === n.service_id))
      .sort((a, b) => toneRank(toneOf(a)) - toneRank(toneOf(b)) || a.name.localeCompare(b.name));
    inspectorHead(chip.tone, "Host", id, true);
    setTabs(true);
    explorerLinks(null, id);
    if (state.tab === "overview") {
      const tiles = element("div", "tiles");
      tiles.append(tile("Services", finite(info?.service_count) ? fmtNumber(info.service_count) : "—", "ok", null, "all environments"),
        tile("Instances", fmtNumber(onHost.length), chip.tone, null, "in this view"),
        tile("Last seen", info?.last_seen ? fmtAgo(info.last_seen) : "—", "ok", null, "latest report"));
      const services = element("ul", "calls");
      const names = info?.services?.length ? info.services : unique(onHost.map((n) => n.name));
      for (const name of names) {
        const s = state.scene.services.find((item) => item.name === name);
        const li = element("li", "call");
        const label = element("span", "call-names");
        label.append(s ? toneIcon(toneOf(s)) : icon("circle-dashed", "ic ic-sm tone-unk"), element("span", "call-a", name));
        li.append(label, element("span", "call-stats num", s ? fmtPercent(s.error_rate) : "out of scope"));
        services.append(s ? selectable(li, "s:" + s.service_id) : li);
      }
      parts.push(tiles, section("Services on this host", services), section("Instances", instanceTable(onHost)));
    } else if (state.tab === "instances") {
      parts.push(instanceTable(onHost));
    } else {
      const ids = new Set(onHost.map((n) => n.id));
      parts.push(callList(find.allIEdges().filter((e) => ids.has(e.source) || ids.has(e.target)).sort(byErrorContribution), nameOf, "x"));
    }
  } else if (kind === "e" && find.edge(id)) {
    const e = find.edge(id);
    const tone = toneFromErrorRate(e.error_rate, e.calls);
    const a = serviceName(e.source), b = serviceName(e.target);
    inspectorHead(tone, "Call", a + " → " + b, true);
    setTabs(true);
    explorerLinks(b);
    const serviceOf = new Map(find.allInstances().map((n) => [n.id, n.service_id]));
    const instanceCalls = find.allIEdges().filter((x) => serviceOf.get(x.source) === e.source && serviceOf.get(x.target) === e.target).sort(byErrorContribution);
    if (state.tab === "overview") {
      const chips = element("div", "insp-actions");
      chips.append(chipButton(a, "s:" + e.source, "boxes"), icon("arrow-right", "ic ic-sm tone-faint"), chipButton(b, "s:" + e.target, "boxes"));
      parts.push(metricTiles({ ...e, requests: e.calls }, "Calls"), chips, details("Identity & evidence", [
        ["Calls", fmtNumber(e.calls)], ["Errors", fmtNumber(e.errors)], ["Instance pairs", fmtNumber(instanceCalls.length)],
        ["Evidence", "Observed trace parent–child spans. Sharing a host never creates a call."],
      ]));
    } else if (state.tab === "instances") {
      parts.push(instanceTable([...find.instancesOf(e.source), ...find.instancesOf(e.target)]));
    } else {
      parts.push(callList(instanceCalls, nameOf, "x"));
    }
  } else if (kind === "x" && find.iedge(id)) {
    const e = find.iedge(id);
    const tone = toneFromErrorRate(e.error_rate, e.calls);
    const a = find.instance(e.source), b = find.instance(e.target);
    inspectorHead(tone, "Instance call", shortInstance(a) + " → " + shortInstance(b), true);
    setTabs(true);
    explorerLinks(b.name);
    if (state.tab === "overview") {
      const chips = element("div", "insp-actions");
      chips.append(chipButton(a.name + " " + shortInstance(a), "i:" + a.id, "box"), icon("arrow-right", "ic ic-sm tone-faint"), chipButton(b.name + " " + shortInstance(b), "i:" + b.id, "box"));
      parts.push(metricTiles({ ...e, requests: e.calls }, "Calls"), chips, details("Identity & evidence", [
        ["Caller", a.instance || shortInstance(a)], ["Caller host", a.hosts.join(", ") || "Unknown"],
        ["Callee", b.instance || shortInstance(b)], ["Callee host", b.hosts.join(", ") || "Unknown"],
        ["Calls", fmtNumber(e.calls)], ["Errors", fmtNumber(e.errors)],
      ]));
    } else if (state.tab === "instances") {
      parts.push(instanceTable([a, b]));
    } else {
      parts.push(callList(find.allIEdges().filter((x) => find.instance(x.source)?.service_id === a.service_id && find.instance(x.target)?.service_id === b.service_id).sort(byErrorContribution), nameOf, "x"));
    }
  } else {
    if (state.selected) state.selected = null;
    const sc = state.scene;
    const focused = state.focus ? find.service(state.focus) : null;
    inspectorHead(null, "Scope · " + envLabel(state.env), focused ? focused.name + " neighbourhood" : state.namespace ? nsLabel(state.namespace) : "All services", false);
    setTabs(false);
    explorerLinks(focused?.name);
    if (!state.svc || state.failure) {
      const tiles = element("div", "tiles");
      tiles.append(tile("Services", "—", "ok"), tile("Instances", "—", "ok"), tile("Hosts", "—", "ok"));
      body.replaceChildren(tiles);
      return;
    }
    const services = sc?.services || [];
    const unhealthy = services.filter((s) => ["crit", "warn"].includes(toneOf(s)))
      .sort((a, b) => toneRank(toneOf(a)) - toneRank(toneOf(b)) || b.error_rate - a.error_rate).slice(0, 6);
    const tiles = element("div", "tiles");
    tiles.append(tile("Services", fmtNumber(services.length), "ok", null, unhealthy.length ? unhealthy.length + " need attention" : "all quiet"),
      tile("Instances", fmtNumber(services.reduce((sum, s) => sum + (s.instance_count || 0), 0)), "ok", null, "known identities"),
      tile("Hosts", fmtNumber(sc?.hostChips.length || 0), "ok", null, "latest placement"));
    const attention = element("ul", "calls");
    for (const s of unhealthy) {
      const li = element("li", "call");
      const label = element("span", "call-names");
      label.append(toneIcon(toneOf(s)), element("span", "call-a", s.name), element("span", "call-ns", s.namespace));
      label.title = s.name;
      const stats = element("span", "call-stats num");
      stats.append(element("span", "tone-" + toneOf(s), fmtPercent(s.error_rate)), element("span", "", fmtMs(s.avg_latency_ms)));
      li.append(label, stats);
      attention.append(selectable(li, "s:" + s.service_id));
    }
    const nameById = (sid) => serviceName(sid);
    const hot = (sc?.svcEdges || []).filter((e) => e.errors > 0).sort(byErrorContribution).slice(0, 6);
    parts.push(tiles,
      section("Needs attention", unhealthy.length ? attention : element("p", "insp-empty", "No degraded or critical services in this scope.")),
      section("Hot connections", hot.length ? callList(hot, nameById, "e") : element("p", "insp-empty", "No failing calls in this scope.")));
  }
  body.replaceChildren(...parts);
}

const trafficKey = (name) => [name, state.start, state.end].join("|");

async function loadTraffic(name) {
  const key = trafficKey(name);
  if (state.traffic.has(key)) return;
  state.trafficController?.abort();
  const controller = new AbortController();
  state.trafficController = controller;
  try {
    const series = await getJSON("/api/metrics/traffic?" + new URLSearchParams({ start: state.start, end: state.end, service_name: name }), controller.signal);
    if (!Array.isArray(series)) return;
    state.traffic.set(key, series.filter((p) => p && finite(p.count) && finite(p.error_count)).slice(-240));
    const subject = state.selected || (state.focus ? "s:" + state.focus : "");
    if (subject.startsWith("s:") && lookup().service(subject.slice(2))?.name === name) renderInspector();
  } catch {
    // Sparklines are optional; the tiles already carry the topology values.
  }
}

// ---------- Selection, focus and drawers ----------

function select(key, { open = true } = {}) {
  state.selected = key;
  paint();
  renderInspector();
  if (key.startsWith("s:")) {
    const s = lookup().service(key.slice(2));
    if (s) loadTraffic(s.name);
  }
  if (open && mqInspector.matches) openDrawer("inspector");
}

function clearSelection() {
  const lost = $("inspector").contains(document.activeElement) || document.activeElement === document.body;
  state.selected = null;
  state.hover = null;
  state.tab = "overview";
  paint();
  renderInspector();
  if (state.drawer === "inspector") closeDrawer();
  else if (lost) canvas.focus();
}

function focusService(id) {
  state.focus = id;
  state.selected = null;
  state.tab = "overview";
  state.mode = "instances";
  state.notice = "";
  closeDrawer();
  writeURL();
  load({ full: false, refit: true });
}

function unfocus() {
  // A pending focus read is obsolete; a full refresh re-reads state.focus when it lands.
  if (state.loading && !state.loadingFull) {
    state.gen++;
    state.controller?.abort();
    state.loading = false;
    canvas.removeAttribute("aria-busy");
  }
  state.focus = "";
  state.focusGraph = null;
  renderAll({ refit: true });
}

// The rail folds away at any width so the canvas can take the room; phones and tablets use a drawer instead.
function applyRail() {
  $("main").classList.toggle("rail-collapsed", state.railCollapsed);
  const open = mqRail.matches ? state.drawer === "rail" : !state.railCollapsed;
  $("rail-open").setAttribute("aria-expanded", String(open));
  $("rail-open").setAttribute("aria-label", open ? "Hide layers" : "Show layers");
}

function toggleRail(open = mqRail.matches ? state.drawer !== "rail" : state.railCollapsed) {
  if (mqRail.matches) return open ? openDrawer("rail") : closeDrawer();
  state.railCollapsed = !open;
  try { localStorage.setItem(RAIL_KEY, state.railCollapsed ? "collapsed" : "open"); } catch { /* preference is optional */ }
  applyRail();
}

function drawerPanel(name) {
  return name === "rail" ? $("rail") : $("inspector");
}

function openDrawer(name) {
  if (state.drawer === name) return;
  if (state.drawer) closeDrawer(false);
  state.drawer = name;
  state.opener = document.activeElement;
  $("main").classList.add(name + "-open");
  $("scrim").hidden = false;
  syncDrawers();
  if (name === "inspector") $("insp-open").setAttribute("aria-expanded", "true");
  applyRail();
  const panel = drawerPanel(name);
  requestAnimationFrame(() => panel.querySelector(".insp-close, #rail-close, button, a, input")?.focus());
}

function closeDrawer(restore = true) {
  if (!state.drawer) return;
  const name = state.drawer;
  state.drawer = null;
  $("main").classList.remove(name + "-open");
  $("scrim").hidden = true;
  if (name === "inspector") $("insp-open").setAttribute("aria-expanded", "false");
  applyRail();
  syncDrawers();
  if (restore && state.opener?.isConnected) state.opener.focus();
  else if (restore) canvas.focus();
  state.opener = null;
}

function syncDrawers() {
  if (state.drawer === "rail" && !mqRail.matches) closeDrawer(false);
  if (state.drawer === "inspector" && !mqInspector.matches) closeDrawer(false);
  $("rail").inert = mqRail.matches && state.drawer !== "rail";
  $("inspector").inert = mqInspector.matches && state.drawer !== "inspector";
  $("inspector").classList.toggle("is-drawer", mqInspector.matches);
}

// The environment control moves into the rail where the top bar has no room.
function placeEnvironment() {
  const field = $("env-field");
  if (mqCompact.matches) $("rail-env").append(field);
  else document.querySelector(".topbar-tools").prepend(field);
  $("rail-env").hidden = !mqCompact.matches;
}

// ---------- Render all ----------

function renderAll({ refit = false } = {}) {
  renderStatus();
  // A shared link may name a namespace this range no longer has.
  if (state.namespace && !state.svc?.nodes.some((n) => n.namespace === state.namespace)) state.namespace = "";
  const empty = state.svc && !state.svc.nodes.length && !state.focusGraph?.nodes.length;
  if (state.failure) {
    const unavailable = state.failure.status === 503 && /unavailable/i.test(state.failure.message);
    showState(unavailable ? "unavailable" : "error", state.failure);
  } else if (empty) {
    showState(state.catalog?.services.length ? "quiet" : "empty");
  } else if (state.svc) {
    showState(null);
  }
  if (state.svc && !state.failure && !empty) {
    const before = state.sceneKey;
    layoutAndFit(refit || !state.scene);
    if (state.scene) state.sceneKey = sceneKey(state.scene);
    if (!refit && before && before !== state.sceneKey) layoutAndFit(true);
  }
  renderControls();
  renderRail();
  renderInspector();
  const note = $("map-note");
  const meta = state.scene?.meta;
  const instances = state.mode === "instances";
  const message = state.notice || (instances && meta?.truncated ?
    "Truncated: " + fmtNumber(state.scene.pills.length) + " of " + fmtNumber(meta.available_nodes) + " instances, " +
      fmtNumber(state.scene.iedges.length) + " call edges. " + (state.focus ? "Use 1 hop or a shorter range." : "Focus a service for complete instance calls.") : "");
  note.textContent = message;
  note.hidden = !message || Boolean(state.failure);
  writeURL();
  const subject = state.selected || (state.focus ? "s:" + state.focus : "");
  if (subject.startsWith("s:")) {
    const s = lookup().service(subject.slice(2));
    if (s) loadTraffic(s.name);
  }
}

// ---------- Events ----------

function hoverKey(target) {
  const el = target?.closest?.("[data-key]");
  if (!el) return null;
  const key = el.dataset.key;
  if (key.startsWith("g:")) return "s:" + key.slice(2);
  return key.startsWith("r:") ? null : key;
}

map.addEventListener("pointerover", (e) => {
  if (e.pointerType === "touch" || dragging) return;
  const key = hoverKey(e.target);
  if (key === state.hover) return;
  state.hover = key;
  paint();
});
map.addEventListener("pointerleave", () => { if (state.hover) { state.hover = null; paint(); } });
map.addEventListener("focusin", (e) => { state.hover = hoverKey(e.target); paint(); });
map.addEventListener("focusout", () => { state.hover = null; paint(); });
map.addEventListener("click", (e) => {
  const key = hoverKey(e.target);
  if (key) select(key);
  else if (state.selected) clearSelection();
});
map.addEventListener("dblclick", (e) => {
  const key = hoverKey(e.target);
  if (key?.startsWith("s:")) focusService(key.slice(2));
});
map.addEventListener("keydown", (e) => {
  const key = hoverKey(e.target);
  if (!key || (e.key !== "Enter" && e.key !== " ")) return;
  e.preventDefault();
  select(key);
});

// Pan with one pointer, pinch with two. Capture only once a drag starts so clicks still land on nodes.
const pointers = new Map();
let dragging = false, gesture = null;
canvas.addEventListener("pointerdown", (e) => {
  if (e.button !== 0 || e.target.closest(".zoom, .map-state, .map-note")) return;
  pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
  const points = [...pointers.values()];
  const view = { ...state.view };
  gesture = points.length === 2 ? {
    pinch: true, view, distance: Math.hypot(points[0].x - points[1].x, points[0].y - points[1].y) || 1,
    mid: { x: (points[0].x + points[1].x) / 2, y: (points[0].y + points[1].y) / 2 },
  } : { pinch: false, view, x: e.clientX, y: e.clientY };
});
canvas.addEventListener("pointermove", (e) => {
  if (!pointers.has(e.pointerId) || !gesture) return;
  pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
  const rect = canvas.getBoundingClientRect();
  if (gesture.pinch && pointers.size === 2) {
    const [a, b] = [...pointers.values()];
    const k = clamp(gesture.view.k * Math.hypot(a.x - b.x, a.y - b.y) / gesture.distance, 0.15, 2.5);
    const mid = { x: (a.x + b.x) / 2 - rect.left, y: (a.y + b.y) / 2 - rect.top };
    const origin = { x: gesture.mid.x - rect.left, y: gesture.mid.y - rect.top };
    const f = k / gesture.view.k;
    state.view = { k, x: mid.x - (origin.x - gesture.view.x) * f, y: mid.y - (origin.y - gesture.view.y) * f };
    dragging = true;
    applyView();
    return;
  }
  const dx = e.clientX - gesture.x, dy = e.clientY - gesture.y;
  if (!dragging && Math.hypot(dx, dy) < 4) return;
  if (!dragging) {
    dragging = true;
    canvas.setPointerCapture(e.pointerId);
    canvas.classList.add("is-panning");
    if (state.hover) { state.hover = null; paint(); }
  }
  state.view = { ...gesture.view, x: gesture.view.x + dx, y: gesture.view.y + dy };
  applyView();
});
for (const type of ["pointerup", "pointercancel"]) canvas.addEventListener(type, (e) => {
  pointers.delete(e.pointerId);
  if (pointers.size) return;
  gesture = null;
  canvas.classList.remove("is-panning");
  if (dragging) setTimeout(() => { dragging = false; });
});
canvas.addEventListener("click", (e) => { if (dragging) e.stopPropagation(); }, true);
canvas.addEventListener("wheel", (e) => {
  e.preventDefault();
  const rect = canvas.getBoundingClientRect();
  zoomAt(Math.exp(-e.deltaY * (e.ctrlKey ? 0.01 : 0.0015)), e.clientX - rect.left, e.clientY - rect.top);
}, { passive: false });
canvas.addEventListener("keydown", (e) => {
  if (e.target !== canvas) return;
  const pan = { ArrowLeft: [64, 0], ArrowRight: [-64, 0], ArrowUp: [0, 64], ArrowDown: [0, -64] }[e.key];
  if (pan) {
    e.preventDefault();
    state.view = { ...state.view, x: state.view.x + pan[0], y: state.view.y + pan[1] };
    applyView();
  } else if (e.key === "+" || e.key === "=") zoomCenter(1.2);
  else if (e.key === "-") zoomCenter(1 / 1.2);
  else if (e.key === "0") layoutAndFit(true);
});

$("zoom-in").addEventListener("click", () => zoomCenter(1.2));
$("zoom-out").addEventListener("click", () => zoomCenter(1 / 1.2));
$("zoom-fit").addEventListener("click", () => layoutAndFit(true));
$("refresh").addEventListener("click", () => load());
$("env").addEventListener("change", () => {
  state.env = $("env").value === "*" ? undefined : JSON.parse($("env").value);
  state.focus = "";
  state.selected = null;
  load();
});
for (const b of document.querySelectorAll("[data-range]")) b.addEventListener("click", () => {
  if (state.range === b.dataset.range) return;
  state.range = b.dataset.range;
  load();
});
for (const b of document.querySelectorAll("[data-mode]")) b.addEventListener("click", () => {
  if (state.mode === b.dataset.mode) return;
  state.mode = b.dataset.mode;
  renderAll({ refit: true });
});
$("hops").addEventListener("change", () => {
  state.hops = Number($("hops").value) === 2 ? 2 : 1;
  writeURL();
  load({ full: false });
});
$("search").addEventListener("input", () => { state.query = $("search").value; renderResults(); });
$("search").addEventListener("keydown", (e) => {
  if (e.key !== "Enter") return;
  const first = $("results").querySelector("button");
  if (first) { e.preventDefault(); first.click(); }
});

const tabs = [...$("insp-tabs").querySelectorAll("[role=tab]")];
for (const tab of tabs) {
  tab.addEventListener("click", () => { state.tab = tab.dataset.tab; renderInspector(); });
  tab.addEventListener("keydown", (e) => {
    const index = tabs.indexOf(tab);
    const next = { ArrowRight: index + 1, ArrowLeft: index - 1, Home: 0, End: tabs.length - 1 }[e.key];
    if (next === undefined) return;
    e.preventDefault();
    const target = tabs[(next + tabs.length) % tabs.length];
    state.tab = target.dataset.tab;
    renderInspector();
    target.focus();
  });
}

$("rail-open").addEventListener("click", () => toggleRail());
$("rail-close").addEventListener("click", () => toggleRail(false));
$("insp-open").addEventListener("click", () => state.drawer === "inspector" ? closeDrawer() : openDrawer("inspector"));
$("scrim").addEventListener("click", () => closeDrawer());

document.addEventListener("keydown", (e) => {
  const typing = e.target.closest?.("input, select, textarea");
  if (e.key === "/" && !typing && !e.metaKey && !e.ctrlKey) {
    e.preventDefault();
    toggleRail(true);
    requestAnimationFrame(() => $("search").focus());
  } else if (e.key === "Escape") {
    if (typing === $("search") && $("search").value) return;
    if (state.drawer) closeDrawer();
    else if (state.selected) clearSelection();
  }
});

for (const mq of [mqInspector, mqRail]) mq.addEventListener("change", () => { syncDrawers(); applyRail(); });
mqCompact.addEventListener("change", placeEnvironment);

let resizeTimer = 0;
new ResizeObserver(() => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => { if (state.scene) layoutAndFit(true); }, 120);
}).observe(canvas);

setInterval(() => {
  renderStatus();
  if (!document.hidden && !state.loading && state.updated && Date.now() - state.updated >= REFRESH_MS) load({ refit: false });
}, 5000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && state.updated && Date.now() - state.updated >= REFRESH_MS && !state.loading) load({ refit: false });
});

initShell();
readURL();
try {
  const stored = localStorage.getItem(RAIL_KEY);
  state.railCollapsed = stored ? stored === "collapsed" : innerWidth < 1600;
} catch {
  state.railCollapsed = innerWidth < 1600;
}
applyRail();
placeEnvironment();
syncDrawers();
renderRail();
renderInspector();
document.fonts.load(FONT.name).then(() => document.fonts.load(FONT.mono)).catch(() => null).finally(() => load());

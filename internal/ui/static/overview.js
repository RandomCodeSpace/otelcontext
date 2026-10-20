import {
  $, element, svg, icon, toneFromStatus, toneFromErrorRate, toneRank, toneIcon, badge, TONE_LABEL,
  fmtNumber, fmtCompact, fmtPercent, fmtMs, fmtAgo, sparkline, fetchJSON, initShell,
} from "./shell.js";

const RANGES = { "15m": [15, "last 15 minutes"], "1h": [60, "last hour"], "6h": [360, "last 6 hours"], "24h": [1440, "last 24 hours"] };
const POLL_MS = 30000;
const RANGED = ["dash", "traffic", "catalog", "services", "instances"];
const PIP_CAP = 12;
const GOLDEN = Math.PI * (3 - Math.sqrt(5));
// Pip order puts failing instances first so a cap never hides them.
const PIP_ORDER = { crit: 0, warn: 1, ok: 2, unk: 3 };

const params = new URLSearchParams(location.search);
const state = {
  range: RANGES[params.get("range")] ? params.get("range") : "1h",
  gen: 0,
  controller: null,
  data: {},
  errors: {},
  trafficCoverage: "",
  updatedAt: 0,
};

const finite = (n) => typeof n === "number" && Number.isFinite(n);
const loading = (key) => state.data[key] === undefined && !state.errors[key];
const unavailable = (key) => state.errors[key]?.status === 503;
const plural = (n, word) => fmtNumber(n) + " " + word + (n === 1 ? "" : "s");
const explorerWhy = (name) => "/explorer?service=" + encodeURIComponent(name) + "&tab=why";

// ---------- Data ----------

function rangeQuery() {
  const end = new Date();
  const start = new Date(end.getTime() - RANGES[state.range][0] * 60000);
  return "start=" + encodeURIComponent(start.toISOString()) + "&end=" + encodeURIComponent(end.toISOString());
}

async function refresh() {
  const gen = ++state.gen;
  state.controller?.abort();
  const controller = new AbortController();
  state.controller = controller;
  const q = rangeQuery();
  const sources = {
    sys: "/api/system/graph",
    dash: "/api/metrics/dashboard?" + q,
    traffic: "/api/metrics/traffic?" + q,
    runtime: "/api/stats/runtime",
    catalog: "/api/topology/catalog?" + q,
    services: "/api/topology/graph?mode=services&limit=100&" + q,
    instances: "/api/topology/graph?mode=instances&limit=100&" + q,
  };
  const keys = Object.keys(sources);
  const results = await Promise.allSettled(keys.map((key) => fetchJSON(sources[key], { signal: controller.signal })));
  if (gen !== state.gen) return;
  let ok = 0;
  results.forEach((result, i) => {
    const key = keys[i];
    if (result.status === "fulfilled") {
      ok++;
      state.data[key] = result.value.data;
      state.errors[key] = null;
      if (key === "traffic") state.trafficCoverage = result.value.headers.get("OtelContext-Data-Coverage") || "";
    } else {
      // fetchJSON aborts on its own timeout; a superseded refresh never reaches here.
      state.errors[key] = result.reason?.name === "AbortError" ? new Error("Request timed out.") : result.reason;
    }
  });
  if (ok) state.updatedAt = Date.now();
  render();
}

function setRange(range) {
  if (range === state.range || !RANGES[range]) return;
  state.range = range;
  for (const key of RANGED) { delete state.data[key]; state.errors[key] = null; }
  const url = new URL(location.href);
  url.searchParams.set("range", range);
  history.replaceState(null, "", url);
  render();
  refresh();
}

// Joined views over the raw responses, rebuilt on each render.
function model() {
  const sys = state.data.sys;
  const services = (sys?.nodes || []).filter((n) => n.kind !== "host").map((n) => ({
    name: n.id,
    tone: toneFromStatus(n.status),
    m: n.metrics || {},
    alerts: n.alerts || [],
    hosts: n.hosts || [],
    hostCount: n.host_count,
  })).sort((a, b) => toneRank(a.tone) - toneRank(b.tone) || (b.m.error_rate || 0) - (a.m.error_rate || 0) || a.name.localeCompare(b.name));

  const instTone = new Map();
  const instHosts = new Map();
  for (const node of state.data.instances?.nodes || []) {
    instTone.set(node.id, toneFromErrorRate(node.error_rate, node.requests));
    instHosts.set(node.id, node.hosts || []);
  }
  // A service may appear once per environment in the catalog; merge by name.
  const catalog = new Map();
  for (const entry of state.data.catalog?.services || []) {
    const item = catalog.get(entry.name) || { namespace: entry.namespace || "default", ids: [] };
    item.ids.push(...(entry.instances || []));
    catalog.set(entry.name, item);
  }
  return { services, catalog, instTone, instHosts };
}

// ---------- Render ----------

function render() {
  const m = model();
  renderRange();
  renderHeader(m);
  renderKpis(m);
  renderAttention(m);
  renderAtlas(m);
  renderTable(m);
  renderStorage();
  renderLive();
}

function renderRange() {
  $("range-label").textContent = "· " + RANGES[state.range][1];
  for (const button of $("range-seg").querySelectorAll("button")) {
    button.setAttribute("aria-pressed", String(button.dataset.range === state.range));
  }
}

function nameList(names) {
  const shown = names.slice(0, 3);
  const parts = [];
  shown.forEach((name, i) => {
    if (i) parts.push(i === shown.length - 1 && names.length <= 3 ? " and " : ", ");
    parts.push(element("strong", "", name));
  });
  if (names.length > 3) parts.push(" and " + (names.length - 3) + " more");
  return parts;
}

function renderHeader(m) {
  const summary = $("summary");
  const investigate = $("investigate");
  if (loading("sys")) return;
  if (!state.data.sys) {
    summary.replaceChildren(element("span", "tone-crit", "Service health is unavailable: " + state.errors.sys.message));
    investigate.hidden = true;
    return;
  }
  const total = m.services.length;
  const crit = m.services.filter((s) => s.tone === "crit").map((s) => s.name);
  const warn = m.services.filter((s) => s.tone === "warn").map((s) => s.name);
  const healthy = m.services.filter((s) => s.tone === "ok").length;
  const parts = [];
  if (!total) parts.push("No services are reporting yet. Send OTLP traces to see reliability here.");
  else if (!crit.length && !warn.length) parts.push("All " + plural(total, "service") + " healthy.");
  else {
    parts.push(fmtNumber(healthy) + " of " + plural(total, "service") + " healthy. ");
    if (crit.length) parts.push(...nameList(crit), (crit.length === 1 ? " is" : " are") + " critical");
    if (crit.length && warn.length) parts.push("; ");
    if (warn.length) parts.push(...nameList(warn), (warn.length === 1 ? " is" : " are") + " degraded");
    parts.push(".");
  }
  if (state.data.sys.truncated) parts.push(element("span", "tone-faint", " Partial view: the service graph was truncated."));
  summary.replaceChildren(...parts);

  const worst = m.services.find((s) => s.tone === "crit" || s.tone === "warn");
  investigate.hidden = !worst;
  if (worst) {
    investigate.href = explorerWhy(worst.name);
    $("investigate-label").textContent = "Investigate " + worst.name;
    investigate.title = "Open root cause analysis for " + worst.name;
  }
}

function setKpi(id, value, context, aside) {
  $(id + "-value").replaceChildren(...[].concat(value));
  $(id + "-context").replaceChildren(...[].concat(context));
  $(id + "-aside").replaceChildren(...[].concat(aside || []));
}

function kpiError(id, key, label) {
  const error = state.errors[key];
  setKpi(id, "—", unavailable(key) ? element("span", "tone-faint", label) : element("span", "tone-crit", "Unavailable: " + (error?.message || "request failed")));
}

function trafficSpark(values, tone, caption) {
  const wrap = element("figure", "kpi-spark");
  wrap.append(sparkline(values, { width: 88, height: 24, tone, label: caption + ", " + RANGES[state.range][1] }));
  wrap.append(element("figcaption", "", caption));
  return wrap;
}

function renderKpis(m) {
  const traffic = Array.isArray(state.data.traffic) ? state.data.traffic : [];
  const sampled = state.trafficCoverage && state.trafficCoverage !== "full" ? " · " + state.trafficCoverage : "";

  // Healthy services, with request volume as the honest traffic signal.
  if (loading("sys")) { /* keep skeleton */ }
  else if (!state.data.sys) kpiError("kpi-health", "sys");
  else {
    const total = m.services.length;
    const healthy = m.services.filter((s) => s.tone === "ok").length;
    const attention = total - healthy;
    setKpi("kpi-health",
      [fmtNumber(healthy), element("span", "kpi-of", "/" + fmtNumber(total))],
      !total ? "No services reporting" : attention ? plural(attention, "service") + " need attention" : "Every service is healthy",
      traffic.length > 1 ? trafficSpark(traffic.map((b) => b.count), "ok", "requests" + sampled) : null);
  }

  // Error rate: dashboard is a percent; the system graph fallback is a ratio.
  const dash = state.data.dash;
  if (loading("dash") && loading("sys")) { /* keep skeleton */ }
  else if (dash || state.data.sys) {
    const ratio = dash && finite(dash.error_rate) ? dash.error_rate / 100 : state.data.sys?.system?.total_error_rate;
    const requests = dash?.requests || dash?.total_traces;
    const errors = dash?.request_errors ?? dash?.total_errors;
    const context = dash ? plural(errors || 0, "error") + " in " + fmtCompact(requests || 0) + " requests" : "Service average, live window";
    const rates = traffic.map((b) => (b.count ? b.error_count / b.count : 0));
    setKpi("kpi-errors", fmtPercent(ratio, 2), dash?.coverage && dash.coverage !== "full" ? [context, element("span", "tone-faint", " · " + dash.coverage)] : context,
      traffic.length > 1 ? trafficSpark(rates, toneFromErrorRate(ratio, 1), "error rate" + sampled) : null);
  } else kpiError("kpi-errors", "dash");

  // p99 latency: no latency series exists, so provenance takes the aside slot.
  if (loading("dash")) { /* keep skeleton */ }
  else if (!dash) kpiError("kpi-latency", "dash");
  else {
    const p = dash.latency_provenance?.p99;
    const approx = p?.status === "approximate";
    const context = ["avg " + fmtMs(dash.avg_latency_ms)];
    if (p?.sample_count) context.push(" · " + fmtCompact(p.sample_count) + " samples");
    if (approx && finite(p.relative_error_bound)) context.push(" · ±" + fmtPercent(p.relative_error_bound, 1));
    const aside = p ? element("span", "badge", approx ? "approx." : p.status || "") : null;
    if (aside) aside.title = p.method ? "p99 " + p.status + " (" + p.method + ")" : "";
    setKpi("kpi-latency", dash.p99_latency_ms ? fmtMs(dash.p99_latency_ms) : "—", context, aside);
  }

  // Instances from the topology catalog.
  const catalog = state.data.catalog;
  if (loading("catalog")) { /* keep skeleton */ }
  else if (!catalog) kpiError("kpi-instances", "catalog", "Instance topology unavailable on this storage driver");
  else {
    const counts = { crit: 0, warn: 0, ok: 0, unk: 0 };
    for (const tone of m.instTone.values()) counts[tone]++;
    const failing = counts.crit + counts.warn;
    const context = ["across " + plural(catalog.hosts, "host")];
    if (failing) context.push(element("span", counts.crit ? "tone-crit" : "tone-warn", " · " + fmtNumber(failing) + " unhealthy"));
    if (catalog.unresolved) context.push(" · " + fmtNumber(catalog.unresolved) + " unresolved");
    if (catalog.truncated) context.push(element("span", "tone-faint", " · truncated"));
    setKpi("kpi-instances", fmtNumber(catalog.instances), context, m.instTone.size ? healthBar(counts) : null);
  }
}

// healthBar is a stacked share of observed instance health.
function healthBar(counts) {
  const total = counts.crit + counts.warn + counts.ok + counts.unk;
  const bar = element("div", "kpi-bar");
  bar.setAttribute("role", "img");
  bar.setAttribute("aria-label", "Observed instance health: " + ["crit", "warn", "ok", "unk"].filter((t) => counts[t]).map((t) => counts[t] + " " + TONE_LABEL[t].toLowerCase()).join(", "));
  bar.title = bar.getAttribute("aria-label");
  for (const tone of ["crit", "warn", "ok", "unk"]) {
    if (!counts[tone]) continue;
    const part = element("i", tone);
    part.style.flexGrow = String(counts[tone] / total);
    bar.append(part);
  }
  const wrap = element("figure", "kpi-spark");
  wrap.append(bar, element("figcaption", "", "instance health"));
  return wrap;
}

function stateBox(kind, title, text, action) {
  const box = element("div", "state state-" + kind);
  if (kind === "loading") box.append(element("div", "spinner"));
  else box.append(icon(kind === "error" ? "triangle-alert" : kind === "ok" ? "circle-check" : "info", "ic state-ic" + (kind === "error" ? " tone-crit" : kind === "ok" ? " tone-ok" : "")));
  box.append(element("b", "", title));
  if (text) box.append(element("p", "", text));
  if (action) box.append(action);
  return box;
}

function retryButton() {
  const button = element("button", "btn btn-sm", "Retry");
  button.type = "button";
  button.addEventListener("click", refresh);
  return button;
}

function errorState(key, what) {
  return stateBox("error", "Couldn't load " + what, state.errors[key]?.message || "The request failed.", retryButton());
}

// ---------- Needs attention ----------

function attentionSentence(s, m) {
  const parts = [];
  if (finite(s.m.error_rate)) parts.push(fmtPercent(s.m.error_rate) + " errors");
  if (finite(s.m.p99_latency_ms) && s.m.p99_latency_ms > 0) parts.push("p99 " + fmtMs(s.m.p99_latency_ms));
  const out = [parts.length ? parts.join(", ") + "." : "No request metrics in the live window."];
  const host = (name) => element("span", "att-host", name);
  const ids = m.catalog.get(s.name)?.ids || [];
  const failing = ids.filter((id) => m.instTone.get(id) === "crit");
  const degraded = ids.filter((id) => m.instTone.get(id) === "warn");
  const lead = failing.length ? failing : degraded;
  if (lead.length) {
    const hosts = [...new Set(lead.flatMap((id) => m.instHosts.get(id) || []))];
    out.push(" " + fmtNumber(lead.length) + " of " + plural(ids.length, "instance") + (failing.length ? " failing" : " degraded"));
    if (lead.length < ids.length && hosts.length === 1) out.push(lead.length === 1 ? ", on " : lead.length === 2 ? ", both on " : ", all on ", host(hosts[0]));
    else if (lead.length < ids.length && hosts.length > 1) out.push(" on " + plural(hosts.length, "host"));
    out.push(".");
    if (failing.length && degraded.length) out.push(" " + fmtNumber(degraded.length) + " more degraded.");
  } else if (s.hostCount) out.push(" Across " + plural(s.hostCount, "host") + ".");
  return out;
}

function attentionCard(s, m) {
  const card = element("article", "panel att-card att-" + s.tone);
  const top = element("div", "att-top");
  const name = element("h3", "att-name", s.name);
  name.title = s.name;
  top.append(toneIcon(s.tone), name, badge(s.tone));
  const text = element("p", "att-text");
  text.append(...attentionSentence(s, m));
  if (s.alerts.length) text.title = "Alerts: " + s.alerts.join("; ");
  const links = element("div", "att-links");
  const why = element("a", "", "Root cause");
  why.href = explorerWhy(s.name);
  why.setAttribute("aria-label", "Root cause for " + s.name);
  const inst = element("a", "", "Instances");
  inst.href = "/service-map?service_name=" + encodeURIComponent(s.name);
  inst.setAttribute("aria-label", "Instances of " + s.name);
  links.append(why, inst);
  card.append(top, text, links);
  return card;
}

function skeletonCard() {
  const card = element("div", "panel att-card");
  card.setAttribute("aria-hidden", "true");
  card.append(element("div", "att-top", ""), element("p", "att-text", ""), element("div", "att-links", ""));
  card.children[0].append(element("span", "skeleton sk-line", "service name"));
  card.children[1].append(element("span", "skeleton sk-line", "Loading the worst services"));
  card.children[2].append(element("span", "skeleton sk-line", "Root cause"));
  return card;
}

function renderAttention(m) {
  const list = $("attention-list");
  const count = $("attention-count");
  if (loading("sys")) {
    list.replaceChildren(skeletonCard(), skeletonCard(), skeletonCard(), skeletonCard());
    return;
  }
  if (!state.data.sys) {
    count.hidden = true;
    const card = element("div", "panel att-card att-calm");
    card.append(errorState("sys", "service health"));
    list.replaceChildren(card);
    return;
  }
  const unhealthy = m.services.filter((s) => s.tone === "crit" || s.tone === "warn");
  count.hidden = !unhealthy.length;
  count.className = "badge " + (unhealthy.some((s) => s.tone === "crit") ? "crit" : "warn");
  count.textContent = String(unhealthy.length);
  count.setAttribute("aria-label", plural(unhealthy.length, "service") + " need attention");
  if (unhealthy.length) {
    list.replaceChildren(...unhealthy.slice(0, 4).map((s) => attentionCard(s, m)));
    return;
  }
  const calm = element("div", "panel att-card att-calm");
  const top = element("div", "att-top");
  top.append(toneIcon(m.services.length ? "ok" : "unk"), element("h3", "att-name", m.services.length ? "All " + plural(m.services.length, "service") + " healthy" : "Waiting for services"));
  calm.append(top, element("p", "att-text", m.services.length
    ? "No service is degraded or critical right now. Services that cross an error or latency threshold appear here first."
    : "Services appear here once OTLP traces arrive."));
  list.replaceChildren(calm);
}

// ---------- Atlas: one disc per namespace ----------

function renderAtlas(m) {
  const host = $("atlas");
  if (loading("catalog")) return host.replaceChildren(stateBox("loading", "Loading domains"));
  if (unavailable("catalog")) {
    const link = element("a", "btn btn-sm", "Open Explorer");
    link.href = "/explorer";
    return host.replaceChildren(stateBox("info", "Topology unavailable on this storage driver", "Instance topology runs on SQLite and PostgreSQL. The Explorer still maps service dependencies.", link));
  }
  if (!state.data.catalog) return host.replaceChildren(errorState("catalog", "the domain map"));

  const tones = new Map(m.services.map((s) => [s.name, s.tone]));
  const domains = new Map();
  for (const [name, item] of m.catalog) {
    const d = domains.get(item.namespace) || { name: item.namespace, counts: { crit: 0, warn: 0, ok: 0, unk: 0 }, total: 0 };
    d.counts[tones.get(name) || "unk"]++;
    d.total++;
    domains.set(item.namespace, d);
  }
  if (!domains.size) return host.replaceChildren(stateBox("info", "No domains yet", "Domains appear once services report a namespace or any traffic."));

  // Aggregate service edges into namespace pairs.
  const nsById = new Map((state.data.services?.nodes || []).map((n) => [n.id, n.namespace || "default"]));
  const pairs = new Map();
  for (const e of state.data.services?.edges || []) {
    const a = nsById.get(e.source);
    const b = nsById.get(e.target);
    if (!a || !b || a === b || !domains.has(a) || !domains.has(b)) continue;
    const key = [a, b].sort().join("\u0000");
    const pair = pairs.get(key) || { a: domains.get(a < b ? a : b), b: domains.get(a < b ? b : a), calls: 0, errors: 0 };
    pair.calls += e.calls || 0;
    pair.errors += e.errors || 0;
    pairs.set(key, pair);
  }
  const maxCalls = Math.max(1, ...[...pairs.values()].map((p) => p.calls));

  // The svg leaves a bottom band for the legend; lay out in its pixel box.
  const width = host.clientWidth;
  const height = host.clientHeight - 32;
  if (width <= 0 || height <= 0) return;
  for (const d of domains.values()) d.degree = 0;
  for (const p of pairs.values()) { p.a.degree += p.calls; p.b.degree += p.calls; }
  // Busiest domains go first so they sit at the centre of the spiral.
  const nodes = [...domains.values()].sort((a, b) => b.degree - a.degree || b.total - a.total || a.name.localeCompare(b.name));
  const maxTotal = Math.max(...nodes.map((d) => d.total));
  const rMax = Math.max(22, Math.min(44, Math.min(width, height) * 0.1));
  for (const d of nodes) {
    d.r = Math.max(14, rMax * Math.sqrt(d.total / maxTotal));
    d.label = d.name.length > 18 ? d.name.slice(0, 17) + "…" : d.name;
  }
  layoutDiscs(nodes, width / height, Math.max(28, rMax * 0.8));

  const pad = 16;
  const minX = Math.min(...nodes.map((d) => d.x - halfWidth(d))) - pad;
  const maxX = Math.max(...nodes.map((d) => d.x + halfWidth(d))) + pad;
  const minY = Math.min(...nodes.map((d) => d.y - d.r)) - pad;
  const maxY = Math.max(...nodes.map((d) => d.y + d.r + 18)) + pad;
  // Never scale up: small layouts stay at pixel size, centred.
  const vw = Math.max(maxX - minX, width);
  const vh = Math.max(maxY - minY, height);
  const cx = (minX + maxX) / 2;
  const cy = (minY + maxY) / 2;
  const root = svg("svg", { class: "atlas-svg", viewBox: [cx - vw / 2, cy - vh / 2, vw, vh].map((v) => v.toFixed(1)).join(" "), preserveAspectRatio: "xMidYMid meet", role: "group", "aria-label": "Service domains, sized by service count, ringed by health" });

  const edges = svg("g", { class: "atlas-edges" });
  for (const p of pairs.values()) {
    const rate = p.calls ? p.errors / p.calls : 0;
    const line = svg("line", { x1: p.a.x, y1: p.a.y, x2: p.b.x, y2: p.b.y, "stroke-width": (1 + 3 * Math.sqrt(p.calls / maxCalls)).toFixed(2), class: rate >= 0.05 ? "atlas-edge edge-hot" : "atlas-edge" });
    line.append(svg("title", {}, p.a.name + " ↔ " + p.b.name + ": " + fmtNumber(p.calls) + " calls, " + fmtPercent(rate) + " errors"));
    edges.append(line);
  }
  root.append(edges);

  for (const d of nodes) root.append(disc(d));
  const legend = element("div", "atlas-legend");
  legend.setAttribute("aria-hidden", "true");
  for (const tone of ["ok", "warn", "crit"]) {
    const item = element("span", "");
    item.append(element("i", tone), TONE_LABEL[tone]);
    legend.append(item);
  }
  legend.append(element("span", "atlas-legend-size", "size = services"));
  const notes = [];
  if (state.errors.services && !state.data.services) notes.push("Edges unavailable: " + state.errors.services.message);
  if (state.data.services?.meta?.truncated) notes.push("Edges truncated to the busiest pairs");
  if (state.data.catalog.truncated) notes.push("Catalog truncated");
  const children = [root, legend];
  if (notes.length) children.push(element("p", "atlas-note", notes.join(" · ")));
  host.replaceChildren(...children);
}

// Mono label at 11px is about 6.6px per glyph; a disc's footprint is the wider
// of the disc and its label, plus the label line below.
const halfWidth = (d) => Math.max(d.r, d.label.length * 3.4);
const clears = (p, d, x, y, gap) => Math.abs(p.x - x) >= halfWidth(p) + halfWidth(d) + gap / 2 ||
  (y >= p.y ? y - d.r - (p.y + p.r + 20) : p.y - p.r - (y + d.r + 20)) >= gap / 2;

// layoutDiscs walks a golden-angle spiral, stretched to the canvas aspect, and
// takes the first slot that clears every placed disc and its label.
function layoutDiscs(nodes, aspect, gap) {
  const ax = Math.sqrt(Math.min(2.5, Math.max(0.6, aspect)));
  const ay = 1 / ax;
  const step = Math.max(...nodes.map((d) => d.r)) * 0.9;
  const placed = [];
  for (const d of nodes) {
    for (let k = 0; ; k++) {
      const dist = step * Math.sqrt(k);
      const x = dist * Math.cos(k * GOLDEN) * ax;
      const y = dist * Math.sin(k * GOLDEN) * ay;
      if (placed.every((p) => Math.hypot(p.x - x, p.y - y) >= p.r + d.r + gap && clears(p, d, x, y, gap))) {
        d.x = x;
        d.y = y;
        break;
      }
    }
    placed.push(d);
  }
}

function disc(d) {
  const link = svg("a", { href: "/service-map?namespace=" + encodeURIComponent(d.name), class: "atlas-node" });
  const unhealthy = d.counts.crit + d.counts.warn;
  link.setAttribute("aria-label", d.name + ": " + plural(d.total, "service") + (unhealthy ? ", " + [d.counts.crit && d.counts.crit + " critical", d.counts.warn && d.counts.warn + " degraded"].filter(Boolean).join(", ") : ", all healthy"));
  link.append(svg("title", {}, link.getAttribute("aria-label")));
  const g = svg("g", { transform: `translate(${d.x.toFixed(1)} ${d.y.toFixed(1)})` });
  if (d.counts.crit) g.append(svg("circle", { r: d.r + 6, class: "atlas-halo" }));
  g.append(svg("circle", { r: d.r + 4, class: "atlas-focus" }));
  g.append(svg("circle", { r: d.r - 2, class: "atlas-track" }));
  // Health ring: one arc per tone, clockwise from 12 o'clock.
  const circumference = 2 * Math.PI * (d.r - 2);
  const gap = d.total > 1 && unhealthy ? 1.5 : 0;
  let offset = 0;
  for (const tone of ["crit", "warn", "ok", "unk"]) {
    if (!d.counts[tone]) continue;
    const len = (d.counts[tone] / d.total) * circumference;
    g.append(svg("circle", {
      r: d.r - 2, class: "atlas-arc arc-" + tone, transform: "rotate(-90)",
      "stroke-dasharray": `${Math.max(0, len - gap).toFixed(2)} ${circumference.toFixed(2)}`,
      "stroke-dashoffset": (-offset).toFixed(2),
    }));
    offset += len;
  }
  g.append(svg("circle", { r: d.r - 6, class: "atlas-core" }));
  g.append(svg("text", { class: "atlas-count", "dominant-baseline": "central", "font-size": Math.round(Math.max(11, Math.min(16, d.r * 0.42))) }, fmtNumber(d.total)));
  g.append(svg("text", { class: "atlas-label", y: d.r + 15 }, d.label));
  link.append(g);
  return link;
}

// ---------- Services table ----------

function pips(ids, tones) {
  const wrap = element("span", "inst");
  if (!ids.length) {
    wrap.append(element("span", "tone-faint", "—"));
    return wrap;
  }
  const list = ids.map((id) => tones.get(id) || "unk").sort((a, b) => PIP_ORDER[a] - PIP_ORDER[b]);
  const counts = {};
  for (const tone of list) counts[tone] = (counts[tone] || 0) + 1;
  const row = element("span", "pips");
  row.setAttribute("role", "img");
  row.setAttribute("aria-label", plural(ids.length, "instance") + ": " + Object.entries(counts).map(([t, n]) => n + " " + TONE_LABEL[t].toLowerCase()).join(", "));
  row.title = row.getAttribute("aria-label");
  for (const tone of list.slice(0, PIP_CAP)) row.append(element("i", tone));
  wrap.append(row);
  wrap.append(element("span", "inst-count num", list.length > PIP_CAP ? "+" + (list.length - PIP_CAP) : String(list.length)));
  return wrap;
}

function renderTable(m) {
  const body = $("services-body");
  if (loading("sys")) {
    const table = element("table", "table svc-table");
    const tb = element("tbody");
    for (let i = 0; i < 8; i++) {
      const tr = element("tr");
      const td = element("td");
      td.colSpan = 6;
      td.append(element("span", "skeleton sk-line", "service"));
      tr.append(td);
      tb.append(tr);
    }
    table.append(tb);
    return body.replaceChildren(table);
  }
  if (!state.data.sys) return body.replaceChildren(errorState("sys", "services"));

  const query = $("services-filter").value.trim().toLowerCase();
  const rows = m.services.filter((s) => !query || (s.name + " " + (m.catalog.get(s.name)?.namespace || "")).toLowerCase().includes(query));
  $("services-count").textContent = query ? rows.length + " of " + m.services.length : String(m.services.length);
  if (!m.services.length) return body.replaceChildren(stateBox("info", "No services yet", "Services appear once OTLP traces arrive."));
  if (!rows.length) return body.replaceChildren(stateBox("info", "No matching services", "Nothing matches “" + query + "”."));

  const table = element("table", "table svc-table" + (state.data.catalog ? "" : " no-topology"));
  const colgroup = element("colgroup");
  const head = element("tr");
  for (const [label, cls] of [["service", "c-name"], ["domain", "c-dom"], ["instances", "c-inst"], ["err", "c-err r"], ["p99", "c-p99 r"], ["rps", "c-rps r"]]) {
    colgroup.append(element("col", cls.split(" ")[0]));
    const th = element("th", cls, label);
    th.scope = "col";
    head.append(th);
  }
  const thead = element("thead");
  thead.append(head);
  const tbody = element("tbody");
  for (const s of rows) {
    const href = "/explorer?service=" + encodeURIComponent(s.name);
    const tr = element("tr", "row-" + s.tone);
    tr.dataset.href = href;
    const nameCell = element("td", "c-name");
    const link = element("a", "cell-name row-link");
    link.href = href;
    link.title = s.name;
    link.append(toneIcon(s.tone), element("span", "", s.name));
    nameCell.append(link);
    const ns = m.catalog.get(s.name)?.namespace;
    const dom = element("td", "c-dom tone-muted", ns || "—");
    if (ns) dom.title = ns;
    const inst = element("td", "c-inst");
    inst.append(state.data.catalog ? pips(m.catalog.get(s.name)?.ids || [], m.instTone) : element("span", "tone-faint", "—"));
    const errTone = toneFromErrorRate(s.m.error_rate, s.m.request_rate_rps || s.m.span_count_1h);
    const err = element("td", "c-err r" + (errTone === "crit" || errTone === "warn" ? " tone-" + errTone : ""), fmtPercent(s.m.error_rate));
    const p99 = element("td", "c-p99 r", s.m.p99_latency_ms ? fmtMs(s.m.p99_latency_ms) : "—");
    if (s.m.latency_provenance?.p99?.status) p99.title = "p99 " + s.m.latency_provenance.p99.status;
    const rps = element("td", "c-rps r", fmtCompact(s.m.request_rate_rps));
    tr.append(nameCell, dom, inst, err, p99, rps);
    tbody.append(tr);
  }
  table.append(colgroup, thead, tbody);
  body.replaceChildren(table);

  const meta = state.data.instances?.meta;
  const note = $("services-note");
  const missing = meta && meta.available_nodes > (state.data.instances.nodes || []).length;
  const failed = state.errors.instances && !state.data.instances && !unavailable("instances") && state.data.catalog;
  note.hidden = !missing && !failed;
  note.textContent = failed ? "Instance health unavailable (" + state.errors.instances.message + "); pips show instances without health." : missing
    ? "Instance health shown for " + fmtNumber(state.data.instances.nodes.length) + " of " + fmtNumber(meta.available_nodes) + " instances; grey pips are outside that sample."
    : "";
}

// ---------- Storage & ingestion ----------

function bytes(n) {
  if (!finite(n)) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = n;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
  return (i && value < 10 ? value.toFixed(1) : Math.round(value)) + " " + units[i];
}

function stat(label, value, cls) {
  const row = element("div", "st-stat");
  row.append(element("dt", "", label), element("dd", "num" + (cls ? " " + cls : ""), value));
  return row;
}

function renderStorage() {
  const body = $("storage-body");
  const mode = $("storage-mode");
  if (loading("runtime")) {
    const sk = element("div", "st-skeleton");
    for (let i = 0; i < 6; i++) sk.append(element("span", "skeleton sk-line", "storage"));
    return body.replaceChildren(sk);
  }
  const data = state.data.runtime;
  if (!data || !data.storage || !data.exports) {
    mode.hidden = true;
    return body.replaceChildren(errorState("runtime", "runtime stats"));
  }
  const s = data.storage;
  const e = data.exports;
  mode.hidden = !data.mode;
  mode.textContent = data.mode + " mode";

  const foot = element("div", "st-foot");
  const total = element("div", "st-total");
  total.append(element("span", "st-label", "Footprint"), element("span", "num st-big", s.complete ? bytes(s.total_bytes) : "Incomplete"));
  const bar = element("div", "st-bar");
  bar.setAttribute("role", "img");
  const parts = [["main", "Main DB", s.main_bytes], ["agg", "Aggregate", s.aggregate_bytes], ["side", "WAL + SHM", s.sidecar_bytes]];
  bar.setAttribute("aria-label", "Storage split: " + parts.map(([, label, v]) => label + " " + bytes(v)).join(", "));
  if (s.complete && s.total_bytes > 0) {
    for (const [cls, , size] of parts) {
      const part = element("i", cls);
      part.style.flexGrow = String(Math.max(0, size / s.total_bytes));
      bar.append(part);
    }
  }
  const legend = element("dl", "st-legend");
  for (const [cls, label, size] of parts) {
    const item = element("div", "");
    const dt = element("dt", "");
    dt.append(element("i", cls), label);
    item.append(dt, element("dd", "num", bytes(size)));
    legend.append(item);
  }
  foot.append(total, bar, legend);
  foot.append(element("p", "st-free", finite(s.free_bytes) ? bytes(s.free_bytes) + " free on the data volume" : "Free space unavailable"));

  const stats = element("dl", "st-stats");
  stats.append(
    stat("Trace exports", fmtNumber(e.trace_exports)),
    stat("Metric exports", fmtNumber(e.metric_exports)),
    stat("Log exports", fmtNumber(e.log_exports)),
    stat("Failed exports", fmtNumber(e.rejected_exports), e.rejected_exports ? "tone-crit" : ""),
    stat("Server ACK p99", e.latency_samples ? fmtMs(e.ack_p99_ms) : "—"),
    stat("Queue usage", finite(data.queue_ratio) ? fmtPercent(data.queue_ratio) : "—", data.queue_ratio >= 0.9 ? "tone-warn" : ""),
    stat("App heap", bytes(data.heap_bytes)),
  );
  const notes = element("div", "st-notes");
  notes.append(element("p", "st-retention", data.retention_days ? "Retention " + data.retention_days + " days" : "Retention unavailable"));
  for (const text of [s.note, e.latency_note && e.latency_note + " Samples: " + fmtNumber(e.latency_samples) + "."]) {
    if (text) notes.append(element("p", "", text));
  }
  body.replaceChildren(foot, stats, notes);
}

// ---------- Live indicator ----------

function renderLive() {
  const live = $("live");
  const failed = Object.values(state.errors).some((e) => e && e.status !== 503);
  if (!state.updatedAt) {
    live.className = "live " + (failed ? "is-error" : "is-off");
    live.textContent = failed ? "Update failed" : "Connecting";
    return;
  }
  const seconds = Math.max(0, Math.round((Date.now() - state.updatedAt) / 1000));
  live.className = "live" + (failed ? " is-error" : document.hidden ? " is-off" : "");
  live.textContent = (failed ? "Partial · " : "") + "Updated " + (seconds < 60 ? seconds + "s ago" : fmtAgo(state.updatedAt));
  live.title = failed ? "Some panels failed to refresh; they keep their last data or show an error." : "Refreshes every 30 seconds while this tab is visible.";
}

// ---------- Wiring ----------

initShell();
$("range-seg").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-range]");
  if (button) setRange(button.dataset.range);
});
$("services-filter").addEventListener("input", () => renderTable(model()));
$("services-body").addEventListener("click", (event) => {
  const row = event.target.closest("tr[data-href]");
  if (row && !event.target.closest("a")) location.href = row.dataset.href;
});
let atlasFrame = 0;
new ResizeObserver(() => {
  cancelAnimationFrame(atlasFrame);
  atlasFrame = requestAnimationFrame(() => renderAtlas(model()));
}).observe($("atlas"));
setInterval(renderLive, 1000);
setInterval(() => { if (!document.hidden) refresh(); }, POLL_MS);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && Date.now() - state.updatedAt > POLL_MS) refresh();
  renderLive();
});
render();
refresh();

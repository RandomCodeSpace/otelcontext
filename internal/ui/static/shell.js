// Shared browser helpers for the Overview and Service map pages: theme,
// icons, formatting, health tone and JSON reads. No framework, no build.

const SVG_NS = "http://www.w3.org/2000/svg";
const THEME_KEY = "oc-theme";

export const $ = (id) => document.getElementById(id);

export function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

export function svg(tag, attrs = {}, text) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value !== undefined && value !== null) node.setAttribute(key, String(value));
  }
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

// icon returns an inline <svg><use> for a Lucide sprite symbol.
export function icon(name, className = "ic") {
  const node = svg("svg", { class: className, "aria-hidden": "true", focusable: "false" });
  node.append(svg("use", { href: "/static/icons.svg#i-" + name }));
  return node;
}

// Health tones. Shape always carries the state as well as colour.
export const TONE_ICON = { ok: "circle-check", warn: "triangle-alert", crit: "octagon-x", unk: "circle-dashed" };
export const TONE_LABEL = { ok: "Healthy", warn: "Degraded", crit: "Critical", unk: "No traffic" };
const TONE_RANK = { crit: 0, warn: 1, unk: 2, ok: 3 };

export function toneFromStatus(status) {
  const value = String(status || "").toLowerCase();
  if (value === "critical" || value === "down" || value === "unhealthy") return "crit";
  if (value === "degraded" || value === "warning") return "warn";
  if (value === "healthy" || value === "ok") return "ok";
  return "unk";
}

// toneFromErrorRate classifies topology summaries that carry no status.
export function toneFromErrorRate(errorRate, requests) {
  if (!requests) return "unk";
  if (errorRate >= 0.05) return "crit";
  if (errorRate >= 0.01) return "warn";
  return "ok";
}

export const toneRank = (tone) => TONE_RANK[tone] ?? 2;

export function toneIcon(tone, className = "ic ic-sm") {
  const node = icon(TONE_ICON[tone] || TONE_ICON.unk, className + " tone-" + tone);
  node.setAttribute("role", "img");
  node.setAttribute("aria-label", TONE_LABEL[tone] || TONE_LABEL.unk);
  node.removeAttribute("aria-hidden");
  return node;
}

export function badge(tone, text) {
  return element("span", "badge " + tone, text || TONE_LABEL[tone]);
}

// Formatting.
const finite = (n) => typeof n === "number" && Number.isFinite(n);

export function fmtNumber(n, digits = 0) {
  return finite(n) ? n.toLocaleString(undefined, { maximumFractionDigits: digits }) : "—";
}

export function fmtCompact(n) {
  if (!finite(n)) return "—";
  const abs = Math.abs(n);
  if (abs >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
  if (abs >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
  if (abs >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, "") + "k";
  return abs >= 10 ? String(Math.round(n)) : String(Math.round(n * 10) / 10);
}

export function fmtRate(rps) {
  return finite(rps) ? fmtCompact(rps) + "/s" : "—";
}

export function fmtPercent(ratio, digits = 1) {
  if (!finite(ratio)) return "—";
  const value = ratio * 100;
  if (value > 0 && value < 0.1) return "<0.1%";
  return value.toFixed(digits) + "%";
}

export function fmtMs(ms) {
  if (!finite(ms)) return "—";
  if (ms >= 1000) return (ms / 1000).toFixed(ms >= 10000 ? 0 : 2) + " s";
  if (ms >= 100) return Math.round(ms) + " ms";
  return (Math.round(ms * 10) / 10) + " ms";
}

export function fmtAgo(date) {
  const value = date instanceof Date ? date : new Date(date);
  const seconds = Math.round((Date.now() - value.getTime()) / 1000);
  if (!Number.isFinite(seconds)) return "—";
  if (seconds < 60) return "just now";
  if (seconds < 3600) return Math.floor(seconds / 60) + "m ago";
  if (seconds < 86400) return Math.floor(seconds / 3600) + "h ago";
  return Math.floor(seconds / 86400) + "d ago";
}

// sparkline renders a fixed-size SVG polyline; empty input yields a flat rule.
export function sparkline(values, { width = 72, height = 24, tone = "ok", label = "" } = {}) {
  const node = svg("svg", { width, height, viewBox: `0 0 ${width} ${height}`, class: "spark", role: label ? "img" : null, "aria-label": label || null, "aria-hidden": label ? null : "true" });
  const points = (values || []).filter(finite);
  const stroke = tone === "crit" ? "var(--crit)" : tone === "warn" ? "var(--warn)" : "var(--tx-3)";
  if (points.length < 2) {
    node.append(svg("line", { x1: 0, x2: width, y1: height - 2, y2: height - 2, stroke: "var(--bd-2)", "stroke-width": 1 }));
    return node;
  }
  const min = Math.min(...points);
  const max = Math.max(...points);
  const span = max - min || 1;
  const d = points.map((v, i) => {
    const x = (i / (points.length - 1)) * width;
    const y = height - 2 - ((v - min) / span) * (height - 4);
    return (i ? "L" : "M") + x.toFixed(1) + " " + y.toFixed(1);
  }).join(" ");
  node.append(svg("path", { d, fill: "none", stroke, "stroke-width": 1.5, "stroke-linejoin": "round", "stroke-linecap": "round" }));
  return node;
}

// fetchJSON reads a same-origin JSON endpoint and surfaces the server's text on failure.
export async function fetchJSON(path, { signal, timeoutMs = 10000 } = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  const abort = () => controller.abort();
  signal?.addEventListener("abort", abort, { once: true });
  try {
    const response = await fetch(path, {
      signal: controller.signal, cache: "no-store", credentials: "same-origin",
      headers: { Accept: "application/json" },
    });
    if (!response.ok) {
      const message = (await response.text()).slice(0, 300).trim();
      const error = new Error(message || "HTTP " + response.status);
      error.status = response.status;
      throw error;
    }
    const data = await response.json();
    return { data, headers: response.headers };
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener("abort", abort);
  }
}

// Theme: shared with the Explorer through the same storage key.
export function applyStoredTheme() {
  let stored = null;
  try { stored = localStorage.getItem(THEME_KEY); } catch { stored = null; }
  const system = matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  setTheme(stored === "light" || stored === "dark" ? stored : system);
}

function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  const meta = document.querySelector("meta[name=theme-color]");
  if (meta) meta.setAttribute("content", theme === "light" ? "#fbfaf8" : "#161615");
  const button = $("theme-button");
  if (button) {
    button.setAttribute("aria-label", theme === "light" ? "Switch to dark theme" : "Switch to light theme");
    button.querySelector("use")?.setAttribute("href", "/static/icons.svg#i-" + (theme === "light" ? "moon" : "sun"));
  }
}

export function initShell() {
  applyStoredTheme();
  $("theme-button")?.addEventListener("click", () => {
    const next = document.documentElement.dataset.theme === "light" ? "dark" : "light";
    try { localStorage.setItem(THEME_KEY, next); } catch { /* theme still applies */ }
    setTheme(next);
  });
}

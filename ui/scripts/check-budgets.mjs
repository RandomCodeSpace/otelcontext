#!/usr/bin/env node
// Zero-dependency bundle budget gate. Gzips the built dist output and fails
// (exit 1) when any budget in ui/budgets.json is exceeded. Run manually or
// in CI via `npm run check-budgets` — deliberately NOT wired into `build`
// yet (phase C1/C2 reports numbers; later phases flip it to a hard gate).
import { gzipSync } from 'node:zlib';
import { readFileSync, readdirSync, existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const uiDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const distDir = resolve(uiDir, '../internal/ui/dist');
const budgets = JSON.parse(readFileSync(join(uiDir, 'budgets.json'), 'utf8')).gzipKb;

if (!existsSync(join(distDir, 'index.html'))) {
  console.error(`check-budgets: ${distDir}/index.html not found — run \`npm run build\` first`);
  process.exit(1);
}

const gzipKb = (path) => gzipSync(readFileSync(path)).length / 1024;
const fmt = (kb) => `${kb.toFixed(2)} KB`;

// "Initial" = everything index.html references: the entry <script>, every
// modulepreload hint, and every stylesheet.
const html = readFileSync(join(distDir, 'index.html'), 'utf8');
const initialJs = [
  ...html.matchAll(/<script[^>]+src="([^"]+\.js)"/g),
  ...html.matchAll(/<link[^>]+rel="modulepreload"[^>]+href="([^"]+\.js)"/g),
].map((m) => m[1]);
const initialCss = [...html.matchAll(/<link[^>]+rel="stylesheet"[^>]+href="([^"]+\.css)"/g)].map(
  (m) => m[1],
);

const failures = [];
const check = (label, actualKb, budgetKb) => {
  const ok = actualKb <= budgetKb;
  console.log(`${ok ? '  ok  ' : ' FAIL '} ${label.padEnd(34)} ${fmt(actualKb).padStart(11)} / ${budgetKb} KB`);
  if (!ok) failures.push(label);
};

const sumGzip = (paths) => paths.reduce((acc, p) => acc + gzipKb(join(distDir, p.replace(/^\//, ''))), 0);

console.log(`bundle budgets (gzip) — dist: ${distDir}`);
check('initial JS (entry + modulepreload)', sumGzip(initialJs), budgets.initialJs);
check('initial CSS', sumGzip(initialCss), budgets.initialCss);

// Fonts: raw bytes — woff2 is already compressed, gzip would lie slightly.
const fontsDir = join(distDir, 'fonts');
const fontKb = existsSync(fontsDir)
  ? readdirSync(fontsDir)
      .filter((f) => f.endsWith('.woff2'))
      .reduce((acc, f) => acc + readFileSync(join(fontsDir, f)).length / 1024, 0)
  : 0;
check('vendored fonts (raw woff2)', fontKb, budgets.fonts);

// Lazy chunks: every JS asset not referenced from index.html.
const initialSet = new Set(initialJs.map((p) => p.replace(/^\/assets\//, '')));
const lazyChunks = readdirSync(join(distDir, 'assets')).filter(
  (f) => f.endsWith('.js') && !initialSet.has(f),
);
for (const chunk of lazyChunks) {
  const name = chunk.replace(/-[^-]+\.js$/, ''); // strip content hash
  const cap = budgets.lazyChunkExceptions[name] ?? budgets.lazyChunkDefault;
  check(`lazy ${chunk}`, gzipKb(join(distDir, 'assets', chunk)), cap);
}

if (failures.length > 0) {
  console.error(`\ncheck-budgets: ${failures.length} budget(s) exceeded`);
  process.exit(1);
}
console.log('\ncheck-budgets: all budgets met');

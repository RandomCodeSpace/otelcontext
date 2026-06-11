#!/usr/bin/env node
// Build-time precompression for the embedded UI bundle.
//
// Emits .br (brotli, quality 11) and .gz (gzip, level 9) siblings next to
// every compressible dist asset so the Go binary can serve precompressed
// bytes with zero runtime CPU — internal/ui/ui.go negotiates Accept-Encoding
// against the siblings picked up by its `all:dist` embed directive.
//
// Zero dependencies: node:zlib + node:fs only (air-gapped build friendly).
// Files under 1KB are skipped (header overhead beats the savings) and the
// emitted .br/.gz siblings are themselves ignored, so reruns are idempotent.
//
// Usage: node scripts/precompress.mjs [distDir]
//   distDir defaults to ../../internal/ui/dist (the Vite outDir).

import { brotliCompressSync, gzipSync, constants } from 'node:zlib';
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, extname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const COMPRESSIBLE = new Set(['.js', '.css', '.html', '.svg', '.json']);
const MIN_BYTES = 1024;

const scriptDir = dirname(fileURLToPath(import.meta.url));
const distDir = resolve(
  process.argv[2] ?? join(scriptDir, '..', '..', 'internal', 'ui', 'dist'),
);

function* walk(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) yield* walk(full);
    else if (entry.isFile()) yield full;
  }
}

const kb = (n) => `${(n / 1024).toFixed(1)} KB`;
const rows = [];

for (const file of walk(distDir)) {
  if (!COMPRESSIBLE.has(extname(file))) continue;
  const buf = readFileSync(file);
  if (buf.length < MIN_BYTES) continue;
  const br = brotliCompressSync(buf, {
    params: {
      [constants.BROTLI_PARAM_QUALITY]: 11,
      [constants.BROTLI_PARAM_SIZE_HINT]: buf.length,
    },
  });
  const gz = gzipSync(buf, { level: 9 });
  writeFileSync(`${file}.br`, br);
  writeFileSync(`${file}.gz`, gz);
  rows.push({ file: relative(distDir, file), orig: buf.length, gz: gz.length, br: br.length });
}

if (rows.length === 0) {
  console.log(`precompress: nothing to do in ${distDir} (no compressible files >= 1KB)`);
} else {
  rows.sort((a, b) => b.orig - a.orig);
  const width = Math.max(...rows.map((r) => r.file.length), 'total'.length);
  console.log(`precompress: ${distDir}`);
  console.log(
    `${'file'.padEnd(width)}  ${'orig'.padStart(10)}  ${'gzip'.padStart(10)}  ${'brotli'.padStart(10)}`,
  );
  let torig = 0;
  let tgz = 0;
  let tbr = 0;
  for (const r of rows) {
    torig += r.orig;
    tgz += r.gz;
    tbr += r.br;
    console.log(
      `${r.file.padEnd(width)}  ${kb(r.orig).padStart(10)}  ${kb(r.gz).padStart(10)}  ${kb(r.br).padStart(10)}`,
    );
  }
  console.log(
    `${'total'.padEnd(width)}  ${kb(torig).padStart(10)}  ${kb(tgz).padStart(10)}  ${kb(tbr).padStart(10)}  (brotli saves ${(100 * (1 - tbr / torig)).toFixed(0)}%)`,
  );
}

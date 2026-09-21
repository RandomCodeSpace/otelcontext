# Dagre layout dependency

The embedded application map uses `@dagrejs/dagre` 3.1.1 and its bundled
`@dagrejs/graphlib` 4.0.5. Both use the MIT license. The full notice for both
libraries is in `internal/ui/static/dagre.min.js.LEGAL.txt`.

The browser distribution exposes `globalThis.dagre`, including
`dagre.graphlib.Graph` and `dagre.layout`. It computes directed service-card
positions and routed connections. Our `map-layout.js` handles graph partitioning
and stable input ordering. The library is embedded in the Go binary; the UI needs
no Node installation, frontend build, package manager, or runtime CDN.

## Source and checksums

Retrieved on 2026-09-19 from the
[versioned npm archive](https://registry.npmjs.org/@dagrejs/dagre/-/dagre-3.1.1.tgz).
[Registry metadata](https://registry.npmjs.org/@dagrejs/dagre/3.1.1) identifies
3.1.1 as the stable release published on 2026-08-08 and records the bundled
dependency version. The [upstream project](https://github.com/dagrejs/dagre)
publishes the code and browser API documentation.

Registry archive integrity:

```text
sha512-zroZB1dFOFiGgv4Xcrn1DckB1o4aOikPqD2NDQPV0WM//CXGcS6xiD0rNkqHmw6FEg4tabt4nxPLwgCWT+Vb2A==
```

SHA-256 checksums:

| File | Bytes | SHA-256 |
| --- | ---: | --- |
| npm archive | 359254 | `6db2c35cf4c52cd1cd3a87c3a97c7c5fa559deaf4e452f67aab4008e00ca28c4` |
| Original `package/dist/dagre.min.js` | 48956 | `3152d214941a5df3a3d4c079dfa338c3cd7a6c0d4c1b4c3a2fdb6bba6f6facf9` |
| Embedded `internal/ui/static/dagre.min.js` | 48918 | `31920d90797df830857836c8a595fa380e88c540dfc0c3af8e5b367b03c6ba54` |
| Original and embedded `dagre.min.js.LEGAL.txt` | 1078 | `9148bffb1e84382a8b6668eeb2b53c6a554341d714fba129856ea5eb350d35f3` |

The only script modification removes its exact trailing line:

```text
//# sourceMappingURL=dagre.min.js.map
```

The unused source map is not shipped. No library code or legal notice changed.

To reproduce, download the pinned archive, verify its SHA-512 integrity, and
extract only `package/dist/dagre.min.js` and
`package/dist/dagre.min.js.LEGAL.txt`. Remove the script's trailing source-map
line and verify the embedded-file hashes above before replacing either asset.

## Dependency review

Context7 and the upstream documentation confirmed the browser-global API before
selection. The registry lists three maintainers; upstream had recent commits
for the August release. Graphlib has no runtime dependencies. Exact-version
queries to the [OSV API](https://google.github.io/osv.dev/post-v1-query/) returned
no advisory records for either package on 2026-09-19. The upstream
[security overview](https://github.com/dagrejs/dagre/security) lists no published
advisories and no security policy. This check does not guarantee the absence of
vulnerabilities.

The fixed asset allowlist permits this pinned library and its notice while
preserving the repository's no-frontend-build contract. Recheck versions,
licenses, advisories, and the focused map scenarios before updating it.

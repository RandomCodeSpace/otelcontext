# Application map implementation plan

Status: superseded by the user's B flow workspace + A overview selection on 2026-09-21. The earlier verification did not establish readable full-system coverage; its results below are historical. Current implementation and evidence: [B flow workspace and A overview](2026-09-21-flow-workspace.md).
Baseline: `436f8f134f7c30cd87302c18d5ef6d484ff97c40`.

## Scope receipt

- Goal: replace the sunflower constellation with an operational application map inspired by AppDynamics flow maps and Azure Application Map.
- Acceptance criteria: directed, routed connections; readable service names; truthful connection evidence; usable connected, mixed, and disconnected 150-service datasets; stable layout during metric refresh; working pointer, keyboard, mobile, and both themes.
- Non-goals: backend or storage redesign, a React migration, a frontend build pipeline, new application/namespace metadata APIs, invented topology or rates, unrelated defect repair, committing, pushing, or deployment.
- Owner: primary agent owns integration, `app.js`, and final acceptance. Independent implementation work uses isolated worktrees with explicit file ownership.
- Smallest implementation: retain the embedded browser client and existing APIs; vendor one pinned layout engine; replace map composition and viewport logic; preserve existing investigation features; add focused browser regressions and a reusable real-OTLP fixture.
- Checks: scoped UI Go tests, relevant protected browser workflow, targeted application-map scenarios, and actual screenshots from a freshly built binary.
- Stop condition: all acceptance scenarios below pass, task diff is reviewed, and a running preview plus screenshots is available. No additional refactors or broad suites.

## Design receipt

Mode: design followed by implementation.
Primary archetype: operational dependency map with scoped investigation.
Routes: decision-system.md, data-visualization.md, responsive-layout.md, performance-rendering.md, application-patterns.md, visual-direction.md, design-systems.md, quality-gates.md.
Material risks: unreadable 150-node graphs, disconnected-node layout failure, misleading metrics, hidden labels, layout jumps, loss of host and keyboard workflows.
Decision: preserve application call flow as the map's organizing structure.
Evidence: current screenshot has 150 dots and no cross-service links; PR #113 introduced Dagre and #117 replaced it after disconnected nodes produced an extremely tall layout; the current graph API exposes service IDs, host membership, and call-edge evidence.
Default: reuse the embedded SVG/browser architecture and existing service/host inspectors.
Choice: a locally embedded Dagre layout, visible rectangular service cards, explicit graph scope, and a separate disconnected inventory.
Override: remove radial placement and label suppression because they violate the user's requested map and readable service identity.
System effects: map HTML/CSS/JS, narrow static-asset contract additions, responsive inspector docking, semantic labels, grouping/focus state, viewport conversion, fixture and browser coverage.
Verification: connected and disconnected 150-service browser scenarios, protected workflows, both themes and desktop/mobile, layout timing and stability measurements. Results will be recorded after implementation.

## Data and dependency decisions

1. Keep `/api/system/graph`, `/api/hosts`, dashboard, WebSocket, and MCP contracts unchanged.
2. Service names and host membership are authoritative. Application and namespace grouping are unavailable in this response. Do not infer them from naming conventions.
3. Derive connected groups from actual call edges. Expose each group's service count and allow opening one group. Preserve the existing host grouping and host metric panel; host placement edges must never become call dependencies.
4. Edge evidence is `call_count`, `avg_latency_ms`, and `error_rate`. Show **calls**, average latency, and error percentage. Never label call counts as requests per second. Unknown evidence stays unavailable.
5. Use `@dagrejs/dagre` **3.1.1**, with its bundled `@dagrejs/graphlib` **4.0.5**. Both are MIT. Registry, Context7, upstream history, browser-global API, and exact-version OSV lookups were checked; no OSV records were returned. Upstream lacks a SECURITY.md, which remains a documented maintenance limitation.
6. Vendor the 48,956-byte browser distribution as a flat embedded asset, retain both licenses and provenance, and remove only its source-map URL comment if necessary. Original bundle SHA-256: `3152d214941a5df3a3d4c079dfa338c3cd7a6c0d4c1b4c3a2fdb6bba6f6facf9`. No runtime CDN or package installation/build step.
7. Narrowly extend the existing static-asset allowlist for the library, its notice, and a small first-party map-layout module. Keep the no-Node, no-remote-assets application contract. This is a deliberate layout dependency, not a general tooling migration.

## Intended experience

### Overview and scope

- Rename the screen to **Application map**. Remove constellation language, decorative rings, dot-cloud fallback, and gradient-heavy map treatment.
- Display connected service and observed dependency counts, plus the number of services with no observed dependencies.
- Provide a connected-group selector when more than one independent call graph exists. Group labels identify a member/entry service and count, not a fabricated application.
- For a connected group above ten services, start in an explicitly labelled, bounded neighborhood of a deterministic service (at most 30 nodes initially), retaining a visible total and a clear full-group action. Do not silently omit services.
- Keep a readable zoom baseline and real panning/minimap navigation. An explicit overview can reduce scale, but never replace service cards with anonymous dots or delete their names.
- Selecting a service opens its inspector. Visible scope controls expose its direct callers, direct dependencies, both neighbors, and return to the full group.
- Search can locate every service, including one outside the current group, and selection opens its owning group or disconnected inventory.

### Nodes and connections

- Service cards show name, explicit status, and concise latency/error evidence. Component types are used only when provided by telemetry.
- Connections use routed paths, clear arrowheads, and compact call/latency labels where they fit. No decorative edge animation.
- A connection is selectable with pointer or keyboard and has an accessible label containing caller, callee, and measured evidence. Selection opens connection details with calls, average latency, errors, and links to both service inspectors.
- Metric updates change text/status without recalculating layout. Cache by sorted node/edge identity and grouping/scope, excluding metrics and theme.
- Scope changes get an intentional fit. Selection, metric refresh, and theme changes preserve the current viewport.
- System health copy must distinguish healthy service counts from request success; avoid the current unexplained 95% health alongside 150 critical services.

### Disconnected services

- Services without observed call edges appear in a searchable, responsive inventory labelled **No observed dependencies**.
- All 150 disconnected services remain counted and inspectable. Do not invent links, force them into a Dagre rank, or switch layout algorithms.
- Mixed datasets account for every service across connected graphs and the disconnected inventory.
- Absence of observed edges describes coverage, not proof that the service has no dependencies.

### Preserved workflows and responsive behavior

| Region | Desktop | Narrow screen | Preserved contract |
| --- | --- | --- | --- |
| Map and scope | Directed SVG with group/focus controls | Focused map or existing service-list switch | Search, selected service, group, dependency direction |
| Inspector | Docked alongside the map, replacing the service rail while selected | Existing dismissible sheet with reachable close/tabs | Overview, Why, Impact, Dependencies, host metrics, connection evidence |
| Service inventory | Compact searchable rows/cards | Single-column or suitable wrapping rows | Names, status, count, inspector access |
| Header | Compact live status and relevant summary | Wrap or defer secondary metrics | Theme, refresh, connection status, command menu |

Preserve URL selection, keyboard shortcuts, command menu, theme persistence, reconnect behavior, loading/empty/error/retry states, latency provenance, host placement distinction, and MCP actions. No controls without working behavior.

## Implementation sequence and file ownership

1. **Plan checkpoint, primary agent:** finish this document and present its decisions before editing production files.
2. **Layout dependency and module, isolated worker:** add flat vendored Dagre assets and license/provenance; add a small first-party layout module for sorted directed layout, component/disconnected partitioning, bounds, and topology identity. Use existing helpers where suitable.
3. **Markup and styling, isolated worker:** update `internal/ui/static/index.html` and `app.css` for the application map, scope controls, disconnected inventory, visible service cards, edge selection, docked inspector, and responsive/theme styles. Coordinate exact selectors with the primary agent before edits.
4. **Application behavior, primary agent:** integrate the module in `app.js`; replace sunflower rendering and fixed 1000-by-1000 assumptions; implement group/focus/inventory/edge selection; preserve existing workflows and accurate metrics.
5. **Verification fixtures, isolated worker:** extend existing `test/browser` infrastructure with real cross-service OTLP fixtures and focused map scenarios. Add a reusable connected/disconnected 150-service preview emitter without changing the existing load simulator engine.
6. **Integration, primary agent:** apply only each worker's owned-file diff, update narrow UI asset/heading contracts and any protected contract wording changed intentionally, inspect the full diff, and run checks serially against the integrated tree.
7. **Preview, primary agent:** build the exact modified server, run a disposable 150-service connected scenario, capture overview/focus/connection/disconnected screenshots, and leave the reviewable preview running.

## Acceptance and focused checks

1. `go test ./internal/ui` passes with explicit checks that Dagre and first-party assets are embedded and application assets make no external requests.
2. Run the existing protected browser workflow against the exact built binary. This targeted browser suite is necessary because changes affect shared map selection, viewport, host mode, inspector, and mobile contracts; isolated layout tests cannot verify them.
3. Run focused application-map browser scenarios:
   - 150 services in several connected groups, with fan-out, a return/cycle edge, and mixed health; all services accounted for, routed arrows rendered, names readable in the initial useful view, group/focus controls functional.
   - One large connected graph; explicitly bounded initial scope, full group reachable, callers/dependencies direction correct, no anonymous-dot fallback.
   - 150 services with zero call edges; complete searchable disconnected inventory and working service selection, no tall single-column graph.
   - A mixed graph; counts reconcile and search crosses scope boundaries correctly.
   - A metric-only refresh; node transforms, selection, focus scope, and viewBox unchanged while metrics update.
   - Connection details match fixture/API values; host placement edges are excluded from calls and impact traversal.
   - Mouse/keyboard selection, pan, zoom, Fit, minimap, inspector close/tab paths, error recovery, and reconnect remain usable.
4. Inspect 1600x1000 desktop, intermediate width, and 390x844 mobile in both themes. Check visible names, control overlap, page overflow, focus, and reduced-motion behavior.
5. Measure directed-layout time for representative 150-service inputs and observed selection latency. Target layout under 500 ms and selection under 200 ms on this host, label these as host-specific checks rather than capacity certification.
6. Report actual results and any unavailable checks. Preserve all three user-owned local review documents. Do not commit or push.

## Decision sources

- Repository PRs #113, #117, and #255 establish the previous layered-map implementation and its later replacement.
- AppDynamics flow maps: https://docs.appdynamics.com/appd/23.x/23.11/en/application-monitoring/business-applications/flow-maps
- Azure Application Map: https://learn.microsoft.com/en-us/azure/azure-monitor/app/app-map
- Dagre source and distribution: https://github.com/dagrejs/dagre and https://www.npmjs.com/package/@dagrejs/dagre

## Implementation observations

- The first rendered 15-service group exceeded the available height at readable label size. The initial neighborhood threshold is therefore ten services; the full group remains an explicit action. This addresses measured viewport fit without changing the directed layout or hiding names.
- Geometry checks found that Dagre's default ranking could route a long dependency across another service card. Longest-path ranking, 176-unit rank spacing, and reserved 100-by-24 edge-label space eliminated card-interior crossings in the connected and large acceptance fixtures. This is library configuration, with an exact segment/rectangle regression check.
- Browser viewport changes produced two successive canvas sizes as media queries settled. Resize fitting now waits for those canvas notifications to settle; selection and metric updates do not trigger a fit.
- The OTLP fixture sends parent spans before child spans in separate acknowledged exports. Use `INGEST_ASYNC_ENABLED=false` for deterministic immediate topology, as the browser harness does. No ingestion implementation was changed.

## Verification results

Implementation branch: `feat/application-flow-map`, with uncommitted changes above the baseline recorded at the top of this plan. The three user-owned local review documents were preserved.

| Check | Result |
| --- | --- |
| `go test ./internal/ui` | All 9 tests passed |
| `node --check internal/ui/static/app.js` | Passed |
| `CGO_ENABLED=0 go build -o /tmp/otelcontext-application-map .` | Passed |
| `TestApplicationMap150`, browser build tag | All four real-OTLP scenarios passed in 17.300 seconds |
| `TestProtectedBrowserWorkflow`, browser build tag | Passed in 43.127 seconds against the final binary |
| `git diff --check` | Passed |
| Served HTML, CSS, application module, layout module, and Dagre asset | Byte-for-byte match with the working tree |

The application-map scenarios verified 150 services with 170 dependencies across ten components; 150 disconnected services; 120 connected plus 30 disconnected services with 136 dependencies; and one 150-service component with 179 dependencies. All exported spans were acknowledged, and graph membership and directed edges were checked against the fixture.

Browser checks cover full-group expansion and reload, disconnected-service deep links, caller/dependency scope, search across coverage states, pointer and keyboard selection, stable metric refresh, both themes, desktop/intermediate/mobile layouts, and the preserved host, MCP, reconnect, navigation, and accessibility workflows. Intermediate-width resize checks require service-name text of at least 12 screen pixels. Reduced-motion behavior was inspected manually. The focused layout checks found no dependency segments passing through another card's interior.

On this host, layout measured 45.4 ms for the ten-component graph and 33.8 ms for the single large graph. Reordered inputs and changed metrics retained identical positions. A service-selection check against the final running preview measured 27.9 ms. These are local measurements, not capacity guarantees.

The running preview is at `http://127.0.0.1:18080/`, replacing the previous preview at that address. It contains 150 simulated services and 170 observed dependencies. Binary SHA-256: `377ec1f596e4da735074f39ffb5dfefd28f5737163c870cd37a535faed48735f`.

Local evidence is under `/tmp/otelcontext-map-preview/`: `verification.json`, `simulation-final.json`, `application-map-accepted/`, `protected-accepted/`, and desktop screenshots `application-map-light.png` and `application-map-dark.png`. The in-chat browser client could not connect; local Chrome browser checks and screenshots succeeded.

The initial view deliberately scopes larger groups to a readable neighborhood. Full graphs remain available through group controls, pan, zoom, Fit, and the minimap; the full 150-service overview is not intended to make every label readable simultaneously. Application/namespace grouping remains unavailable because the graph API does not expose that metadata.

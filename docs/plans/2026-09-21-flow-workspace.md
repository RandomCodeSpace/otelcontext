# B flow workspace and A overview

The user selected this direction on 2026-09-21 after comparing three running prototypes. The concrete design and implementation sequence preceded product edits in the local planning workspace. This receipt records the resulting implementation.

## Scope

- Goal: make a 150-service system navigable through a directed flow workspace and complete system overview.
- Criteria: every service accounted for; readable service cards; exact dependency evidence across neighborhood boundaries; Back restores scope and viewport; existing investigation, host, keyboard, and mobile paths remain usable.
- Non-goals: telemetry/backend changes, ownership inference, framework or layout-library replacement, commit, push, deployment.
- Owner: primary agent integrates and verifies; independent layout, shell, and browser-check work used isolated worktrees.
- Smallest change: reuse embedded HTML/CSS/JavaScript, Dagre, and existing API/inspector contracts.
- Checks: focused UI tests, four real-OTLP 150-service browser scenarios, protected browser workflow, screenshots and source-to-served-asset comparison.
- Stop: focused checks pass and the running preview and evidence are available for review.

## Implemented behavior

B is the default workspace: a left neighborhood index, directed service cards, scope controls, and existing inspectors. A is available through **System overview**. Its cards show each service once as an accessible health mark, with exact service and boundary counts. Marks open service inspectors; **Open map** enters the neighborhood.

Deterministic breadth-first traversal partitions each connected component into navigation groups of at most eight members. These are labeled Neighborhood 01, etc.; they do not claim application or team ownership. The initial flow focuses a critical service where present. Full-group scope and direct callers/dependencies remain available, with a maximum of eight cards per flow. Larger flows require panning; Fit preserves readable text instead of reducing an entire system to tiny labels.

The expandable evidence list contains the current group's full service names and every internal or crossing dependency, using exact source/target IDs. Selecting a dependency opens its two endpoints and connection inspector. Back restores the previous scope, selection, and viewport. Keyboard focus on dynamic navigation rows survives refresh.

## Evidence, 2026-09-21

- `go test ./internal/ui`: passed.
- JavaScript syntax and `git diff --check`: passed.
- `TestApplicationMap150`: all four cases passed against `/tmp/otelcontext-map-final-20260921` in 18.035 seconds: 150 connected services/170 edges, 150 services/zero edges, mixed 150 services/136 edges, and one connected component of 150 services/179 edges. Fixtures enter through real OTLP HTTP exports, not intercepted graph responses.
- Browser coverage includes exact service/edge identity, bounded membership, full-system overview geometry at 1440×900, crossing-edge inspection and Back, readable names after Fit/resize, metric-refresh stability, themes, mobile, search, and keyboard interaction.
- `TestProtectedBrowserWorkflow`: passed in 43.514 seconds against the same binary, including Overview/Dependencies, Why/Impact MCP calls, commands and keyboard controls, reconnect, themes, mobile, and host grouping/metrics. The test now selects the visible **Services** index before choosing a service after clearing search. Chromium emitted unsupported DevTools event notices; the application event assertions passed.
- Grouping module checks covered a chain, cycle, and 149-neighbor hub; these additional shapes were module checks, not real-OTLP browser runs.
- Live preview: `http://127.0.0.1:18082/`, 150 services and 170 dependencies, all 340 exported spans acknowledged. The default three-card flow is entirely inside its 1440×900 canvas with 14px service names. The overview has 20 visible cards and exactly 150 distinct service marks.
- Screenshots: `/tmp/otelcontext-flow-implementation/B-flow-1440.png` and `A-overview-1440.png`. Browser artifacts: `/tmp/otelcontext-map-20260921-final/`.
- Preview assets match working-tree `app.js`, `app.css`, and `map-layout.js` byte for byte. Binary SHA-256: `e4b3503eb44b01557f076ecd0fdac822ccdb8cf9ce90836fe66645d8b236682a`.

The atlas provides full-system coverage; it does not display 150 readable service names simultaneously. Full names remain available through search, the service index, group evidence, and inspectors. On smaller screens the overview scrolls. Hosted CI, production deployment, and the user's acceptance of the implemented boundary workflow are not established by these local checks.

## Release preparation

The user authorized push and release on 2026-09-21. The candidate version is
`v0.6.0`. Soft edges now follow the same Dagre routes with rounded quadratic
bends; pointer hit paths use identical geometry. A release review caught canvas
resizing through inspector docking or evidence expansion reducing font size.
The resize observer now updates the viewport for both cases, and focused browser
regressions pass with all four 150-service scenarios. CI and release-binary
browser jobs now run these scenarios before publication.

The earlier binary hashes and temporary screenshots above document local
iterations. Release identity and publication evidence come from the tagged
commit and the draft-first release workflow.

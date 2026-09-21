export const CARD_WIDTH = 200;
export const CARD_HEIGHT = 86;

const compare = (a, b) => a < b ? -1 : a > b ? 1 : 0;
const isHost = (node) => node.kind === "host" || node.id.startsWith("host/");

function sortedNodes(nodes) {
  return [...nodes].sort((a, b) => compare(a.id, b.id));
}

function validEdges(nodes, edges) {
  const ids = new Set(nodes.map((node) => node.id));
  return edges.filter((edge) => ids.has(edge.source) && ids.has(edge.target))
    .sort((a, b) => compare(a.source, b.source) || compare(a.target, b.target) || compare(a.kind || "", b.kind || ""));
}

// Components describe observed calls, never service placement on hosts.
export function partitionGraph(nodes, edges) {
  const services = sortedNodes(nodes.filter((node) => !isHost(node)));
  const calls = validEdges(services, edges.filter((edge) => edge.kind !== "runs_on"));
  const neighbors = new Map(services.map((node) => [node.id, new Set()]));
  const incoming = new Set();
  for (const edge of calls) {
    neighbors.get(edge.source).add(edge.target);
    neighbors.get(edge.target).add(edge.source);
    incoming.add(edge.target);
  }

  const disconnected = services.filter((node) => neighbors.get(node.id).size === 0);
  const visited = new Set(disconnected.map((node) => node.id));
  const components = [];
  for (const node of services) {
    if (visited.has(node.id)) continue;
    const nodeIds = [];
    const pending = [node.id];
    visited.add(node.id);
    while (pending.length) {
      const id = pending.pop();
      nodeIds.push(id);
      for (const neighbor of neighbors.get(id)) {
        if (visited.has(neighbor)) continue;
        visited.add(neighbor);
        pending.push(neighbor);
      }
    }
    nodeIds.sort(compare);
    const members = new Set(nodeIds);
    components.push({
      id: nodeIds[0],
      label: nodeIds.find((id) => !incoming.has(id)) || nodeIds[0],
      nodeIds,
      edgeCount: calls.filter((edge) => members.has(edge.source)).length,
    });
  }
  return { components, disconnected };
}

// Keep service identity and call evidence independent of metrics and host placement.
function observedGraph(nodes, edges) {
  const services = sortedNodes([...new Map(nodes.filter((node) => !isHost(node)).map((node) => [node.id, node])).values()]);
  return { services, calls: validEdges(services, edges.filter((edge) => edge.kind !== "runs_on")) };
}

function neighborhoodLimit(limit) {
  return Number.isFinite(limit) ? Math.max(1, Math.floor(limit)) : 8;
}

// Traverse each whole component once before chunking, so hubs do not strand
// their remaining neighbors as separate one-service neighborhoods.
export function partitionNeighborhoods(nodes, edges, limit = 8) {
  const { services, calls } = observedGraph(nodes, edges);
  const original = partitionGraph(services, calls);
  const neighbors = new Map(services.map((node) => [node.id, new Set()]));
  for (const edge of calls) {
    neighbors.get(edge.source).add(edge.target);
    neighbors.get(edge.target).add(edge.source);
  }
  const components = [];
  const size = neighborhoodLimit(limit);
  for (const component of original.components) {
    const pending = [component.nodeIds[0]];
    const visited = new Set(pending);
    for (let cursor = 0; cursor < pending.length; cursor++) {
      for (const id of [...neighbors.get(pending[cursor])].sort(compare)) {
        if (visited.has(id)) continue;
        visited.add(id);
        pending.push(id);
      }
    }
    for (let offset = 0; offset < pending.length; offset += size) {
      const nodeIds = pending.slice(offset, offset + size);
      const members = new Set(nodeIds);
      const internalEdges = calls.filter((edge) => members.has(edge.source) && members.has(edge.target));
      components.push({
        id: nodeIds[0],
        label: `Neighborhood ${String(components.length + 1).padStart(2, "0")}`,
        nodeIds,
        edgeCount: internalEdges.length,
        incomingEdges: calls.filter((edge) => !members.has(edge.source) && members.has(edge.target)),
        outgoingEdges: calls.filter((edge) => members.has(edge.source) && !members.has(edge.target)),
        internalEdges,
      });
    }
  }
  return { components, disconnected: original.disconnected, componentCount: original.components.length };
}

// Reserve a slot for the root before limiting its deterministic neighbor list.
export function boundedNeighborhood(nodes, edges, root, scope = "neighbors", limit = 8) {
  const { services, calls } = observedGraph(nodes, edges);
  const byId = new Map(services.map((node) => [node.id, node]));
  if (!byId.has(root)) return { nodes: [], edges: [], remainingIds: [] };
  const candidates = new Set();
  if (scope === "all") {
    for (const node of services) candidates.add(node.id);
  } else {
    for (const edge of calls) {
      if (scope !== "dependencies" && edge.target === root) candidates.add(edge.source);
      if (scope !== "callers" && edge.source === root) candidates.add(edge.target);
    }
  }
  candidates.delete(root);
  const ordered = [...candidates].sort(compare);
  const capacity = neighborhoodLimit(limit) - 1;
  const chosen = [root, ...ordered.slice(0, capacity)];
  const ids = new Set(chosen);
  return {
    nodes: chosen.map((id) => byId.get(id)),
    edges: calls.filter((edge) => ids.has(edge.source) && ids.has(edge.target)),
    remainingIds: ordered.slice(capacity),
  };
}

// Metrics and input ordering do not change a layout's identity.
export function topologyKey(nodes, edges) {
  return JSON.stringify([
    sortedNodes(nodes).map((node) => [node.id, node.kind || "", [...(node.hosts || [])].sort(compare)]),
    validEdges(nodes, edges).map((edge) => [edge.source, edge.target, edge.kind || ""]),
  ]);
}

// Positions are card centers; route points and bounds use the same SVG space.
export function layoutGraph(nodes, edges, spacing = {}) {
  const started = performance.now();
  const positions = new Map();
  const routes = new Map();
  if (!nodes.length) {
    return { positions, routes, bounds: { x: 0, y: 0, width: 0, height: 0 }, durationMs: performance.now() - started };
  }
  const dagre = globalThis.dagre;
  const graph = new dagre.graphlib.Graph().setGraph({
    nodesep: 42,
    ranksep: 176,
    ranker: "longest-path",
    edgesep: 24,
    marginx: 32,
    marginy: 32,
    ...spacing,
    rankdir: "LR",
  // Reserve real space for evidence labels; empty edge labels let long routes
  // cut across service cards when several dependency ranks share a column.
  }).setDefaultEdgeLabel(() => ({ width: 100, height: 24, labelpos: "c" }));
  for (const node of sortedNodes(nodes)) {
    graph.setNode(node.id, { width: CARD_WIDTH, height: CARD_HEIGHT });
  }
  for (const edge of validEdges(nodes, edges)) graph.setEdge(edge.source, edge.target);
  dagre.layout(graph);
  for (const id of graph.nodes()) {
    const { x, y } = graph.node(id);
    positions.set(id, { x, y });
  }
  for (const edge of graph.edges()) {
    routes.set(edge.v + ">" + edge.w, graph.edge(edge).points);
  }
  return {
    positions,
    routes,
    bounds: { x: 0, y: 0, width: graph.graph().width, height: graph.graph().height },
    durationMs: performance.now() - started,
  };
}

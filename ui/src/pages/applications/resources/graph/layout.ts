/*
 * Layered left-to-right layout for resource graphs (trees with occasional extra parents).
 *
 *  1. Cycle breaking: a DFS from the roots marks edges that close a cycle; they are ignored
 *     for layering and drawn as plain curves.
 *  2. Layering: longest path from the roots (Kahn order), so every edge points right.
 *  3. Spanning tree: each node's layout parent is its first parent in the previous layer.
 *  4. Ordering and rows: a tidy-tree pass over the spanning tree. Leaves take consecutive rows
 *     in child order, parents are centered on their children, so subtrees never interleave and
 *     sibling order (already sorted by the caller) is preserved. This is O(nodes + edges), which
 *     keeps thousands of nodes interactive; crossings only come from secondary parent edges.
 *  5. Edge routing: tree edges are orthogonal "bus" connectors (out of the parent, along a
 *     shared vertical trunk halfway between the layers, into the child); secondary edges are
 *     cubic curves.
 */

export interface LayoutNodeInput {
  id: string;
  width: number;
}

export interface LayoutEdgeInput {
  from: string;
  to: string;
}

export interface Box {
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface PositionedNode extends Box {
  id: string;
  layer: number;
}

export interface RoutedEdge {
  from: string;
  to: string;
  path: string;
  /** Part of the spanning tree (bus connector) rather than a secondary link. */
  primary: boolean;
  /** Bounding box, for viewport culling. */
  bounds: { x1: number; y1: number; x2: number; y2: number };
}

export interface LayoutResult {
  nodes: Map<string, PositionedNode>;
  edges: RoutedEdge[];
  width: number;
  height: number;
  /** Spanning-tree pre-order: the reading and keyboard order. */
  order: string[];
  /** Spanning-tree parent and children (for keyboard navigation). */
  parent: Map<string, string>;
  children: Map<string, string[]>;
}

export interface LayoutOptions {
  rowHeight?: number;
  rowGap?: number;
  layerGap?: number;
  margin?: number;
}

const DEFAULTS: Required<LayoutOptions> = { rowHeight: 56, rowGap: 12, layerGap: 72, margin: 24 };

export function layoutGraph(
  inputNodes: readonly LayoutNodeInput[],
  inputEdges: readonly LayoutEdgeInput[],
  roots: readonly string[],
  options: LayoutOptions = {},
): LayoutResult {
  const { rowHeight, rowGap, layerGap, margin } = { ...DEFAULTS, ...options };
  const ids = new Set(inputNodes.map((n) => n.id));
  const widthOf = new Map(inputNodes.map((n) => [n.id, n.width]));
  const out = new Map<string, string[]>();
  const edges = inputEdges.filter((e) => ids.has(e.from) && ids.has(e.to) && e.from !== e.to);
  for (const id of ids) out.set(id, []);
  const seenEdge = new Set<string>();
  const uniqueEdges: LayoutEdgeInput[] = [];
  for (const e of edges) {
    const k = `${e.from}\u0000${e.to}`;
    if (seenEdge.has(k)) continue;
    seenEdge.add(k);
    uniqueEdges.push(e);
    out.get(e.from)?.push(e.to);
  }

  // 1. Back edges via iterative DFS (white/grey/black) from roots, then any remaining node.
  const state = new Map<string, 0 | 1 | 2>();
  const back = new Set<string>();
  const startOrder = [...roots.filter((r) => ids.has(r)), ...ids];
  for (const start of startOrder) {
    if (state.get(start)) continue;
    const stack: [string, number][] = [[start, 0]];
    state.set(start, 1);
    while (stack.length > 0) {
      const top = stack[stack.length - 1]!;
      const [node, index] = top;
      const next = out.get(node) ?? [];
      if (index < next.length) {
        top[1] = index + 1;
        const child = next[index]!;
        const s = state.get(child) ?? 0;
        if (s === 1) back.add(`${node}\u0000${child}`);
        else if (s === 0) {
          state.set(child, 1);
          stack.push([child, 0]);
        }
      } else {
        state.set(node, 2);
        stack.pop();
      }
    }
  }
  const forward = uniqueEdges.filter((e) => !back.has(`${e.from}\u0000${e.to}`));
  const parentsOf = new Map<string, string[]>();
  const indegree = new Map<string, number>();
  for (const id of ids) {
    parentsOf.set(id, []);
    indegree.set(id, 0);
  }
  for (const e of forward) {
    parentsOf.get(e.to)?.push(e.from);
    indegree.set(e.to, (indegree.get(e.to) ?? 0) + 1);
  }

  // 2. Longest-path layering in Kahn order (roots first, in the given order).
  const layer = new Map<string, number>();
  const queue: string[] = [];
  const rootSet = new Set(roots);
  for (const id of [...roots, ...ids]) {
    if (ids.has(id) && (indegree.get(id) ?? 0) === 0 && !layer.has(id)) {
      layer.set(id, 0);
      queue.push(id);
    }
  }
  const remaining = new Map(indegree);
  // The array iterator sees elements pushed during the loop (queue semantics).
  for (const node of queue) {
    const l = layer.get(node) ?? 0;
    for (const child of out.get(node) ?? []) {
      if (back.has(`${node}\u0000${child}`)) continue;
      layer.set(child, Math.max(layer.get(child) ?? 0, l + 1));
      const left = (remaining.get(child) ?? 0) - 1;
      remaining.set(child, left);
      if (left === 0) queue.push(child);
    }
  }

  // 3. Spanning tree: first parent in the previous layer.
  const parent = new Map<string, string>();
  const children = new Map<string, string[]>();
  for (const id of ids) children.set(id, []);
  for (const e of forward) {
    if (parent.has(e.to)) continue;
    if ((layer.get(e.from) ?? 0) === (layer.get(e.to) ?? 0) - 1) {
      parent.set(e.to, e.from);
      children.get(e.from)?.push(e.to);
    }
  }
  const treeRoots = [...roots.filter((r) => ids.has(r)), ...[...ids].filter((id) => !parent.has(id) && !rootSet.has(id))];

  // 4. Tidy rows: post-order, leaves take the next row, parents center on their children.
  const centerY = new Map<string, number>();
  const order: string[] = [];
  let cursor = margin;
  const slot = rowHeight + rowGap;
  for (const root of treeRoots) {
    if (centerY.has(root)) continue;
    const stack: [string, number][] = [[root, 0]];
    order.push(root);
    while (stack.length > 0) {
      const top = stack[stack.length - 1]!;
      const [node, index] = top;
      const kids = children.get(node) ?? [];
      if (index < kids.length) {
        top[1] = index + 1;
        const child = kids[index]!;
        if (!centerY.has(child)) {
          order.push(child);
          stack.push([child, 0]);
        }
        continue;
      }
      stack.pop();
      const placed = kids.filter((k) => centerY.has(k));
      if (placed.length === 0) {
        centerY.set(node, cursor + rowHeight / 2);
        cursor += slot;
      } else {
        const first = centerY.get(placed[0]!) ?? cursor;
        const last = centerY.get(placed[placed.length - 1]!) ?? cursor;
        centerY.set(node, (first + last) / 2);
      }
    }
  }

  // Columns: each layer as wide as its widest node.
  const layerWidths: number[] = [];
  for (const id of ids) {
    const l = layer.get(id) ?? 0;
    layerWidths[l] = Math.max(layerWidths[l] ?? 0, widthOf.get(id) ?? 0);
  }
  const layerX: number[] = [];
  let x = margin;
  for (let l = 0; l < layerWidths.length; l += 1) {
    layerX[l] = x;
    x += (layerWidths[l] ?? 0) + layerGap;
  }

  const nodes = new Map<string, PositionedNode>();
  for (const id of ids) {
    const l = layer.get(id) ?? 0;
    const cy = centerY.get(id) ?? margin + rowHeight / 2;
    nodes.set(id, { id, layer: l, x: layerX[l] ?? margin, y: cy - rowHeight / 2, width: widthOf.get(id) ?? 0, height: rowHeight });
  }

  // 5. Edge routing.
  const routed: RoutedEdge[] = [];
  for (const e of uniqueEdges) {
    const a = nodes.get(e.from);
    const b = nodes.get(e.to);
    if (!a || !b) continue;
    const x1 = a.x + a.width;
    const y1 = a.y + a.height / 2;
    const x2 = b.x;
    const y2 = b.y + b.height / 2;
    const primary = parent.get(e.to) === e.from;
    let path: string;
    if (primary) {
      const xm = x1 + layerGap / 2;
      const dy = y2 - y1;
      if (Math.abs(dy) < 0.5) path = `M${x1},${y1}H${x2}`;
      else {
        const r = Math.min(6, Math.abs(dy) / 2);
        const s = Math.sign(dy);
        path = `M${x1},${y1}H${xm - r}Q${xm},${y1} ${xm},${y1 + s * r}V${y2 - s * r}Q${xm},${y2} ${xm + r},${y2}H${x2}`;
      }
    } else {
      const dx = Math.max(40, Math.abs(x2 - x1) / 2);
      path = `M${x1},${y1}C${x1 + dx},${y1} ${x2 - dx},${y2} ${x2},${y2}`;
    }
    routed.push({
      from: e.from,
      to: e.to,
      path,
      primary,
      bounds: { x1: Math.min(x1, x2), y1: Math.min(y1, y2), x2: Math.max(x1, x2), y2: Math.max(y1, y2) },
    });
  }

  const width = Math.max(0, x - layerGap + margin);
  const height = Math.max(cursor - rowGap + margin, margin * 2);
  return { nodes, edges: routed, width, height, order, parent, children };
}

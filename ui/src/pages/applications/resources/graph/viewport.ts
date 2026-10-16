import type { Box, LayoutResult } from './layout';

/** Pan/zoom transform: screen = graph * k + (x, y). */
export interface ViewTransform {
  x: number;
  y: number;
  k: number;
}

export interface Size {
  width: number;
  height: number;
}

export const MIN_ZOOM = 0.1;
export const MAX_ZOOM = 2;
export const ZOOM_STEP = 1.25;

export function clampZoom(k: number): number {
  return Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, k));
}

/** Zooms by `factor` keeping the screen point (cx, cy) fixed. */
export function zoomAt(view: ViewTransform, factor: number, cx: number, cy: number): ViewTransform {
  const k = clampZoom(view.k * factor);
  const ratio = k / view.k;
  return { k, x: cx - (cx - view.x) * ratio, y: cy - (cy - view.y) * ratio };
}

/** Transform that fits the whole graph in the viewport (never zooming in past 100%). */
export function fitView(graph: Size, viewport: Size, padding = 16): ViewTransform {
  if (viewport.width <= 0 || viewport.height <= 0 || graph.width <= 0 || graph.height <= 0) return { x: 0, y: 0, k: 1 };
  const k = clampZoom(Math.min((viewport.width - padding * 2) / graph.width, (viewport.height - padding * 2) / graph.height, 1));
  return { k, x: (viewport.width - graph.width * k) / 2, y: Math.max(padding, (viewport.height - graph.height * k) / 2) };
}

/** Pans the minimum needed so `box` (graph coordinates) is inside the viewport with `padding`. */
export function ensureVisible(view: ViewTransform, box: Box, viewport: Size, padding = 24): ViewTransform {
  if (viewport.width <= 0 || viewport.height <= 0) return view;
  const shift = (start: number, size: number, extent: number) => {
    const end = start + size;
    if (start < padding) return padding - start;
    if (end > extent - padding) return Math.max(padding - start, extent - padding - end);
    return 0;
  };
  const dx = shift(box.x * view.k + view.x, box.width * view.k, viewport.width);
  const dy = shift(box.y * view.k + view.y, box.height * view.k, viewport.height);
  return dx === 0 && dy === 0 ? view : { ...view, x: view.x + dx, y: view.y + dy };
}

/** Viewport rectangle in graph coordinates, grown by `margin` screen pixels. */
export function visibleRect(view: ViewTransform, viewport: Size, margin = 200) {
  return {
    x1: (-view.x - margin) / view.k,
    y1: (-view.y - margin) / view.k,
    x2: (viewport.width - view.x + margin) / view.k,
    y2: (viewport.height - view.y + margin) / view.k,
  };
}

/**
 * Ids of nodes and indexes of edges intersecting the viewport. An unknown viewport size
 * (0×0, e.g. before the first measurement) disables culling.
 */
export function cullLayout(layout: LayoutResult, view: ViewTransform, viewport: Size, margin = 200): { nodes: Set<string>; edges: number[] } {
  const nodes = new Set<string>();
  const edges: number[] = [];
  const cull = viewport.width > 0 && viewport.height > 0;
  const r = visibleRect(view, viewport, margin);
  for (const node of layout.nodes.values()) {
    if (!cull || (node.x + node.width >= r.x1 && node.x <= r.x2 && node.y + node.height >= r.y1 && node.y <= r.y2)) nodes.add(node.id);
  }
  layout.edges.forEach((edge, index) => {
    const b = edge.bounds;
    if (!cull || (b.x2 >= r.x1 && b.x1 <= r.x2 && b.y2 >= r.y1 && b.y1 <= r.y2)) edges.push(index);
  });
  return { nodes, edges };
}

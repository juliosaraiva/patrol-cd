import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type MouseEvent, type PointerEvent } from 'react';
import { Button, IconButton } from '../../../../design';
import { IconMinus, IconPlus } from '../../../../design/icons';
import { GraphNodeView, nodeWidth } from './GraphNodeView';
import { layoutGraph, type LayoutResult } from './layout';
import type { Graph, GraphNode } from './types';
import { ZOOM_STEP, clampZoom, cullLayout, ensureVisible, fitView, zoomAt, type Size, type ViewTransform } from './viewport';
import styles from './Graph.module.css';

export interface GraphCanvasProps {
  graph: Graph;
  /** Accessible name of the tree (e.g. "Resource tree of guestbook"). */
  label: string;
  /** Node shown as selected (its details are open). */
  selectedId?: string | undefined;
  /** Enter, Space or click on a node. */
  onActivate: (node: GraphNode) => void;
  /** Expand (true) or collapse (false) a node with children (ArrowRight / ArrowLeft). */
  onToggle?: ((node: GraphNode, expand: boolean) => void) | undefined;
  /** Initial zoom and persistence of zoom changes. */
  zoom?: number | undefined;
  onZoomChange?: ((zoom: number) => void) | undefined;
}

const PAN_STEP = 80;
const UNMEASURED: Size = { width: 0, height: 0 };
const NOTHING_VISIBLE: ReturnType<typeof cullLayout> = { nodes: new Set(), edges: [] };

/** Client size of the element; undefined until it is measured, right after the first render. */
function useElementSize(ref: React.RefObject<HTMLElement | null>): Size | undefined {
  const [size, setSize] = useState<Size | undefined>(undefined);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return undefined;
    const measure = () => setSize((prev) => (prev?.width === el.clientWidth && prev.height === el.clientHeight ? prev : { width: el.clientWidth, height: el.clientHeight }));
    measure();
    if (typeof ResizeObserver === 'undefined') return undefined;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref]);
  return size;
}

/** Tree metadata (level, set size, position) of every node in the layout's spanning tree. */
function treeMeta(layout: LayoutResult) {
  const meta = new Map<string, { level: number; setSize: number; posInSet: number }>();
  const roots = layout.order.filter((id) => !layout.parent.has(id));
  roots.forEach((id, i) => meta.set(id, { level: 1, setSize: roots.length, posInSet: i + 1 }));
  for (const id of layout.order) {
    const level = meta.get(id)?.level ?? 1;
    const kids = layout.children.get(id) ?? [];
    kids.forEach((kid, i) => meta.set(kid, { level: level + 1, setSize: kids.length, posInSet: i + 1 }));
  }
  return meta;
}

/**
 * Pannable, zoomable SVG drawing of a layered graph. Only nodes and edges near the viewport are
 * rendered, so graphs with thousands of nodes stay responsive. Nodes form an ARIA tree with a
 * roving tab stop: arrow keys follow the layout tree, Enter opens, Shift+arrows pan, + and −
 * zoom, 0 fits the graph.
 */
export function GraphCanvas({ graph, label, selectedId, onActivate, onToggle, zoom = 1, onZoomChange }: GraphCanvasProps) {
  const helpId = useId();
  const viewportRef = useRef<HTMLDivElement>(null);
  const measured = useElementSize(viewportRef);
  const size = measured ?? UNMEASURED;
  const nodeById = useMemo(() => new Map(graph.nodes.map((n) => [n.id, n])), [graph]);
  const layout = useMemo(
    () => layoutGraph(graph.nodes.map((n) => ({ id: n.id, width: nodeWidth(n) })), graph.edges, graph.roots),
    [graph],
  );
  const meta = useMemo(() => treeMeta(layout), [layout]);
  const dashed = useMemo(() => new Set(graph.edges.filter((e) => e.dashed).map((e) => `${e.from}\u0000${e.to}`)), [graph]);
  const [view, setView] = useState<ViewTransform>(() => ({ x: 0, y: 0, k: clampZoom(zoom) }));
  const [activeId, setActiveId] = useState<string | undefined>(undefined);
  const nodeRefs = useRef(new Map<string, SVGGElement>());
  const pendingFocus = useRef<string | null>(null);
  const viewRef = useRef(view);
  viewRef.current = view;

  const active = activeId && layout.nodes.has(activeId) ? activeId : layout.order[0];
  // Before the measurement (a layout effect, so before the first paint) only the tab stop and the
  // selected node are drawn: culling needs the size, and mounting every node of a large graph for
  // that one render costs seconds. A viewport that measures 0×0 (never laid out, e.g. jsdom)
  // disables culling.
  const culled = useMemo(() => (measured ? cullLayout(layout, view, measured) : NOTHING_VISIBLE), [layout, view, measured]);
  // The tab stop and the selected node stay mounted when panned out of view (focus survives).
  const renderIds = new Set(culled.nodes);
  if (active) renderIds.add(active);
  if (selectedId && layout.nodes.has(selectedId)) renderIds.add(selectedId);

  // First measurement: bring the root into view (tidy layouts center it on all its leaves,
  // which can be far below the top of a large graph).
  const placed = useRef(false);
  useLayoutEffect(() => {
    if (placed.current || size.width <= 0 || size.height <= 0) return;
    const root = layout.order[0] ? layout.nodes.get(layout.order[0]) : undefined;
    if (!root) return;
    placed.current = true;
    setView((v) => {
      if (layout.height * v.k <= size.height) return v;
      const centered = size.height / 2 - (root.y + root.height / 2) * v.k;
      return { ...v, y: Math.min(0, Math.max(size.height - layout.height * v.k, centered)) };
    });
  }, [layout, size]);

  useEffect(() => {
    const id = pendingFocus.current;
    if (!id) return;
    const el = nodeRefs.current.get(id);
    if (el) {
      pendingFocus.current = null;
      el.focus({ preventScroll: true });
    }
  });

  // Persist zoom changes (after the gesture settles).
  useEffect(() => {
    if (!onZoomChange) return undefined;
    const t = setTimeout(() => onZoomChange(view.k), 300);
    return () => clearTimeout(t);
  }, [view.k, onZoomChange]);

  const updateView = useCallback((next: ViewTransform | ((v: ViewTransform) => ViewTransform)) => setView(next), []);

  const zoomBy = (factor: number) => updateView((v) => zoomAt(v, factor, size.width / 2 || 0, size.height / 2 || 0));
  const fit = () => updateView(fitView({ width: layout.width, height: layout.height }, size));
  const resetZoom = () => updateView((v) => zoomAt(v, 1 / v.k, size.width / 2 || 0, size.height / 2 || 0));

  const moveTo = (id: string | undefined) => {
    if (!id) return;
    const box = layout.nodes.get(id);
    setActiveId(id);
    pendingFocus.current = id;
    if (box) updateView((v) => ensureVisible(v, box, size));
  };

  const onNodeKeyDown = (event: KeyboardEvent<SVGGElement>, node: GraphNode) => {
    const index = layout.order.indexOf(node.id);
    if (event.shiftKey && event.key.startsWith('Arrow')) {
      event.preventDefault();
      const dx = event.key === 'ArrowLeft' ? PAN_STEP : event.key === 'ArrowRight' ? -PAN_STEP : 0;
      const dy = event.key === 'ArrowUp' ? PAN_STEP : event.key === 'ArrowDown' ? -PAN_STEP : 0;
      updateView((v) => ({ ...v, x: v.x + dx, y: v.y + dy }));
      return;
    }
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault();
        moveTo(layout.order[Math.min(layout.order.length - 1, index + 1)]);
        break;
      case 'ArrowUp':
        event.preventDefault();
        moveTo(layout.order[Math.max(0, index - 1)]);
        break;
      case 'Home':
        event.preventDefault();
        moveTo(layout.order[0]);
        break;
      case 'End':
        event.preventDefault();
        moveTo(layout.order[layout.order.length - 1]);
        break;
      case 'ArrowRight': {
        event.preventDefault();
        const kids = layout.children.get(node.id) ?? [];
        if (node.expanded === false && onToggle) onToggle(node, true);
        else if (kids.length > 0) moveTo(kids[0]);
        break;
      }
      case 'ArrowLeft': {
        event.preventDefault();
        const kids = layout.children.get(node.id) ?? [];
        if (node.expanded === true && kids.length > 0 && onToggle && node.type !== 'app') onToggle(node, false);
        else moveTo(layout.parent.get(node.id));
        break;
      }
      case 'Enter':
      case ' ':
        event.preventDefault();
        onActivate(node);
        break;
      case '+':
      case '=':
        event.preventDefault();
        zoomBy(ZOOM_STEP);
        break;
      case '-':
      case '_':
        event.preventDefault();
        zoomBy(1 / ZOOM_STEP);
        break;
      case '0':
        event.preventDefault();
        fit();
        break;
      default:
        break;
    }
  };

  const onNodeClick = (event: MouseEvent<SVGGElement>, node: GraphNode) => {
    if (dragged.current) return;
    event.stopPropagation();
    setActiveId(node.id);
    onActivate(node);
  };

  const onFocusNode = useCallback((node: GraphNode) => setActiveId(node.id), []);

  // Pointer panning on the background.
  const drag = useRef<{ id: number; x: number; y: number; vx: number; vy: number } | null>(null);
  const dragged = useRef(false);
  const onPointerDown = (event: PointerEvent<SVGSVGElement>) => {
    if (event.button !== 0) return;
    dragged.current = false;
    drag.current = { id: event.pointerId, x: event.clientX, y: event.clientY, vx: viewRef.current.x, vy: viewRef.current.y };
  };
  const onPointerMove = (event: PointerEvent<SVGSVGElement>) => {
    const d = drag.current;
    if (d?.id !== event.pointerId) return;
    const dx = event.clientX - d.x;
    const dy = event.clientY - d.y;
    if (!dragged.current && Math.abs(dx) + Math.abs(dy) < 4) return;
    if (!dragged.current) {
      dragged.current = true;
      event.currentTarget.setPointerCapture?.(event.pointerId);
    }
    updateView((v) => ({ ...v, x: d.vx + dx, y: d.vy + dy }));
  };
  const endDrag = (event: PointerEvent<SVGSVGElement>) => {
    if (drag.current?.id !== event.pointerId) return;
    drag.current = null;
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
    // Let the click that ends a drag be ignored, then reset.
    setTimeout(() => {
      dragged.current = false;
    }, 0);
  };

  // Wheel: pan; Ctrl/⌘ + wheel (and trackpad pinch) zooms around the pointer. Non-passive so the
  // page does not scroll underneath.
  useEffect(() => {
    const el = viewportRef.current;
    if (!el) return undefined;
    const onWheel = (event: WheelEvent) => {
      event.preventDefault();
      if (event.ctrlKey || event.metaKey) {
        const rect = el.getBoundingClientRect();
        const factor = Math.exp(-event.deltaY * 0.0025);
        setView((v) => zoomAt(v, factor, event.clientX - rect.left, event.clientY - rect.top));
      } else {
        setView((v) => ({ ...v, x: v.x - event.deltaX, y: v.y - event.deltaY }));
      }
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  }, []);

  const nodeIds = [...renderIds];
  // Level of detail: zoomed far out, nodes are drawn as outlines with their status only.
  const detailed = view.k >= 0.45;
  return (
    <div className={styles.canvas}>
      <div className={styles.toolbar} role="toolbar" aria-label="Graph zoom">
        <IconButton aria-label="Zoom out" icon={<IconMinus />} size="sm" variant="secondary" onClick={() => zoomBy(1 / ZOOM_STEP)} />
        <Button size="sm" variant="ghost" onClick={resetZoom} aria-label={`Zoom ${Math.round(view.k * 100)}%, reset to 100%`}>
          {Math.round(view.k * 100)}%
        </Button>
        <IconButton aria-label="Zoom in" icon={<IconPlus />} size="sm" variant="secondary" onClick={() => zoomBy(ZOOM_STEP)} />
        <Button size="sm" variant="secondary" onClick={fit}>
          Fit to screen
        </Button>
        <p id={helpId} className={styles.help}>
          Arrow keys move between resources, Enter opens details, Shift+arrows pan, + and − zoom, 0 fits.
        </p>
      </div>
      <div ref={viewportRef} className={styles.viewport}>
        <svg
          className={styles.svg}
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={endDrag}
          onPointerCancel={endDrag}
          data-node-count={layout.nodes.size}
          data-rendered-count={nodeIds.length}
        >
          <g transform={`translate(${view.x},${view.y}) scale(${view.k})`}>
            <g aria-hidden="true">
              {culled.edges.map((i) => {
                const edge = layout.edges[i];
                if (!edge) return null;
                return (
                  <path
                    key={`${edge.from}->${edge.to}`}
                    d={edge.path}
                    className={[edge.primary ? styles.edge : styles.edgeSecondary, dashed.has(`${edge.from}\u0000${edge.to}`) ? styles.edgeDashed : undefined]
                      .filter(Boolean)
                      .join(' ')}
                  />
                );
              })}
            </g>
            <g role="tree" aria-label={label} aria-describedby={helpId}>
              {nodeIds.map((id) => {
                const node = nodeById.get(id);
                const box = layout.nodes.get(id);
                if (!node || !box) return null;
                const m = meta.get(id) ?? { level: 1, setSize: 1, posInSet: 1 };
                return (
                  <GraphNodeView
                    key={id}
                    node={node}
                    box={box}
                    level={m.level}
                    setSize={m.setSize}
                    posInSet={m.posInSet}
                    tabbable={id === active}
                    selected={id === selectedId}
                    detailed={detailed}
                    nodeRef={(el) => {
                      if (el) nodeRefs.current.set(id, el);
                      else nodeRefs.current.delete(id);
                    }}
                    onKeyDown={onNodeKeyDown}
                    onClick={onNodeClick}
                    onFocusNode={onFocusNode}
                  />
                );
              })}
            </g>
          </g>
        </svg>
      </div>
    </div>
  );
}

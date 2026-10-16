import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { Badge, HealthStatus, Mono, SyncStatus } from '../../../../design';
import { IconChevronDown, IconChevronRight } from '../../../../design/icons';
import { nodeWidth } from '../graph/GraphNodeView';
import { layoutGraph } from '../graph/layout';
import type { Graph, GraphNode } from '../graph/types';
import { kindCaption } from '../model/kinds';
import { KindIcon } from '../shared/KindIcon';
import styles from './Views.module.css';

const ROW_HEIGHT = 40;
const OVERSCAN = 10;
const WINDOW_THRESHOLD = 150;

export interface OutlineTreeProps {
  graph: Graph;
  label: string;
  selectedId?: string | undefined;
  onActivate: (node: GraphNode) => void;
  onToggle?: ((node: GraphNode, expand: boolean) => void) | undefined;
}

/**
 * Text outline of the same graph the tree view draws: an APG tree (flat rows with
 * aria-level/setsize/posinset) that screen readers and narrow screens read linearly. Large
 * outlines render only the rows near the scroll position.
 */
export function OutlineTree({ graph, label, selectedId, onActivate, onToggle }: OutlineTreeProps) {
  const layout = useMemo(() => layoutGraph(graph.nodes.map((n) => ({ id: n.id, width: nodeWidth(n) })), graph.edges, graph.roots), [graph]);
  const nodeById = useMemo(() => new Map(graph.nodes.map((n) => [n.id, n])), [graph]);
  const levels = useMemo(() => {
    const result = new Map<string, { level: number; setSize: number; posInSet: number }>();
    const roots = layout.order.filter((id) => !layout.parent.has(id));
    roots.forEach((id, i) => result.set(id, { level: 1, setSize: roots.length, posInSet: i + 1 }));
    for (const id of layout.order) {
      const level = result.get(id)?.level ?? 1;
      const kids = layout.children.get(id) ?? [];
      kids.forEach((k, i) => result.set(k, { level: level + 1, setSize: kids.length, posInSet: i + 1 }));
    }
    return result;
  }, [layout]);
  const order = layout.order;
  const [activeId, setActiveId] = useState<string | undefined>(undefined);
  const active = activeId && nodeById.has(activeId) ? activeId : order[0];
  const activeIndex = active ? order.indexOf(active) : 0;
  const scrollRef = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const rowRefs = useRef(new Map<string, HTMLDivElement>());
  const pendingFocus = useRef<string | null>(null);

  const windowed = order.length > WINDOW_THRESHOLD;
  const viewportHeight = 560;
  const start = windowed ? Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN) : 0;
  const end = windowed ? Math.min(order.length, Math.ceil((scrollTop + viewportHeight) / ROW_HEIGHT) + OVERSCAN) : order.length;
  const indexes: number[] = [];
  for (let i = start; i < end; i += 1) indexes.push(i);
  if (windowed && (activeIndex < start || activeIndex >= end)) indexes.push(activeIndex);
  indexes.sort((a, b) => a - b);

  useEffect(() => {
    const id = pendingFocus.current;
    if (!id) return;
    const el = rowRefs.current.get(id);
    if (el) {
      pendingFocus.current = null;
      el.focus({ preventScroll: windowed });
    }
  });

  const moveTo = (index: number) => {
    const id = order[Math.max(0, Math.min(order.length - 1, index))];
    if (!id) return;
    setActiveId(id);
    pendingFocus.current = id;
    const container = scrollRef.current;
    if (windowed && container) {
      const top = order.indexOf(id) * ROW_HEIGHT;
      if (top < container.scrollTop) container.scrollTop = top;
      else if (top + ROW_HEIGHT > container.scrollTop + container.clientHeight) container.scrollTop = top + ROW_HEIGHT - container.clientHeight;
      setScrollTop(container.scrollTop);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>, node: GraphNode, index: number) => {
    const kids = layout.children.get(node.id) ?? [];
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault();
        moveTo(index + 1);
        break;
      case 'ArrowUp':
        event.preventDefault();
        moveTo(index - 1);
        break;
      case 'Home':
        event.preventDefault();
        moveTo(0);
        break;
      case 'End':
        event.preventDefault();
        moveTo(order.length - 1);
        break;
      case 'ArrowRight':
        event.preventDefault();
        if (node.expanded === false && onToggle) onToggle(node, true);
        else if (kids[0]) moveTo(order.indexOf(kids[0]));
        break;
      case 'ArrowLeft': {
        event.preventDefault();
        const parent = layout.parent.get(node.id);
        if (node.expanded === true && kids.length > 0 && onToggle && node.type !== 'app') onToggle(node, false);
        else if (parent) moveTo(order.indexOf(parent));
        break;
      }
      case 'Enter':
      case ' ':
        event.preventDefault();
        onActivate(node);
        break;
      default:
        break;
    }
  };

  const rows = [];
  let previous = -1;
  for (const index of indexes) {
    if (windowed && index > previous + 1) {
      rows.push(<div key={`gap-${index}`} aria-hidden="true" style={{ height: (index - previous - 1) * ROW_HEIGHT }} />);
    }
    previous = index;
    const id = order[index];
    const node = id ? nodeById.get(id) : undefined;
    if (!node || !id) continue;
    const meta = levels.get(id) ?? { level: 1, setSize: 1, posInSet: 1 };
    const item = node.item;
    rows.push(
      // APG treeitem: focusable row with keyboard handling.
      <div
        key={id}
        ref={(el) => {
          if (el) rowRefs.current.set(id, el);
          else rowRefs.current.delete(id);
        }}
        role="treeitem"
        aria-level={meta.level}
        aria-setsize={meta.setSize}
        aria-posinset={meta.posInSet}
        aria-expanded={node.type === 'group' ? false : node.expanded}
        aria-selected={id === selectedId}
        tabIndex={id === active ? 0 : -1}
        className={[styles.outlineRow, id === selectedId ? styles.outlineSelected : undefined, node.dimmed ? styles.outlineDimmed : undefined]
          .filter(Boolean)
          .join(' ')}
        style={{ paddingInlineStart: `calc(${(meta.level - 1) * 20}px + var(--space-3))` }}
        onKeyDown={(e) => onKeyDown(e, node, index)}
        onClick={() => {
          setActiveId(id);
          onActivate(node);
        }}
        onFocus={(e) => {
          if (e.target === e.currentTarget) setActiveId(id);
        }}
      >
        <span className={styles.outlineToggle} aria-hidden="true">
          {node.expanded === true ? <IconChevronDown size={12} /> : node.expanded === false || node.type === 'group' ? <IconChevronRight size={12} /> : null}
        </span>
        {node.type === 'more' || node.type === 'traffic' ? (
          <span className={styles.outlineName}>{node.label}</span>
        ) : (
          <>
            <KindIcon kind={node.kind} size={16} />
            <span className={styles.outlineKind}>{node.type === 'group' ? node.kind : node.type === 'app' ? 'Application' : kindCaption(node.kind)}</span>
            <Mono className={styles.outlineName}>{node.type === 'group' ? `× ${node.members?.length ?? 0}` : node.label}</Mono>
            {node.type !== 'group' ? <HealthStatus status={node.health} /> : null}
            {node.type === 'app' || item?.managed ? <SyncStatus status={node.sync} /> : null}
            {item?.hook ? <Badge tone="info">Hook</Badge> : null}
            {item?.requiresPruning ? <Badge tone="warning">Requires pruning</Badge> : null}
            {item?.orphaned ? <Badge>Orphaned</Badge> : null}
            {node.collapsedCount ? <Badge>{`${node.collapsedCount} hidden`}</Badge> : null}
          </>
        )}
      </div>,
    );
  }
  if (windowed && previous < order.length - 1) {
    rows.push(<div key="gap-end" aria-hidden="true" style={{ height: (order.length - previous - 1) * ROW_HEIGHT }} />);
  }

  return (
    <div
      ref={scrollRef}
      className={styles.outline}
      style={windowed ? { maxHeight: viewportHeight } : undefined}
      onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
    >
      <div role="tree" aria-label={label}>
        {rows}
      </div>
    </div>
  );
}

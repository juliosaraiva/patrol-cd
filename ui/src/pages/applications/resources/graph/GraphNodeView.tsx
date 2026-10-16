import { memo, type KeyboardEvent, type MouseEvent, type Ref } from 'react';
import { HEALTH_GLYPHS, SYNC_GLYPHS } from '../../../../design/status/glyphs';
import { kindCaption } from '../model/kinds';
import { KindIcon } from '../shared/KindIcon';
import type { PositionedNode } from './layout';
import { HEALTH_COLOR, SYNC_COLOR, healthLabel, healthState, syncLabel, syncState } from './statusText';
import type { GraphNode } from './types';
import styles from './Graph.module.css';

/** Node widths per type; heights come from the layout row height. */
export function nodeWidth(node: GraphNode): number {
  switch (node.type) {
    case 'more':
      return 168;
    case 'traffic':
      return 176;
    default:
      return 236;
  }
}

const CHAR_WIDTH = 7.4; // ~13px monospace advance
const SMALL_CHAR = 6.6; // 11px sans

function truncate(text: string, width: number, charWidth = CHAR_WIDTH): string {
  const max = Math.max(4, Math.floor(width / charWidth));
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

/** Accessible name of a node: kind, name, namespace and every status shown on it. */
export function nodeAccessibleName(node: GraphNode): string {
  switch (node.type) {
    case 'app':
      return [`Application ${node.label}`, healthLabel(node.health), syncLabel(node.sync)].join(', ');
    case 'traffic':
      return node.label;
    case 'more':
      return `${node.label} resources`;
    case 'group': {
      const counts = new Map<string, number>();
      for (const m of node.members ?? []) {
        const h = m.health?.status ?? 'Unknown';
        counts.set(h, (counts.get(h) ?? 0) + 1);
      }
      const summary = [...counts].map(([h, n]) => `${n} ${h}`).join(', ');
      return `Group of ${node.members?.length ?? 0} ${node.kind} resources${summary ? `: ${summary}` : ''}`;
    }
    case 'resource': {
      const item = node.item;
      const parts = [`${node.kind} ${node.label}`];
      if (item?.namespace) parts.push(`namespace ${item.namespace}`);
      parts.push(healthLabel(node.health));
      if (item?.managed) parts.push(syncLabel(node.sync));
      if (item?.hook) parts.push('sync hook');
      if (item?.requiresPruning) parts.push('requires pruning');
      if (item?.orphaned) parts.push('orphaned');
      if (node.dimmed) parts.push('does not match the filters');
      if (node.collapsedCount) parts.push(`${node.collapsedCount} hidden descendants`);
      return parts.join(', ');
    }
  }
}

interface Badge {
  text: string;
  tone: 'neutral' | 'warning' | 'accent';
}

function badgesFor(node: GraphNode): Badge[] {
  const badges: Badge[] = [];
  const item = node.item;
  if (item?.hook) badges.push({ text: 'hook', tone: 'accent' });
  if (item?.requiresPruning) badges.push({ text: 'prune', tone: 'warning' });
  if (item?.orphaned) badges.push({ text: 'orphan', tone: 'neutral' });
  if (node.collapsedCount) badges.push({ text: `+${node.collapsedCount}`, tone: 'neutral' });
  return badges;
}

function HealthGlyph({ status, x, y }: { status: string | undefined; x: number; y: number }) {
  const state = healthState(status);
  return (
    <g transform={`translate(${x},${y})`} style={{ color: HEALTH_COLOR[state] }} data-health={state}>
      {HEALTH_GLYPHS[state]}
    </g>
  );
}

function SyncGlyph({ status, x, y }: { status: string | undefined; x: number; y: number }) {
  const state = syncState(status);
  return (
    <g transform={`translate(${x},${y})`} style={{ color: SYNC_COLOR[state] }} data-sync={state}>
      <rect x={0.5} y={0.5} width={15} height={15} rx={2} fill="none" stroke="currentColor" />
      <g transform="translate(2,2)">{SYNC_GLYPHS[state]}</g>
    </g>
  );
}

export interface GraphNodeViewProps {
  node: GraphNode;
  box: PositionedNode;
  level: number;
  setSize: number;
  posInSet: number;
  tabbable: boolean;
  selected: boolean;
  /** Full content; false draws only the frame and health (zoomed far out). */
  detailed?: boolean;
  nodeRef: Ref<SVGGElement>;
  onKeyDown: (event: KeyboardEvent<SVGGElement>, node: GraphNode) => void;
  onClick: (event: MouseEvent<SVGGElement>, node: GraphNode) => void;
  onFocusNode: (node: GraphNode) => void;
}

/** One graph node: an APG treeitem drawn in SVG. */
export const GraphNodeView = memo(function GraphNodeView({
  node,
  box,
  level,
  setSize,
  posInSet,
  tabbable,
  selected,
  detailed = true,
  nodeRef,
  onKeyDown,
  onClick,
  onFocusNode,
}: GraphNodeViewProps) {
  const { width } = box;
  const height = node.type === 'more' || node.type === 'traffic' ? 40 : box.height;
  const top = (box.height - height) / 2;
  const classes = [
    styles.node,
    styles[`node_${node.type}`],
    selected ? styles.selected : undefined,
    node.dimmed ? styles.dimmed : undefined,
    node.item?.orphaned ? styles.orphaned : undefined,
  ]
    .filter(Boolean)
    .join(' ');
  const textX = 52;
  const textWidth = width - textX - 10;
  const badges = badgesFor(node);

  let body;
  if (!detailed) {
    body = node.type === 'resource' || node.type === 'app' || node.type === 'group' ? <HealthGlyph status={node.health} x={12} y={top + (height - 14) / 2} /> : null;
  } else if (node.type === 'more') {
    body = (
      <>
        <path d={`M16 ${top + 20}h10M21 ${top + 15}v10`} className={styles.moreIcon} />
        <text x={36} y={top + 24} className={styles.moreText}>
          {node.label}
        </text>
      </>
    );
  } else if (node.type === 'traffic') {
    body = (
      <>
        <path d={`M12 ${top + 20}h14M21 ${top + 15}l5 5-5 5`} className={styles.moreIcon} />
        <text x={34} y={top + 24} className={styles.trafficText}>
          {node.label}
        </text>
      </>
    );
  } else {
    const isGroup = node.type === 'group';
    const title = isGroup ? `${node.kind} × ${node.members?.length ?? 0}` : node.label;
    const groupCounts = isGroup ? healthCounts(node) : [];
    body = (
      <>
        <KindIcon kind={node.kind} x={14} y={top + 8} size={24} className={styles.kindIcon} />
        {node.type !== 'app' ? (
          <text x={26} y={top + 47} className={styles.caption} textAnchor="middle">
            {truncate(kindCaption(node.kind), 40, SMALL_CHAR)}
          </text>
        ) : null}
        <text x={textX} y={top + 21} className={isGroup ? styles.groupTitle : styles.name}>
          {truncate(title, textWidth, isGroup ? SMALL_CHAR + 0.6 : CHAR_WIDTH)}
        </text>
        {isGroup ? (
          groupCounts.slice(0, 4).map(([state, count], i) => (
            <g key={state}>
              <HealthGlyph status={state} x={textX + i * 44} y={top + 31} />
              <text x={textX + i * 44 + 18} y={top + 43} className={styles.count}>
                {count}
              </text>
            </g>
          ))
        ) : (
          <>
            <HealthGlyph status={node.health} x={textX} y={top + 31} />
            {node.type === 'app' || node.item?.managed ? <SyncGlyph status={node.sync} x={textX + 20} y={top + 30} /> : null}
            <Badges badges={badges} x={textX + (node.type === 'app' || node.item?.managed ? 42 : 20)} y={top + 30} maxX={width - 8} />
          </>
        )}
      </>
    );
  }

  return (
    <g
      ref={nodeRef}
      role="treeitem"
      aria-label={nodeAccessibleName(node)}
      aria-level={level}
      aria-setsize={setSize}
      aria-posinset={posInSet}
      aria-expanded={node.type === 'group' ? false : node.expanded}
      aria-selected={selected}
      tabIndex={tabbable ? 0 : -1}
      data-node-id={node.id}
      transform={`translate(${box.x},${box.y})`}
      className={classes}
      onKeyDown={(e) => onKeyDown(e, node)}
      onClick={(e) => onClick(e, node)}
      onFocus={() => onFocusNode(node)}
    >
      <title>{nodeAccessibleName(node)}</title>
      <rect x={-3} y={top - 3} width={width + 6} height={height + 6} rx={4} className={styles.focusRing} />
      <rect x={0.5} y={top + 0.5} width={width - 1} height={height - 1} rx={4} className={styles.frame} />
      {body}
    </g>
  );
});

function healthCounts(node: GraphNode): [string, number][] {
  const order = ['Degraded', 'Missing', 'Progressing', 'Suspended', 'Unknown', 'Healthy'];
  const counts = new Map<string, number>();
  for (const m of node.members ?? []) {
    const h = healthState(m.health?.status);
    counts.set(h, (counts.get(h) ?? 0) + 1);
  }
  return order.filter((s) => counts.has(s)).map((s) => [s, counts.get(s) ?? 0]);
}

function Badges({ badges, x, y, maxX }: { badges: Badge[]; x: number; y: number; maxX: number }) {
  let cursor = x;
  return (
    <>
      {badges.map((b) => {
        const w = Math.ceil(b.text.length * 6.2 + 10);
        if (cursor + w > maxX) return null;
        const at = cursor;
        cursor += w + 4;
        return (
          <g key={b.text} transform={`translate(${at},${y})`} className={styles[`badge_${b.tone}`]}>
            <rect x={0.5} y={0.5} width={w - 1} height={15} rx={2} className={styles.badgeFrame} />
            <text x={w / 2} y={11.5} textAnchor="middle" className={styles.badgeText}>
              {b.text}
            </text>
          </g>
        );
      })}
    </>
  );
}

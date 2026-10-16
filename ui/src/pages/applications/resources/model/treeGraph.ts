import type { Application } from '../../../../api/types';
import type { Graph, GraphEdge, GraphNode } from '../graph/types';
import { compareItems, type ResourceItem, type ResourceModel } from './resources';

export const APP_NODE_ID = '__app__';

export interface TreeGraphOptions {
  /** Narrowing filter; non-matching resources are hidden unless an ancestor of a match. */
  matcher: (item: ResourceItem) => boolean;
  filtering: boolean;
  /** Node ids whose children are hidden. */
  collapsed: ReadonlySet<string>;
  /** Parents that show all their children (no "show more" node). */
  expandedParents: ReadonlySet<string>;
  /** Expanded group keys (see groupKey). */
  expandedGroups: ReadonlySet<string>;
  /** Group same-kind leaf siblings (e.g. the pods of a ReplicaSet) into one node. */
  groupSimilar: boolean;
  /** Children shown per parent before a "show more" node (default 25). */
  childLimit?: number;
  /** Minimum same-kind leaf siblings that form a group (default 5). */
  groupThreshold?: number;
}

export function groupKey(parentId: string, kind: string): string {
  return `${parentId}\u0000${kind}`;
}

export interface TreeStructure {
  /** Children of each node id (APP_NODE_ID for the application root), unsorted. */
  children: Map<string, ResourceItem[]>;
  /** Primary (layout) parent of each item. */
  primaryParent: Map<string, string>;
  /** Every resolvable parent link (child id → parent ids), for secondary edges. */
  parents: Map<string, string[]>;
}

/**
 * Parent/child structure of the tree: managed resources and resources without a resolvable
 * parent hang off the application; everything else hangs off its parentRefs (the first
 * resolvable one is the layout parent).
 */
export function treeStructure(model: ResourceModel): TreeStructure {
  const children = new Map<string, ResourceItem[]>();
  const primaryParent = new Map<string, string>();
  const parents = new Map<string, string[]>();
  const push = (parent: string, child: ResourceItem) => {
    const list = children.get(parent);
    if (list) list.push(child);
    else children.set(parent, [child]);
  };
  for (const item of model.items) {
    const resolved = item.managed ? [] : item.parentIds.filter((id) => id !== item.id && model.byId.has(id));
    const unique = [...new Set(resolved)];
    if (unique.length === 0) {
      push(APP_NODE_ID, item);
      primaryParent.set(item.id, APP_NODE_ID);
      continue;
    }
    parents.set(item.id, unique);
    primaryParent.set(item.id, unique[0] ?? APP_NODE_ID);
    push(unique[0] ?? APP_NODE_ID, item);
  }
  return { children, primaryParent, parents };
}

/** Ids of matching items and every layout ancestor of a match. */
function visibleUnderFilter(model: ResourceModel, structure: TreeStructure, matcher: (item: ResourceItem) => boolean) {
  const matched = new Set<string>();
  const visible = new Set<string>();
  for (const item of model.items) {
    if (!matcher(item)) continue;
    matched.add(item.id);
    let id: string | undefined = item.id;
    const guard = new Set<string>();
    while (id && id !== APP_NODE_ID && !visible.has(id) && !guard.has(id)) {
      guard.add(id);
      visible.add(id);
      id = structure.primaryParent.get(id);
    }
  }
  return { matched, visible };
}

function healthOrder(status: string | undefined): number {
  switch (status) {
    case 'Degraded':
      return 0;
    case 'Missing':
      return 1;
    case 'Progressing':
      return 2;
    case 'Suspended':
      return 3;
    case 'Healthy':
      return 5;
    default:
      return 4;
  }
}

/** Worst health among group members, for the group node's glyph. */
export function worstHealth(items: readonly ResourceItem[]): string | undefined {
  let worst: string | undefined;
  for (const item of items) {
    const status = item.health?.status;
    if (status && (worst === undefined || healthOrder(status) < healthOrder(worst))) worst = status;
  }
  return worst;
}

/** Builds the graph drawn by the tree view, applying filters, collapsing, grouping and paging. */
export function buildTreeGraph(app: Application, model: ResourceModel, options: TreeGraphOptions): Graph {
  const childLimit = options.childLimit ?? 25;
  const groupThreshold = options.groupThreshold ?? 5;
  const structure = treeStructure(model);
  const filter = options.filtering ? visibleUnderFilter(model, structure, options.matcher) : null;
  const isVisible = (item: ResourceItem) => (filter ? filter.visible.has(item.id) : options.matcher(item));

  const nodes: GraphNode[] = [];
  const edges: GraphEdge[] = [];
  const emitted = new Set<string>();

  const visibleChildren = (id: string) => (structure.children.get(id) ?? []).filter(isVisible).sort(compareItems);

  const countDescendants = (id: string, seen: Set<string>): number => {
    let total = 0;
    for (const child of visibleChildren(id)) {
      if (seen.has(child.id)) continue;
      seen.add(child.id);
      total += 1 + countDescendants(child.id, seen);
    }
    return total;
  };

  const appNode: GraphNode = {
    id: APP_NODE_ID,
    type: 'app',
    label: app.metadata.name ?? '',
    kind: 'Application',
    health: app.status?.health?.status,
    sync: app.status?.sync?.status,
    subtitle: app.metadata.namespace,
  };
  nodes.push(appNode);

  // Iterative DFS keeps deep or wide trees off the call stack.
  const stack: string[] = [APP_NODE_ID];
  const nodeById = new Map<string, GraphNode>([[APP_NODE_ID, appNode]]);
  while (stack.length > 0) {
    const parentId = stack.pop()!;
    const parentNode = nodeById.get(parentId);
    const kids = visibleChildren(parentId).filter((k) => !emitted.has(k.id));
    if (!parentNode || kids.length === 0) continue;
    if (parentId !== APP_NODE_ID && options.collapsed.has(parentId)) {
      parentNode.expanded = false;
      parentNode.collapsedCount = countDescendants(parentId, new Set([parentId]));
      continue;
    }
    parentNode.expanded = true;

    type Entry = { kind: 'item'; item: ResourceItem } | { kind: 'group'; key: string; groupKind: string; members: ResourceItem[] };
    let entries: Entry[] = kids.map((item) => ({ kind: 'item', item }));
    if (options.groupSimilar) {
      const leavesByKind = new Map<string, ResourceItem[]>();
      for (const k of kids) {
        if (visibleChildren(k.id).length > 0) continue;
        const list = leavesByKind.get(k.kind);
        if (list) list.push(k);
        else leavesByKind.set(k.kind, [k]);
      }
      const grouped = new Map<string, Entry>();
      for (const [kind, members] of leavesByKind) {
        const key = groupKey(parentId, kind);
        if (members.length >= groupThreshold && !options.expandedGroups.has(key)) {
          grouped.set(kind, { kind: 'group', key, groupKind: kind, members });
        }
      }
      if (grouped.size > 0) {
        const memberIds = new Set<string>();
        for (const group of grouped.values()) if (group.kind === 'group') for (const m of group.members) memberIds.add(m.id);
        const placed = new Set<string>();
        const next: Entry[] = [];
        for (const entry of entries) {
          if (entry.kind !== 'item') continue;
          const group = grouped.get(entry.item.kind);
          if (!group || !memberIds.has(entry.item.id)) next.push(entry);
          else if (!placed.has(entry.item.kind)) {
            placed.add(entry.item.kind);
            next.push(group);
          }
        }
        entries = next;
      }
    }

    let hidden = 0;
    if (entries.length > childLimit && !options.expandedParents.has(parentId)) {
      hidden = entries.slice(childLimit).reduce((sum, e) => sum + (e.kind === 'group' ? e.members.length : 1), 0);
      entries = entries.slice(0, childLimit);
    }

    const pushLater: string[] = [];
    for (const entry of entries) {
      if (entry.kind === 'group') {
        const id = `group:${entry.key}`;
        const node: GraphNode = {
          id,
          type: 'group',
          label: `${entry.members.length} ${entry.groupKind} resources`,
          kind: entry.groupKind,
          members: entry.members,
          expandKey: entry.key,
          health: worstHealth(entry.members),
          dimmed: filter ? entry.members.every((m) => !filter.matched.has(m.id)) : undefined,
        };
        for (const m of entry.members) emitted.add(m.id);
        nodes.push(node);
        nodeById.set(id, node);
        edges.push({ from: parentId, to: id });
        continue;
      }
      const item = entry.item;
      emitted.add(item.id);
      const node: GraphNode = {
        id: item.id,
        type: 'resource',
        label: item.name,
        kind: item.kind,
        item,
        health: item.health?.status,
        sync: item.sync,
        subtitle: item.namespace || undefined,
        dimmed: filter ? !filter.matched.has(item.id) : undefined,
      };
      nodes.push(node);
      nodeById.set(item.id, node);
      edges.push({ from: parentId, to: item.id, dashed: item.orphaned && parentId === APP_NODE_ID });
      pushLater.push(item.id);
    }
    if (hidden > 0) {
      const id = `more:${parentId}`;
      const node: GraphNode = { id, type: 'more', label: `Show ${hidden} more`, kind: '', expandKey: parentId, hiddenCount: hidden };
      nodes.push(node);
      nodeById.set(id, node);
      edges.push({ from: parentId, to: id });
    }
    // Reverse so the first child is visited first (stack order).
    for (let i = pushLater.length - 1; i >= 0; i -= 1) stack.push(pushLater[i]!);
  }

  // Secondary parent links between nodes that are both drawn.
  for (const [childId, parentIds] of structure.parents) {
    if (!nodeById.has(childId)) continue;
    for (const parentId of parentIds.slice(1)) {
      if (nodeById.has(parentId)) edges.push({ from: parentId, to: childId });
    }
  }
  return { nodes, edges, roots: [APP_NODE_ID] };
}

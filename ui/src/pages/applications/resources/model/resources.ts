import type { Application, ApplicationTree, ResourceIdentifier, ResourceNode, ResourceRef } from '../../../../api/types';

/*
 * One merged record per resource: the live node from the resource tree plus the managed
 * resource status from Application.status.resources (sync status, hook, pruning, wave).
 * Managed resources that have no live object (health Missing) still get a record, so the
 * views show what is desired but absent.
 */

type ManagedStatus = NonNullable<NonNullable<Application['status']>['resources']>[number];
export type InfoItem = NonNullable<ResourceNode['info']>[number];
export type NetworkingInfo = NonNullable<ResourceNode['networkingInfo']>;

export interface ResourceItem {
  /** "group/kind/namespace/name": the deep-link and filter key. */
  key: string;
  /** uid when known, otherwise the key: what parentRefs resolve against. */
  id: string;
  group: string;
  version: string;
  kind: string;
  namespace: string;
  name: string;
  uid: string | undefined;
  /** Resolved parent ids (uid or key), in parentRefs order. */
  parentIds: string[];
  health: { status?: string; message?: string } | undefined;
  /** Sync status for managed resources (Synced, OutOfSync, Unknown). */
  sync: string | undefined;
  hook: boolean;
  requiresPruning: boolean;
  syncWave: number | undefined;
  /** Declared in the application's source (present in status.resources). */
  managed: boolean;
  /** Listed in tree.orphanedNodes. */
  orphaned: boolean;
  /** Managed but no live object exists. */
  missing: boolean;
  createdAt: string | undefined;
  info: InfoItem[];
  images: string[];
  networkingInfo: NetworkingInfo | undefined;
  resourceVersion: string | undefined;
}

/** The string, or undefined when it is empty (for `??` fallbacks on optional API strings). */
export function nonEmpty(value: string | null | undefined): string | undefined {
  return value === undefined || value === null || value === '' ? undefined : value;
}

export function resourceKey(r: { group?: string | undefined; kind?: string | undefined; namespace?: string | undefined; name?: string | undefined }): string {
  return [r.group ?? '', r.kind ?? '', r.namespace ?? '', r.name ?? ''].join('/');
}

/** Parses a "group/kind/namespace/name" key (group and namespace may be empty). */
export function parseResourceKey(key: string): { group: string; kind: string; namespace: string; name: string } | null {
  const parts = key.split('/');
  if (parts.length < 4) return null;
  const [group = '', kind = '', namespace = '', ...rest] = parts;
  const name = rest.join('/');
  if (!kind || !name) return null;
  return { group, kind, namespace, name };
}

function refId(ref: ResourceRef): string {
  return nonEmpty(ref.uid) ?? resourceKey(ref);
}

export function identifierOf(item: Pick<ResourceItem, 'group' | 'version' | 'kind' | 'namespace' | 'name'>): ResourceIdentifier {
  return {
    group: item.group || undefined,
    version: item.version || undefined,
    kind: item.kind,
    namespace: item.namespace || undefined,
    name: item.name,
  };
}

function fromNode(node: ResourceNode, status: ManagedStatus | undefined, orphaned: boolean): ResourceItem {
  const key = resourceKey(node);
  const health = status?.health ?? node.health;
  return {
    key,
    id: nonEmpty(node.uid) ?? key,
    group: node.group ?? '',
    version: node.version ?? status?.version ?? '',
    kind: node.kind ?? '',
    namespace: node.namespace ?? '',
    name: node.name ?? '',
    uid: nonEmpty(node.uid),
    parentIds: (node.parentRefs ?? []).map(refId),
    health: health && (health.status || health.message) ? { status: health.status, message: health.message } : undefined,
    sync: status?.status,
    hook: status?.hook ?? false,
    requiresPruning: status?.requiresPruning ?? false,
    syncWave: status?.syncWave,
    managed: status !== undefined,
    orphaned,
    missing: false,
    createdAt: node.createdAt,
    info: node.info ?? [],
    images: node.images ?? [],
    networkingInfo: node.networkingInfo,
    resourceVersion: node.resourceVersion,
  };
}

function fromStatus(status: ManagedStatus): ResourceItem {
  const key = resourceKey(status);
  return {
    key,
    id: key,
    group: status.group ?? '',
    version: status.version ?? '',
    kind: status.kind ?? '',
    namespace: status.namespace ?? '',
    name: status.name ?? '',
    uid: undefined,
    parentIds: [],
    health: { status: status.health?.status ?? 'Missing', message: status.health?.message },
    sync: status.status,
    hook: status.hook ?? false,
    requiresPruning: status.requiresPruning ?? false,
    syncWave: status.syncWave,
    managed: true,
    orphaned: false,
    missing: true,
    createdAt: undefined,
    info: [],
    images: [],
    networkingInfo: undefined,
    resourceVersion: undefined,
  };
}

export interface ResourceModel {
  items: ResourceItem[];
  byId: ReadonlyMap<string, ResourceItem>;
  byKey: ReadonlyMap<string, ResourceItem>;
  /** Count of orphaned nodes in the tree, whether or not they are included. */
  orphanedCount: number;
}

/**
 * Merges the tree with the application's managed resource status. Orphaned nodes are included
 * only when `includeOrphaned` is set.
 */
export function buildResourceModel(app: Application, tree: ApplicationTree | undefined, includeOrphaned: boolean): ResourceModel {
  const statusByKey = new Map<string, ManagedStatus>();
  for (const res of app.status?.resources ?? []) statusByKey.set(resourceKey(res), res);

  const items: ResourceItem[] = [];
  const byId = new Map<string, ResourceItem>();
  const byKey = new Map<string, ResourceItem>();
  const add = (item: ResourceItem) => {
    if (byKey.has(item.key)) return;
    items.push(item);
    byKey.set(item.key, item);
    byId.set(item.id, item);
  };
  for (const node of tree?.nodes ?? []) add(fromNode(node, statusByKey.get(resourceKey(node)), false));
  if (includeOrphaned) {
    for (const node of tree?.orphanedNodes ?? []) add(fromNode(node, statusByKey.get(resourceKey(node)), true));
  }
  for (const [key, status] of statusByKey) {
    if (!byKey.has(key)) add(fromStatus(status));
  }
  return { items, byId, byKey, orphanedCount: tree?.orphanedNodes?.length ?? 0 };
}

/** Info item value by name (e.g. "Restart Count", "Containers", "Node", "Status Reason"). */
export function infoValue(item: Pick<ResourceItem, 'info'>, name: string): string | undefined {
  return item.info.find((i) => i.name === name)?.value;
}

/**
 * Owner chain of a resource (closest parent first), following the first resolvable parent at
 * each step. Stops on cycles.
 */
export function ownerChain(item: ResourceItem, byId: ReadonlyMap<string, ResourceItem>): ResourceItem[] {
  const chain: ResourceItem[] = [];
  const seen = new Set<string>([item.id]);
  let current: ResourceItem | undefined = item;
  while (current) {
    const parent: ResourceItem | undefined = current.parentIds.map((id) => byId.get(id)).find((p) => p !== undefined);
    if (!parent || seen.has(parent.id)) break;
    seen.add(parent.id);
    chain.push(parent);
    current = parent;
  }
  return chain;
}

/** Natural, case-insensitive comparison used for stable ordering everywhere. */
export const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

export function compareItems(a: ResourceItem, b: ResourceItem): number {
  return Number(a.orphaned) - Number(b.orphaned) || collator.compare(a.kind, b.kind) || collator.compare(a.namespace, b.namespace) || collator.compare(a.name, b.name);
}

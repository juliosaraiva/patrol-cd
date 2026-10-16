import type { ApplicationTree } from '../../../../api/types';
import { collator, compareItems, infoValue, ownerChain, type ResourceItem, type ResourceModel } from './resources';

export type PodGroupMode = 'node' | 'parentResource' | 'topLevelResource';
export const POD_GROUP_MODES: readonly PodGroupMode[] = ['node', 'parentResource', 'topLevelResource'];

export type HostInfo = NonNullable<ApplicationTree['hosts']>[number];

export interface PodGroup {
  id: string;
  /** Node name, or "Kind/name" of the parent or top-level resource. */
  title: string;
  /** "node" for hosts, otherwise the owner's kind ('' for the ungrouped bucket). */
  kind: string;
  owner?: ResourceItem | undefined;
  host?: HostInfo | undefined;
  pods: ResourceItem[];
}

export function isPodGroupMode(value: string | null | undefined): value is PodGroupMode {
  return value === 'node' || value === 'parentResource' || value === 'topLevelResource';
}

/** Pods grouped by node (with host resource usage), by direct parent or by top-level resource. */
export function groupPods(model: ResourceModel, hosts: readonly HostInfo[], mode: PodGroupMode, matcher: (item: ResourceItem) => boolean): PodGroup[] {
  const pods = model.items.filter((i) => i.kind === 'Pod' && matcher(i)).sort(compareItems);
  const groups = new Map<string, PodGroup>();
  const ensure = (id: string, make: () => PodGroup) => {
    let group = groups.get(id);
    if (!group) {
      group = make();
      groups.set(id, group);
    }
    return group;
  };

  if (mode === 'node') {
    for (const host of hosts) {
      if (host.name) ensure(`node:${host.name}`, () => ({ id: `node:${host.name ?? ''}`, title: host.name ?? '', kind: 'node', host, pods: [] }));
    }
    for (const pod of pods) {
      const node = infoValue(pod, 'Node');
      if (node) {
        ensure(`node:${node}`, () => ({ id: `node:${node}`, title: node, kind: 'node', pods: [] })).pods.push(pod);
      } else {
        ensure('node:', () => ({ id: 'node:', title: 'Not scheduled', kind: '', pods: [] })).pods.push(pod);
      }
    }
  } else {
    for (const pod of pods) {
      const chain = ownerChain(pod, model.byId);
      const owner = mode === 'parentResource' ? chain[0] : chain[chain.length - 1];
      if (owner) {
        ensure(`owner:${owner.key}`, () => ({ id: `owner:${owner.key}`, title: `${owner.kind}/${owner.name}`, kind: owner.kind, owner, pods: [] })).pods.push(pod);
      } else {
        ensure('owner:', () => ({ id: 'owner:', title: 'No owner', kind: '', pods: [] })).pods.push(pod);
      }
    }
  }
  return [...groups.values()].sort((a, b) => Number(a.id.endsWith(':')) - Number(b.id.endsWith(':')) || collator.compare(a.title, b.title));
}

/** Formats a host resource quantity: CPU arrives in millicores, memory in bytes. */
export function formatQuantity(resourceName: string | undefined, value: number | undefined): string {
  if (value === undefined) return '—';
  if (resourceName === 'cpu') {
    if (value < 1000) return `${value}m`;
    return `${(value / 1000).toFixed(value % 1000 === 0 ? 0 : 2)} cores`;
  }
  if (resourceName === 'memory' || resourceName === 'ephemeral-storage') {
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    let v = value;
    let u = 0;
    while (v >= 1024 && u < units.length - 1) {
      v /= 1024;
      u += 1;
    }
    return `${v >= 10 || u === 0 ? Math.round(v) : v.toFixed(1)} ${units[u] ?? 'B'}`;
  }
  return String(value);
}

/** Pod facts from the tree's info items (as reported by the controller). */
export function podFacts(pod: ResourceItem): { phase: string | undefined; ready: string | undefined; restarts: number | undefined; node: string | undefined } {
  const restarts = infoValue(pod, 'Restart Count');
  const parsed = restarts === undefined ? undefined : Number.parseInt(restarts, 10);
  return {
    phase: infoValue(pod, 'Status Reason'),
    ready: infoValue(pod, 'Containers'),
    restarts: parsed === undefined || Number.isNaN(parsed) ? undefined : parsed,
    node: infoValue(pod, 'Node'),
  };
}

import type { Graph, GraphEdge, GraphNode } from '../graph/types';
import { compareItems, nonEmpty, resourceKey, type ResourceItem, type ResourceModel } from './resources';

export const EXTERNAL_TRAFFIC_ID = '__external__';
export const INTERNAL_TRAFFIC_ID = '__internal__';

export interface ExternalEndpoint {
  item: ResourceItem;
  /** Load balancer IPs and hostnames. */
  addresses: string[];
  /** URLs reported by the resource (e.g. Ingress rules). */
  urls: string[];
}

export interface NetworkGraph extends Graph {
  endpoints: ExternalEndpoint[];
}

function addresses(item: ResourceItem): string[] {
  return (item.networkingInfo?.ingress ?? []).map((i) => nonEmpty(i.hostname) ?? nonEmpty(i.ip) ?? '').filter(Boolean);
}

function labelsMatch(selector: Record<string, string>, labels: Record<string, string> | undefined): boolean {
  const entries = Object.entries(selector);
  if (entries.length === 0 || !labels) return false;
  return entries.every(([k, v]) => labels[k] === v);
}

/**
 * Networking graph from the tree's networkingInfo: external traffic → routers (resources with
 * targetRefs, e.g. Ingress or HTTPRoute) → Services (targetLabels) → Pods (matching labels).
 * Resources with load balancer addresses or external URLs hang off "External traffic"; the
 * remaining entry points off "Internal traffic". Resources unrelated to networking are omitted.
 */
export function buildNetworkGraph(model: ResourceModel, matcher: (item: ResourceItem) => boolean): NetworkGraph {
  const items = model.items.filter(matcher);
  const byKey = new Map(items.map((i) => [i.key, i]));
  const edges: GraphEdge[] = [];
  const involved = new Set<string>();
  const targeted = new Set<string>();
  const link = (from: ResourceItem, to: ResourceItem) => {
    edges.push({ from: from.id, to: to.id });
    involved.add(from.id);
    involved.add(to.id);
    targeted.add(to.id);
  };

  const pods = items.filter((i) => i.kind === 'Pod').sort(compareItems);
  const sorted = [...items].sort(compareItems);
  for (const item of sorted) {
    const info = item.networkingInfo;
    if (!info) continue;
    for (const ref of info.targetRefs ?? []) {
      const target = byKey.get(resourceKey({ group: ref.group ?? '', kind: ref.kind, namespace: ref.namespace ?? item.namespace, name: ref.name }));
      if (target && target.id !== item.id) link(item, target);
    }
    if (info.targetLabels && Object.keys(info.targetLabels).length > 0) {
      involved.add(item.id);
      for (const pod of pods) {
        if (pod.namespace === item.namespace && labelsMatch(info.targetLabels, pod.networkingInfo?.labels)) link(item, pod);
      }
    }
    if (addresses(item).length > 0 || (info.externalURLs ?? []).length > 0) involved.add(item.id);
    if (item.kind === 'Service' || item.kind === 'Ingress') involved.add(item.id);
  }

  const nodes: GraphNode[] = [];
  const endpoints: ExternalEndpoint[] = [];
  const externalRoots: string[] = [];
  const internalRoots: string[] = [];
  for (const item of sorted) {
    if (!involved.has(item.id)) continue;
    const addr = addresses(item);
    const urls = item.networkingInfo?.externalURLs ?? [];
    if (addr.length > 0 || urls.length > 0) {
      endpoints.push({ item, addresses: addr, urls });
      externalRoots.push(item.id);
    } else if (!targeted.has(item.id)) {
      internalRoots.push(item.id);
    }
    const subtitle = addr.length > 0 ? (addr.length > 1 ? `${addr[0] ?? ''} +${addr.length - 1}` : addr[0]) : item.namespace || undefined;
    nodes.push({
      id: item.id,
      type: 'resource',
      label: item.name,
      kind: item.kind,
      item,
      health: item.health?.status,
      sync: item.sync,
      subtitle,
    });
  }

  const roots: string[] = [];
  if (externalRoots.length > 0) {
    nodes.unshift({ id: EXTERNAL_TRAFFIC_ID, type: 'traffic', label: 'External traffic', kind: '' });
    roots.push(EXTERNAL_TRAFFIC_ID);
    edges.unshift(...externalRoots.map((id) => ({ from: EXTERNAL_TRAFFIC_ID, to: id })));
  }
  if (internalRoots.length > 0) {
    nodes.unshift({ id: INTERNAL_TRAFFIC_ID, type: 'traffic', label: 'Internal traffic', kind: '' });
    roots.push(INTERNAL_TRAFFIC_ID);
    edges.push(...internalRoots.map((id) => ({ from: INTERNAL_TRAFFIC_ID, to: id })));
  }
  return { nodes, edges, roots, endpoints };
}

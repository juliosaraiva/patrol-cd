import type { ClusterInput, RegisteredCluster } from '../../../api/types';
import { nonEmpty, rowsToMap, type Row } from '../common/editors';

export const IN_CLUSTER_SERVER = 'https://kubernetes.default.svc';

/** Route to a cluster's details page (the server URL is one encoded path segment). */
export function clusterPath(server: string): string {
  return `/settings/clusters/${encodeURIComponent(server)}`;
}

/**
 * How Patrol authenticates to the cluster, from RegisteredCluster.credentials (flags only; the
 * API never returns credential values, and removes execProviderConfig from the spec).
 */
export function credentialsType(cluster: RegisteredCluster): string {
  const flags = cluster.credentials;
  const stored = [
    flags.hasBearerToken ? 'bearer token' : null,
    flags.hasClientCertificate ? 'client certificate' : null,
    flags.hasBasicAuth ? 'username and password' : null,
  ].filter((v): v is string => v !== null);
  if (flags.hasExecProviderConfig) return 'Exec credential plugin';
  if (cluster.spec.config?.awsAuthConfig) return 'AWS IAM';
  if (stored.length > 0) {
    const text = stored.join(', ');
    return `${text.charAt(0).toUpperCase()}${text.slice(1)}${flags.hasCAData ? ' (custom CA)' : ''}`;
  }
  if (isInCluster(cluster)) return 'In-cluster service account';
  return 'None';
}

/** The control plane's own cluster (the implicit in-cluster destination or its server URL). */
export function isInCluster(cluster: RegisteredCluster): boolean {
  return cluster.inCluster === true || cluster.spec.server === IN_CLUSTER_SERVER;
}

export function namespacesSummary(cluster: RegisteredCluster): string {
  const ns = cluster.spec.namespaces ?? [];
  if (ns.length === 0) return 'All namespaces';
  return ns.length <= 3 ? ns.join(', ') : `${ns.slice(0, 3).join(', ')} and ${ns.length - 3} more`;
}

function sameList(a: readonly string[] | undefined, b: readonly string[] | undefined): boolean {
  const x = a ?? [];
  const y = b ?? [];
  return x.length === y.length && x.every((v, i) => v === y[i]);
}

function sameMap(a: Record<string, string> | undefined, b: Record<string, string> | undefined): boolean {
  const x = Object.entries(a ?? {}).sort();
  const y = Object.entries(b ?? {}).sort();
  return JSON.stringify(x) === JSON.stringify(y);
}

export interface ClusterEditState {
  name: string;
  namespaces: string[];
  clusterResources: boolean;
  project: string;
  labels: Row[];
  annotations: Row[];
}

/** Fields changed by the edit, named as the API's updatedFields expects (ClusterUpdatableFields). */
export function changedClusterFields(cluster: RegisteredCluster, edit: ClusterEditState): string[] {
  const fields: string[] = [];
  if (edit.name.trim() !== cluster.spec.name) fields.push('name');
  if (!sameList(nonEmpty(edit.namespaces), cluster.spec.namespaces)) fields.push('namespaces');
  if (edit.clusterResources !== (cluster.spec.clusterResources ?? false)) fields.push('clusterResources');
  if (edit.project !== (cluster.spec.project ?? '')) fields.push('project');
  if (!sameMap(rowsToMap(edit.labels), cluster.metadata.labels)) fields.push('labels');
  if (!sameMap(rowsToMap(edit.annotations), cluster.metadata.annotations)) fields.push('annotations');
  return fields;
}

/**
 * PUT body for an edit: the flat ClusterInput. With `updatedFields` the server changes only
 * those fields, so `config` (write-only credentials) is never sent and stays as stored.
 */
export function clusterUpdateInput(cluster: RegisteredCluster, edit: ClusterEditState): ClusterInput {
  return {
    server: cluster.spec.server,
    name: edit.name.trim(),
    namespaces: nonEmpty(edit.namespaces),
    clusterResources: edit.clusterResources,
    ...(edit.project ? { project: edit.project } : {}),
    labels: rowsToMap(edit.labels),
    annotations: rowsToMap(edit.annotations),
    ...(cluster.metadata.resourceVersion ? { resourceVersion: cluster.metadata.resourceVersion } : {}),
  };
}

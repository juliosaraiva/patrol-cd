/*
 * Kind captions and icon resolution for resource nodes.
 *
 * Captions follow the baseline abbreviation map (parity API-130): known kinds get their
 * kubectl-style short name, anything else is lowercased. Icons are Patrol's own drawings
 * (shared/KindIcon.tsx); resolution order mirrors API-127 for the parts Patrol bundles:
 * special kinds first, then the built-in kind map, then initials.
 */

const ABBREVIATIONS: ReadonlyMap<string, string> = new Map([
  ['ClusterRole', 'c-role'],
  ['ConfigMap', 'cm'],
  ['ClusterRoleBinding', 'crb'],
  ['CustomResourceDefinition', 'crd'],
  ['CronJob', 'cronjob'],
  ['Deployment', 'deploy'],
  ['DaemonSet', 'ds'],
  ['Endpoint', 'ep'],
  ['Endpoints', 'ep'],
  ['Group', 'group'],
  ['HorizontalPodAutoscaler', 'hpa'],
  ['Ingress', 'ing'],
  ['Job', 'job'],
  ['LimitRange', 'limits'],
  ['NetworkPolicy', 'netpol'],
  ['Namespace', 'ns'],
  ['Pod', 'pod'],
  ['PodSecurityPolicy', 'psp'],
  ['PersistentVolume', 'pv'],
  ['PersistentVolumeClaim', 'pvc'],
  ['RoleBinding', 'rb'],
  ['Role', 'role'],
  ['ReplicaSet', 'rs'],
  ['ServiceAccount', 'sa'],
  ['StorageClass', 'sc'],
  ['Secret', 'secret'],
  ['StatefulSet', 'sts'],
  ['Service', 'svc'],
  ['User', 'user'],
  ['Volume', 'vol'],
  ['ComponentStatus', 'cs'],
  ['CertificateSigningRequest', 'csr'],
  ['Event', 'ev'],
  ['Node', 'no'],
  ['PodDisruptionBudget', 'pdb'],
  ['PriorityClass', 'pc'],
  ['ReplicationController', 'rc'],
  ['ResourceQuota', 'quota'],
]);

/** Short caption printed under a resource icon ("Deployment" → "deploy", "Certificate" → "certificate"). */
export function kindCaption(kind: string | undefined): string {
  if (!kind) return '';
  return ABBREVIATIONS.get(kind) ?? kind.toLowerCase();
}

/** All known abbreviations, e.g. for matching a kind filter typed as "deploy". */
export function kindForAbbreviation(text: string): string | undefined {
  const needle = text.trim().toLowerCase();
  for (const [kind, short] of ABBREVIATIONS) if (short === needle) return kind;
  return undefined;
}

/** Initials fallback: the kind with ASCII lowercase letters removed ("MyCustomKind" → "MCK"). */
export function kindInitials(kind: string | undefined): string {
  return (kind ?? '').replace(/[a-z]/g, '');
}

/**
 * Icons Patrol draws, keyed by kind (case-sensitive, group-agnostic like the baseline kind
 * map). `special` icons (node host, Application, ApplicationSet) win over everything.
 */
export type KindIconId =
  | 'node'
  | 'application'
  | 'applicationset'
  | 'pod'
  | 'deployment'
  | 'replicaset'
  | 'statefulset'
  | 'daemonset'
  | 'job'
  | 'cronjob'
  | 'service'
  | 'ingress'
  | 'endpoints'
  | 'configmap'
  | 'secret'
  | 'pvc'
  | 'pv'
  | 'storageclass'
  | 'serviceaccount'
  | 'role'
  | 'rolebinding'
  | 'namespace'
  | 'hpa'
  | 'networkpolicy'
  | 'crd'
  | 'limitrange'
  | 'quota'
  | 'psp'
  | 'user'
  | 'group'
  | 'volume';

const KIND_ICONS: ReadonlyMap<string, KindIconId> = new Map([
  ['Pod', 'pod'],
  ['Deployment', 'deployment'],
  ['ReplicaSet', 'replicaset'],
  ['StatefulSet', 'statefulset'],
  ['DaemonSet', 'daemonset'],
  ['Job', 'job'],
  ['CronJob', 'cronjob'],
  ['Service', 'service'],
  ['Ingress', 'ingress'],
  ['Endpoint', 'endpoints'],
  ['Endpoints', 'endpoints'],
  ['EndpointSlice', 'endpoints'],
  ['ConfigMap', 'configmap'],
  ['Secret', 'secret'],
  ['PersistentVolumeClaim', 'pvc'],
  ['PersistentVolume', 'pv'],
  ['StorageClass', 'storageclass'],
  ['ServiceAccount', 'serviceaccount'],
  ['Role', 'role'],
  ['ClusterRole', 'role'],
  ['RoleBinding', 'rolebinding'],
  ['ClusterRoleBinding', 'rolebinding'],
  ['Namespace', 'namespace'],
  ['HorizontalPodAutoscaler', 'hpa'],
  ['NetworkPolicy', 'networkpolicy'],
  ['CustomResourceDefinition', 'crd'],
  ['LimitRange', 'limitrange'],
  // The baseline keys its quota icon "Quote" (so ResourceQuota shows initials); kept as is.
  ['Quote', 'quota'],
  ['PodSecurityPolicy', 'psp'],
  ['User', 'user'],
  ['Group', 'group'],
  ['Volume', 'volume'],
]);

/** Icon for a kind, or undefined when the initials fallback applies. */
export function resolveKindIcon(kind: string | undefined): KindIconId | undefined {
  if (!kind) return undefined;
  if (kind === 'node') return 'node';
  if (kind === 'Application') return 'application';
  if (kind === 'ApplicationSet') return 'applicationset';
  return KIND_ICONS.get(kind);
}

/** Workload kinds whose pods' logs can be aggregated (plus Pod itself). */
const LOG_KINDS = new Set(['Pod', 'Deployment', 'StatefulSet', 'DaemonSet', 'ReplicaSet', 'Job', 'CronJob', 'Rollout', 'ReplicationController']);

export function kindHasLogs(kind: string): boolean {
  return LOG_KINDS.has(kind);
}

/** Static suggestion lists for pickers (resource kinds, time zones). Free text stays allowed. */

export interface GroupKind {
  group: string;
  kind: string;
  namespaced: boolean;
}

/** Common built-in kinds offered as suggestions in the project resource editors. */
export const COMMON_RESOURCES: readonly GroupKind[] = [
  { group: '', kind: 'ConfigMap', namespaced: true },
  { group: '', kind: 'Secret', namespaced: true },
  { group: '', kind: 'Service', namespaced: true },
  { group: '', kind: 'ServiceAccount', namespaced: true },
  { group: '', kind: 'PersistentVolumeClaim', namespaced: true },
  { group: '', kind: 'Pod', namespaced: true },
  { group: '', kind: 'ResourceQuota', namespaced: true },
  { group: '', kind: 'LimitRange', namespaced: true },
  { group: '', kind: 'Namespace', namespaced: false },
  { group: '', kind: 'PersistentVolume', namespaced: false },
  { group: '', kind: 'Node', namespaced: false },
  { group: 'apps', kind: 'Deployment', namespaced: true },
  { group: 'apps', kind: 'StatefulSet', namespaced: true },
  { group: 'apps', kind: 'DaemonSet', namespaced: true },
  { group: 'apps', kind: 'ReplicaSet', namespaced: true },
  { group: 'batch', kind: 'Job', namespaced: true },
  { group: 'batch', kind: 'CronJob', namespaced: true },
  { group: 'networking.k8s.io', kind: 'Ingress', namespaced: true },
  { group: 'networking.k8s.io', kind: 'NetworkPolicy', namespaced: true },
  { group: 'networking.k8s.io', kind: 'IngressClass', namespaced: false },
  { group: 'policy', kind: 'PodDisruptionBudget', namespaced: true },
  { group: 'autoscaling', kind: 'HorizontalPodAutoscaler', namespaced: true },
  { group: 'rbac.authorization.k8s.io', kind: 'Role', namespaced: true },
  { group: 'rbac.authorization.k8s.io', kind: 'RoleBinding', namespaced: true },
  { group: 'rbac.authorization.k8s.io', kind: 'ClusterRole', namespaced: false },
  { group: 'rbac.authorization.k8s.io', kind: 'ClusterRoleBinding', namespaced: false },
  { group: 'apiextensions.k8s.io', kind: 'CustomResourceDefinition', namespaced: false },
  { group: 'storage.k8s.io', kind: 'StorageClass', namespaced: false },
  { group: 'admissionregistration.k8s.io', kind: 'ValidatingWebhookConfiguration', namespaced: false },
  { group: 'admissionregistration.k8s.io', kind: 'MutatingWebhookConfiguration', namespaced: false },
  { group: 'scheduling.k8s.io', kind: 'PriorityClass', namespaced: false },
  { group: 'cd.patrol.example', kind: 'Application', namespaced: true },
  { group: 'cd.patrol.example', kind: 'ApplicationSet', namespaced: true },
];

export function groupSuggestions(namespaced: boolean | undefined): string[] {
  const groups = COMMON_RESOURCES.filter((r) => namespaced === undefined || r.namespaced === namespaced).map((r) => r.group);
  return ['*', ...new Set(groups.filter((g) => g !== ''))];
}

export function kindSuggestions(namespaced: boolean | undefined): string[] {
  return ['*', ...new Set(COMMON_RESOURCES.filter((r) => namespaced === undefined || r.namespaced === namespaced).map((r) => r.kind))];
}

const FALLBACK_ZONES = [
  'UTC',
  'Africa/Johannesburg',
  'America/Chicago',
  'America/Los_Angeles',
  'America/New_York',
  'America/Sao_Paulo',
  'Asia/Kolkata',
  'Asia/Shanghai',
  'Asia/Singapore',
  'Asia/Tokyo',
  'Australia/Sydney',
  'Europe/Berlin',
  'Europe/London',
  'Europe/Paris',
];

/** IANA time zones known to this browser, UTC first. */
export function timeZones(): string[] {
  try {
    const zones = Intl.supportedValuesOf('timeZone');
    return ['UTC', ...zones.filter((z) => z !== 'UTC')];
  } catch {
    return FALLBACK_ZONES;
  }
}

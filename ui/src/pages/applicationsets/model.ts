/**
 * Pure ApplicationSet helpers for the list and details pages (API-118, ASET-124..126): health,
 * conditions guidance, generator summaries, generated-application rows and URLs.
 */
import type { Application, ApplicationSet, HealthStatusCode } from '../../api/types';

export type AppSetCondition = NonNullable<NonNullable<ApplicationSet['status']>['conditions']>[number];
export type AppSetResource = NonNullable<NonNullable<ApplicationSet['status']>['resources']>[number];
export type AppSetApplicationStatus = NonNullable<NonNullable<ApplicationSet['status']>['applicationStatus']>[number];
export type Generator = ApplicationSet['spec']['generators'][number];

const HEALTH_CODES: readonly string[] = ['Healthy', 'Progressing', 'Degraded', 'Suspended', 'Missing', 'Unknown'];

/**
 * status.health when the controller reports it, else derived from conditions as upstream:
 * ErrorOccurred=True → Degraded, RolloutProgressing=True → Progressing,
 * ResourcesUpToDate=True → Healthy, otherwise Unknown.
 */
export function appSetHealth(set: ApplicationSet): HealthStatusCode {
  const reported = set.status?.health?.status;
  if (reported && HEALTH_CODES.includes(reported)) return reported as HealthStatusCode;
  const conditions = set.status?.conditions ?? [];
  if (conditions.length === 0) return 'Unknown';
  if (conditions.some((c) => c.type === 'ErrorOccurred' && c.status === 'True')) return 'Degraded';
  if (conditions.some((c) => c.type === 'RolloutProgressing' && c.status === 'True')) return 'Progressing';
  if (conditions.some((c) => c.type === 'ResourcesUpToDate' && c.status === 'True')) return 'Healthy';
  return 'Unknown';
}

/** A condition that needs attention: an error, or a success-type condition that is not True. */
export function isProblem(c: AppSetCondition): boolean {
  if (c.type === 'ErrorOccurred') return c.status === 'True';
  if (c.type === 'ParametersGenerated' || c.type === 'ResourcesUpToDate') return c.status === 'False';
  return false;
}

export function problemConditions(set: ApplicationSet): AppSetCondition[] {
  return (set.status?.conditions ?? []).filter(isProblem);
}

const REASON_GUIDANCE: Record<string, string> = {
  ApplicationGenerationFromParamsError:
    'A generator failed. Check repository access and credentials, SCM provider tokens, cluster selectors and the generator settings, then wait for the next reconciliation.',
  RenderTemplateParamsError:
    'The template could not be rendered with the generated parameters. Check template expressions (goTemplate syntax, missing keys) against the parameters each generator produces; use Preview to test a fix.',
  CreateApplicationError: 'The controller could not create a generated Application. Check that the template project exists and allows the destination and source.',
  UpdateApplicationError: 'The controller could not update a generated Application. Check its project permissions and for conflicting edits made outside the ApplicationSet.',
  DeleteApplicationError: 'A generated Application could not be deleted. Check its finalizers and the controller logs.',
  RefreshApplicationError: 'A generated Application could not be refreshed. It may have been deleted or the controller lacks permission.',
  ApplicationValidationError:
    'A generated Application failed validation. Check that the template project, destination cluster and namespace exist and are permitted by the project.',
  SyncApplicationError: 'A progressive sync step could not sync an Application. Open the Application to see the sync error.',
  ErrorOccurred: 'The controller reported an error. Read the message for details.',
};

/** What the user can do about a condition; null when nothing is needed. */
export function conditionGuidance(c: AppSetCondition): string | null {
  if (c.type === 'RolloutProgressing' && c.status === 'True') {
    return 'A RollingSync rollout is in progress: each step waits until its Applications are Healthy before the next one syncs.';
  }
  if (!isProblem(c)) return null;
  return REASON_GUIDANCE[c.reason] ?? REASON_GUIDANCE.ErrorOccurred ?? null;
}

/** Most recent condition transition (the "last updated" time of the status bar). */
export function lastUpdated(set: ApplicationSet): string | undefined {
  const times = (set.status?.conditions ?? []).map((c) => c.lastTransitionTime).filter(Boolean).sort();
  return times[times.length - 1] ?? set.metadata.creationTimestamp;
}

export const GENERATOR_LABELS: Record<string, string> = {
  list: 'List',
  clusters: 'Clusters',
  git: 'Git',
  scmProvider: 'SCM provider',
  pullRequest: 'Pull requests',
  clusterDecisionResource: 'Cluster decision resource',
  plugin: 'Plugin',
  matrix: 'Matrix',
  merge: 'Merge',
};

/** Generator type keys set on one generator entry (normally exactly one; "selector" excluded). */
export function generatorKinds(gen: Generator): string[] {
  return Object.keys(GENERATOR_LABELS).filter((k) => (gen as Record<string, unknown>)[k] !== undefined && (gen as Record<string, unknown>)[k] !== null);
}

/** "Matrix (Git, Clusters)"-style label for a generator, nested generators included. */
export function generatorLabel(gen: Generator): string {
  return generatorKinds(gen)
    .map((kind) => {
      const nested = (kind === 'matrix' ? gen.matrix?.generators : kind === 'merge' ? gen.merge?.generators : undefined) as Generator[] | undefined;
      const label = GENERATOR_LABELS[kind] ?? kind;
      return nested && nested.length > 0 ? `${label} (${nested.map(generatorLabel).join(', ')})` : label;
    })
    .join(', ');
}

export function generatorSummary(set: ApplicationSet): string {
  return set.spec.generators.map(generatorLabel).join(', ') || 'No generators';
}

/** Every generator, depth first, including those nested in matrix and merge generators. */
export function flattenGenerators(gens: readonly Generator[]): Generator[] {
  return gens.flatMap((g) => [g, ...flattenGenerators(((g.matrix?.generators ?? g.merge?.generators ?? []) as Generator[]))]);
}

export function hasPullRequestGenerator(set: ApplicationSet): boolean {
  return flattenGenerators(set.spec.generators).some((g) => g.pullRequest !== undefined);
}

/** Projects the template (and generator templates) deploy into. */
export function appSetProjects(set: ApplicationSet): string[] {
  const projects = new Set<string>();
  if (set.spec.template.spec.project) projects.add(set.spec.template.spec.project);
  for (const g of flattenGenerators(set.spec.generators)) {
    for (const kind of generatorKinds(g)) {
      const tpl = (g as Record<string, { template?: { spec?: { project?: string } } } | undefined>)[kind]?.template;
      if (tpl?.spec?.project) projects.add(tpl.spec.project);
    }
  }
  return [...projects];
}

export function formatSelector(selector: { matchLabels?: Record<string, string>; matchExpressions?: { key: string; operator: string; values?: string[] }[] } | undefined): string {
  if (!selector) return '';
  const parts = Object.entries(selector.matchLabels ?? {}).map(([k, v]) => `${k}=${v}`);
  for (const e of selector.matchExpressions ?? []) {
    const values = e.values && e.values.length > 0 ? ` (${e.values.join(', ')})` : '';
    parts.push(`${e.key} ${e.operator}${values}`);
  }
  return parts.join(', ');
}

/** Router path of an ApplicationSet (namespace segment only outside the control plane). */
export function appSetPath(set: { metadata: { name?: string; namespace?: string } }, controlPlaneNamespace: string | undefined): string {
  const name = encodeURIComponent(set.metadata.name ?? '');
  const ns = set.metadata.namespace;
  return ns && ns !== controlPlaneNamespace ? `/applicationsets/${encodeURIComponent(ns)}/${name}` : `/applicationsets/${name}`;
}

/** Router path of an Application (namespace segment only outside the control plane). */
export function applicationPath(name: string, namespace: string | undefined, controlPlaneNamespace: string | undefined): string {
  return namespace && namespace !== controlPlaneNamespace
    ? `/applications/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`
    : `/applications/${encodeURIComponent(name)}`;
}

export interface GeneratedAppRow {
  key: string;
  name: string;
  namespace: string;
  health: string;
  sync: string;
  /** Progressive sync (RollingSync) state, when a strategy is active. */
  step?: string | undefined;
  progressiveStatus?: string | undefined;
  progressiveMessage?: string | undefined;
  targetRevisions?: string[] | undefined;
  /** The live Application when the application list includes it. */
  app?: Application | undefined;
}

/**
 * Generated Applications: status.resources (what the controller manages), merged with live
 * Applications owned by the set (fresher health/sync) and status.applicationStatus (step).
 */
export function generatedAppRows(set: ApplicationSet, owned: readonly Application[]): GeneratedAppRow[] {
  const namespace = set.metadata.namespace ?? '';
  const live = new Map(owned.map((a) => [`${a.metadata.namespace ?? namespace}/${a.metadata.name ?? ''}`, a]));
  const progressive = new Map((set.status?.applicationStatus ?? []).map((s) => [s.application, s]));
  const rows = new Map<string, GeneratedAppRow>();
  const add = (name: string, ns: string, health: string | undefined, sync: string | undefined) => {
    const key = `${ns}/${name}`;
    const app = live.get(key);
    const p = progressive.get(name);
    rows.set(key, {
      key,
      name,
      namespace: ns,
      health: app?.status?.health?.status ?? health ?? 'Unknown',
      sync: app?.status?.sync?.status ?? sync ?? 'Unknown',
      step: p?.step,
      progressiveStatus: p?.status,
      progressiveMessage: p?.message,
      targetRevisions: p?.targetRevisions,
      app,
    });
  };
  for (const r of set.status?.resources ?? []) {
    if (r.kind !== 'Application') continue;
    add(r.name ?? '', r.namespace ?? namespace, r.health?.status, r.status);
  }
  for (const a of owned) {
    const ns = a.metadata.namespace ?? namespace;
    if (!rows.has(`${ns}/${a.metadata.name ?? ''}`)) add(a.metadata.name ?? '', ns, undefined, undefined);
  }
  return [...rows.values()].sort((a, b) => a.name.localeCompare(b.name));
}

export function appCount(set: ApplicationSet): number {
  return set.status?.resourcesCount ?? (set.status?.resources ?? []).filter((r) => r.kind === 'Application').length;
}

/** What deleting the set does to generated Applications and their resources. */
export function deletionImpact(set: ApplicationSet): { preserve: boolean; summary: string; order: string | null } {
  const preserve = set.spec.syncPolicy?.preserveResourcesOnDeletion === true;
  const summary = preserve
    ? 'preserveResourcesOnDeletion is true: the generated Applications are deleted without cascading, so the Kubernetes resources they deployed keep running in their clusters.'
    : 'preserveResourcesOnDeletion is not set: the generated Applications are deleted, and those with the resources finalizer also delete the Kubernetes resources they deployed.';
  const order =
    set.spec.strategy?.deletionOrder === 'Reverse'
      ? 'Deletion order is Reverse: Applications are removed step by step, starting with the last RollingSync step.'
      : null;
  return { preserve, summary, order };
}

export function syncPolicyText(set: ApplicationSet): string {
  const policy = set.spec.syncPolicy?.applicationsSync;
  switch (policy) {
    case 'create-only':
      return 'create-only: Applications are created, never updated or deleted';
    case 'create-update':
      return 'create-update: Applications are created and updated, never deleted';
    case 'create-delete':
      return 'create-delete: Applications are created and deleted, never updated';
    case 'sync':
      return 'sync: Applications are created, updated and deleted';
    default:
      return 'Default (sync): Applications are created, updated and deleted, unless the controller policy restricts it';
  }
}

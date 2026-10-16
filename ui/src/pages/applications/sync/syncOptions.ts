/**
 * Sync options (api/v1alpha1/sync_options.go) with UI labels and explanations, plus the pure
 * request builder for the sync panel (parity API-098, SYNC-*).
 */
import type { Application, ApplicationSyncRequest } from '../../../api/types';
import { appSources, isMultiSource, type RetryStrategy } from '../shared/app';

export interface BooleanSyncOption {
  kind: 'boolean';
  key: string;
  label: string;
  description: string;
  /** Checked state is `key=false` (e.g. Validate=false is "Skip schema validation"). */
  inverted?: boolean;
  /** Needs a typed confirmation before syncing. */
  destructive?: boolean;
}

export interface SelectSyncOption {
  kind: 'select';
  key: string;
  label: string;
  description: string;
  values: readonly string[];
  defaultValue: string;
}

export type SyncOptionDef = BooleanSyncOption | SelectSyncOption;

export const SYNC_OPTIONS: readonly SyncOptionDef[] = [
  { kind: 'boolean', key: 'Validate', inverted: true, label: 'Skip schema validation', description: 'Apply manifests without kubectl schema validation (Validate=false).' },
  { kind: 'boolean', key: 'CreateNamespace', label: 'Auto-create namespace', description: 'Create the destination namespace if it does not exist.' },
  { kind: 'boolean', key: 'ApplyOutOfSyncOnly', label: 'Apply out of sync only', description: 'Apply only resources that are out of sync instead of every resource.' },
  { kind: 'boolean', key: 'RespectIgnoreDifferences', label: 'Respect ignore differences', description: 'Keep fields listed in ignoreDifferences at their live values while syncing.' },
  { kind: 'boolean', key: 'ServerSideApply', label: 'Server-side apply', description: 'Use Kubernetes server-side apply (needed for very large resources and field ownership).' },
  { kind: 'boolean', key: 'PruneLast', label: 'Prune last', description: 'Delete pruned resources after every other wave has been applied and is healthy.' },
  { kind: 'boolean', key: 'FailOnSharedResource', label: 'Fail on shared resource', description: 'Fail the sync when a resource is already managed by another application.' },
  {
    kind: 'select',
    key: 'PrunePropagationPolicy',
    label: 'Prune propagation policy',
    description: 'How dependents of pruned resources are deleted: foreground waits for them, background deletes them asynchronously, orphan leaves them.',
    values: ['foreground', 'background', 'orphan'],
    defaultValue: 'foreground',
  },
  {
    kind: 'select',
    key: 'Prune',
    label: 'Prune',
    description: 'Whether resources may be pruned. "confirm" holds pruning until someone approves it with Confirm pruning.',
    values: ['true', 'false', 'confirm'],
    defaultValue: 'true',
  },
  {
    kind: 'select',
    key: 'Delete',
    label: 'Delete',
    description: 'Whether resources may be deleted when the application is deleted. "confirm" requires an approval.',
    values: ['true', 'false', 'confirm'],
    defaultValue: 'true',
  },
];

export const REPLACE_OPTION: BooleanSyncOption = {
  kind: 'boolean',
  key: 'Replace',
  label: 'Replace',
  description: 'Use kubectl replace/create instead of apply. Resources are deleted and recreated when needed, which can cause downtime.',
  destructive: true,
};

/** Catalog for this installation: Replace only when settings.syncWithReplaceAllowed. */
export function syncOptionCatalog(replaceAllowed: boolean): SyncOptionDef[] {
  return replaceAllowed ? [...SYNC_OPTIONS, REPLACE_OPTION] : [...SYNC_OPTIONS];
}

function prefix(key: string): string {
  return `${key}=`;
}

export function optionValue(options: readonly string[], key: string): string | undefined {
  const found = options.find((o) => o.startsWith(prefix(key)));
  return found?.slice(prefix(key).length);
}

export function isBooleanOptionChecked(options: readonly string[], def: BooleanSyncOption): boolean {
  const value = optionValue(options, def.key);
  return def.inverted ? value === 'false' : value === 'true';
}

/** Returns options with `key` set to `value` (undefined removes it), keeping unknown options. */
export function setOption(options: readonly string[], key: string, value: string | undefined): string[] {
  const rest = options.filter((o) => !o.startsWith(prefix(key)));
  return value === undefined ? rest : [...rest, `${key}=${value}`];
}

export function setBooleanOption(options: readonly string[], def: BooleanSyncOption, checked: boolean): string[] {
  if (!checked) return setOption(options, def.key, undefined);
  return setOption(options, def.key, def.inverted ? 'false' : 'true');
}

/** Select options: the default value is not written. */
export function setSelectOption(options: readonly string[], def: SelectSyncOption, value: string): string[] {
  return setOption(options, def.key, value === def.defaultValue ? undefined : value);
}

export interface RetryForm {
  enabled: boolean;
  limit: string;
  duration: string;
  factor: string;
  maxDuration: string;
  refresh: boolean;
}

export const DEFAULT_RETRY: RetryForm = { enabled: false, limit: '5', duration: '5s', factor: '2', maxDuration: '3m', refresh: false };

export function retryFormFrom(retry: RetryStrategy | undefined): RetryForm {
  if (!retry) return DEFAULT_RETRY;
  return {
    enabled: true,
    limit: retry.limit !== undefined ? String(retry.limit) : DEFAULT_RETRY.limit,
    duration: retry.backoff?.duration ?? DEFAULT_RETRY.duration,
    factor: retry.backoff?.factor !== undefined ? String(retry.backoff.factor) : DEFAULT_RETRY.factor,
    maxDuration: retry.backoff?.maxDuration ?? DEFAULT_RETRY.maxDuration,
    refresh: retry.refresh ?? false,
  };
}

const DURATION = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;

/** Validation messages per retry field (empty when valid). */
export function validateRetry(form: RetryForm): Partial<Record<'limit' | 'duration' | 'factor' | 'maxDuration', string>> {
  if (!form.enabled) return {};
  const errors: Partial<Record<'limit' | 'duration' | 'factor' | 'maxDuration', string>> = {};
  if (!/^-?\d+$/.test(form.limit.trim())) errors.limit = 'Enter a whole number (negative retries forever).';
  if (form.duration.trim() && !DURATION.test(form.duration.trim())) errors.duration = 'Use a Go duration such as 5s, 2m or 1h30m.';
  if (form.maxDuration.trim() && !DURATION.test(form.maxDuration.trim())) errors.maxDuration = 'Use a Go duration such as 3m or 1h.';
  if (form.factor.trim() && !(Number(form.factor) >= 1)) errors.factor = 'Enter a number of at least 1.';
  return errors;
}

export function retryFromForm(form: RetryForm): RetryStrategy | undefined {
  if (!form.enabled) return undefined;
  const backoff: NonNullable<RetryStrategy['backoff']> = {};
  if (form.duration.trim()) backoff.duration = form.duration.trim();
  if (form.factor.trim()) backoff.factor = Number(form.factor);
  if (form.maxDuration.trim()) backoff.maxDuration = form.maxDuration.trim();
  return { limit: Number(form.limit), backoff, ...(form.refresh ? { refresh: true } : {}) };
}

export interface SyncResourceRef {
  group?: string | undefined;
  kind: string;
  namespace?: string | undefined;
  name: string;
}

export interface SyncForm {
  /** One revision per source position. */
  revisions: string[];
  prune: boolean;
  dryRun: boolean;
  applyOnly: boolean;
  force: boolean;
  syncOptions: string[];
  retry: RetryForm;
  /** Selected resources; null means "all" (a full sync). */
  resources: SyncResourceRef[] | null;
}

export function initialSyncForm(app: Application): SyncForm {
  return {
    revisions: appSources(app).map((s) => s.targetRevision ?? 'HEAD'),
    prune: false,
    dryRun: false,
    applyOnly: false,
    force: false,
    syncOptions: [...(app.spec.syncPolicy?.syncOptions ?? [])],
    retry: retryFormFrom(app.spec.syncPolicy?.retry),
    resources: null,
  };
}

/**
 * POST .../sync body (ApplicationSyncRequest). Revisions are sent only when they differ from the
 * spec (an override needs the `override` permission); a selective sync lists its resources.
 * `syncOptions` ({items}) replaces the Application's options when present, so it is sent when
 * options are selected or when the user cleared options the Application declares.
 */
export function buildSyncRequest(app: Application, form: SyncForm): ApplicationSyncRequest {
  const sources = appSources(app);
  const request: ApplicationSyncRequest = {
    prune: form.prune,
    dryRun: form.dryRun,
    strategy: form.applyOnly ? { apply: { force: form.force } } : { hook: { force: form.force } },
  };
  const changed = sources.some((s, i) => (form.revisions[i] ?? '').trim() !== (s.targetRevision ?? 'HEAD'));
  if (changed) {
    if (isMultiSource(app)) request.revisions = sources.map((s, i) => (form.revisions[i] ?? '').trim() || (s.targetRevision ?? 'HEAD'));
    else request.revision = (form.revisions[0] ?? '').trim() || 'HEAD';
  }
  if (form.syncOptions.length > 0 || (app.spec.syncPolicy?.syncOptions ?? []).length > 0) {
    request.syncOptions = { items: [...form.syncOptions] };
  }
  if (form.resources !== null) {
    request.resources = form.resources.map((r) => ({
      kind: r.kind,
      name: r.name,
      ...(r.group ? { group: r.group } : {}),
      ...(r.namespace ? { namespace: r.namespace } : {}),
    }));
  }
  const retry = retryFromForm(form.retry);
  if (retry) request.retryStrategy = retry;
  return request;
}

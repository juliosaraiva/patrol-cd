/**
 * Pure editing helpers for the application details/parameters panel: form models derived from an
 * Application, validation, and application of a form back onto a base object (the version the
 * edit started from, so the server's optimistic concurrency check catches concurrent changes).
 */
import { parse as parseYAML, stringify as stringifyYAML, YAMLParseError } from 'yaml';
import type { Application } from '../../../api/types';
import { cloneJSON, type SpecSource } from '../shared/app';
import { DEFAULT_RETRY, retryFormFrom, retryFromForm, validateRetry, type RetryForm } from '../sync/syncOptions';

export interface KV {
  key: string;
  value: string;
}

export function toKV(record: Readonly<Record<string, string>> | undefined): KV[] {
  return Object.entries(record ?? {}).map(([key, value]) => ({ key, value }));
}

export function fromKV(rows: readonly KV[]): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const row of rows) {
    const key = row.key.trim();
    if (key) out[key] = row.value;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

const DNS_SUBDOMAIN = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
const QUALIFIED_NAME = /^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$/;
const LABEL_VALUE = /^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$/;

/** Kubernetes label/annotation key: optional DNS prefix + "/" + name of at most 63 characters. */
export function validateKey(key: string): string | undefined {
  const slash = key.lastIndexOf('/');
  const prefix = slash >= 0 ? key.slice(0, slash) : '';
  const name = slash >= 0 ? key.slice(slash + 1) : key;
  if (slash >= 0 && (prefix.length === 0 || prefix.length > 253 || !DNS_SUBDOMAIN.test(prefix))) return 'The prefix must be a DNS subdomain.';
  if (name.length === 0 || name.length > 63 || !QUALIFIED_NAME.test(name)) {
    return 'Use at most 63 letters, digits, "-", "_" or ".", starting and ending with a letter or digit.';
  }
  return undefined;
}

/**
 * Row errors of key/value rows. 'label' and 'annotation' check Kubernetes key syntax (and label
 * values); 'map' only requires unique, non-empty keys (plugin parameter maps).
 */
export function validateKVRows(rows: readonly KV[], kind: 'label' | 'annotation' | 'map'): Record<number, string> {
  const errors: Record<number, string> = {};
  const seen = new Set<string>();
  rows.forEach((row, i) => {
    const key = row.key.trim();
    if (!key) {
      if (row.value) errors[i] = 'Enter a key or remove the row.';
      return;
    }
    if (seen.has(key)) errors[i] = `Duplicate key ${key}.`;
    seen.add(key);
    if (kind === 'map') return;
    const keyError = validateKey(key);
    if (keyError) errors[i] = keyError;
    else if (kind === 'label' && (row.value.length > 63 || !LABEL_VALUE.test(row.value))) {
      errors[i] = 'Label values have at most 63 letters, digits, "-", "_" or ".", starting and ending with a letter or digit.';
    }
  });
  return errors;
}

export interface InfoRow {
  name: string;
  value: string;
}

export interface SummaryForm {
  project: string;
  labels: KV[];
  annotations: KV[];
  destinationMode: 'server' | 'name';
  destinationServer: string;
  destinationName: string;
  destinationNamespace: string;
  automated: boolean;
  prune: boolean;
  selfHeal: boolean;
  allowEmpty: boolean;
  syncOptions: string[];
  retry: RetryForm;
  info: InfoRow[];
  revisionHistoryLimit: string;
}

export function summaryFormFrom(app: Application): SummaryForm {
  const d = app.spec.destination;
  const automated = app.spec.syncPolicy?.automated;
  return {
    project: app.spec.project,
    labels: toKV(app.metadata.labels),
    annotations: toKV(app.metadata.annotations),
    destinationMode: d.name && !d.server ? 'name' : 'server',
    destinationServer: d.server ?? '',
    destinationName: d.name ?? '',
    destinationNamespace: d.namespace ?? '',
    automated: automated !== undefined && automated.enabled !== false,
    prune: automated?.prune ?? false,
    selfHeal: automated?.selfHeal ?? false,
    allowEmpty: automated?.allowEmpty ?? false,
    syncOptions: [...(app.spec.syncPolicy?.syncOptions ?? [])],
    retry: app.spec.syncPolicy?.retry ? retryFormFrom(app.spec.syncPolicy.retry) : DEFAULT_RETRY,
    info: (app.spec.info ?? []).map((i) => ({ name: i.name, value: i.value })),
    revisionHistoryLimit: app.spec.revisionHistoryLimit !== undefined ? String(app.spec.revisionHistoryLimit) : '',
  };
}

export type SummaryErrors = Partial<Record<'project' | 'destination' | 'revisionHistoryLimit' | 'info' | 'retry', string>> & {
  labels?: Record<number, string>;
  annotations?: Record<number, string>;
};

export function validateSummary(form: SummaryForm): SummaryErrors {
  const errors: SummaryErrors = {};
  if (!form.project.trim()) errors.project = 'Choose a project.';
  if (form.destinationMode === 'server' && !form.destinationServer.trim()) errors.destination = 'Enter the cluster API server URL.';
  if (form.destinationMode === 'name' && !form.destinationName.trim()) errors.destination = 'Enter the cluster name.';
  const limit = form.revisionHistoryLimit.trim();
  if (limit && !/^\d+$/.test(limit)) errors.revisionHistoryLimit = 'Enter a whole number of at least 0.';
  if (form.info.some((i) => (i.name.trim() === '') !== (i.value.trim() === ''))) errors.info = 'Every info item needs a name and a value.';
  if (Object.keys(validateRetry(form.retry)).length > 0) errors.retry = 'Fix the retry settings.';
  const labels = validateKVRows(form.labels, 'label');
  if (Object.keys(labels).length > 0) errors.labels = labels;
  const annotations = validateKVRows(form.annotations, 'annotation');
  if (Object.keys(annotations).length > 0) errors.annotations = annotations;
  return errors;
}

export function hasErrors(errors: object): boolean {
  return Object.values(errors).some((v) => v !== undefined && (typeof v !== 'object' || Object.keys(v as object).length > 0));
}

/** The base application with the summary form applied (status untouched). */
export function applySummary(base: Application, form: SummaryForm): Application {
  const app = cloneJSON(base);
  app.metadata.labels = fromKV(form.labels);
  app.metadata.annotations = fromKV(form.annotations);
  app.spec.project = form.project.trim();
  app.spec.destination = {
    ...(form.destinationMode === 'server' ? { server: form.destinationServer.trim() } : { name: form.destinationName.trim() }),
    ...(form.destinationNamespace.trim() ? { namespace: form.destinationNamespace.trim() } : {}),
  };
  const policy = { ...(app.spec.syncPolicy ?? {}) };
  const previous = policy.automated;
  if (form.automated || previous) {
    // Flags are written when on or already present, so an untouched policy round-trips unchanged.
    const automated: NonNullable<typeof previous> = { ...previous };
    // A present `automated` block means enabled; `enabled` is written only to turn it off or when set.
    if (!form.automated || previous?.enabled !== undefined) automated.enabled = form.automated;
    for (const [key, value] of [['prune', form.prune], ['selfHeal', form.selfHeal], ['allowEmpty', form.allowEmpty]] as const) {
      if (value || previous?.[key] !== undefined) automated[key] = value;
    }
    policy.automated = automated;
  }
  policy.syncOptions = form.syncOptions.length > 0 ? [...form.syncOptions] : undefined;
  policy.retry = retryFromForm(form.retry);
  const cleaned = Object.fromEntries(Object.entries(policy).filter(([, v]) => v !== undefined));
  app.spec.syncPolicy = Object.keys(cleaned).length > 0 ? cleaned : undefined;
  const info = form.info.filter((i) => i.name.trim() && i.value.trim()).map((i) => ({ name: i.name.trim(), value: i.value.trim() }));
  app.spec.info = info.length > 0 ? info : undefined;
  const limit = form.revisionHistoryLimit.trim();
  app.spec.revisionHistoryLimit = limit ? Number(limit) : undefined;
  return dropUndefined(app);
}

/** Removes undefined-valued keys recursively (JSON round trip). */
export function dropUndefined<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}

// ---------------------------------------------------------------------------------------------
// Sources

export type ToolKind = 'auto' | 'helm' | 'kustomize' | 'directory' | 'plugin';

export function toolOf(source: SpecSource): ToolKind {
  if (source.chart || source.helm) return 'helm';
  if (source.kustomize) return 'kustomize';
  if (source.plugin) return 'plugin';
  if (source.directory) return 'directory';
  return 'auto';
}

/** Switches a source to another tool, dropping the other tools' settings. */
export function setTool(source: SpecSource, tool: ToolKind): SpecSource {
  const { helm, kustomize, directory, plugin, ...rest } = source;
  switch (tool) {
    case 'helm':
      return { ...rest, helm: helm ?? {} };
    case 'kustomize':
      return { ...rest, kustomize: kustomize ?? {} };
    case 'directory':
      return { ...rest, directory: directory ?? {} };
    case 'plugin':
      return { ...rest, plugin: plugin ?? {} };
    case 'auto':
      return rest;
  }
}

/**
 * Editing state of one source. Values the form edits as rows or text are kept here, not in
 * `source`, so incomplete input (a new row without a key yet, YAML being typed) survives
 * re-renders; compactSource() converts them when saving.
 */
export interface SourceDraft {
  source: SpecSource;
  /** YAML text of helm.valuesObject. */
  valuesObjectText: string;
  /** YAML text of kustomize.patches. */
  patchesText: string;
  /** Rows of kustomize.commonLabels. */
  commonLabels: KV[];
  /** Rows of kustomize.commonAnnotations. */
  commonAnnotations: KV[];
  /** Rows of map-type plugin parameters, by parameter name (only parameters set or edited). */
  pluginMaps: Record<string, KV[]>;
}

export function newSourceDraft(source: SpecSource = { repoURL: '', targetRevision: 'HEAD' }): SourceDraft {
  return { source, valuesObjectText: '', patchesText: '', commonLabels: [], commonAnnotations: [], pluginMaps: {} };
}

function yamlText(value: unknown): string {
  if (value === undefined || value === null) return '';
  if (Array.isArray(value) && value.length === 0) return '';
  if (typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === 0) return '';
  return stringifyYAML(value);
}

export function sourceDrafts(app: Application): SourceDraft[] {
  const sources = app.spec.sources && app.spec.sources.length > 0 ? app.spec.sources : app.spec.source ? [app.spec.source] : [];
  return sources.map((s) => ({
    source: cloneJSON(s),
    valuesObjectText: yamlText(s.helm?.valuesObject),
    patchesText: yamlText(s.kustomize?.patches),
    commonLabels: toKV(s.kustomize?.commonLabels),
    commonAnnotations: toKV(s.kustomize?.commonAnnotations),
    pluginMaps: pluginMapsOf(s),
  }));
}

function pluginMapsOf(source: SpecSource): Record<string, KV[]> {
  const out: Record<string, KV[]> = {};
  for (const p of source.plugin?.parameters ?? []) {
    if (p.name && p.map) out[p.name] = toKV(p.map);
  }
  return out;
}

/** "line N, column M: message" for YAML errors. */
export function yamlErrorMessage(error: unknown): string {
  if (error instanceof YAMLParseError) {
    const pos = error.linePos?.[0];
    const where = pos ? `Line ${pos.line}, column ${pos.col}: ` : '';
    return `${where}${error.message.split('\n')[0] ?? ''}`;
  }
  return error instanceof Error ? error.message : String(error);
}

export type SourceErrors = Partial<Record<'repoURL' | 'location' | 'ref' | 'valuesObject' | 'patches' | 'values', string>> & {
  commonLabels?: Record<number, string>;
  commonAnnotations?: Record<number, string>;
  /** Row errors of map-type plugin parameters, by parameter name. */
  pluginMaps?: Record<string, Record<number, string>>;
};

const REF = /^[A-Za-z0-9_-]+$/;

export function validateSources(drafts: readonly SourceDraft[], multi: boolean): SourceErrors[] {
  const refs = new Map<string, number>();
  return drafts.map((d, i) => {
    const errors: SourceErrors = {};
    const s = d.source;
    if (!s.repoURL.trim()) errors.repoURL = 'Enter the repository URL.';
    if (!s.path && !s.chart && !(multi && s.ref)) errors.location = multi ? 'Set a path, a chart, or a ref.' : 'Set a path or a chart.';
    if (s.ref) {
      if (!REF.test(s.ref)) errors.ref = 'Use letters, digits, "-" or "_".';
      else if (refs.has(s.ref)) errors.ref = `Ref ${s.ref} is already used by source ${(refs.get(s.ref) ?? 0) + 1}.`;
      refs.set(s.ref, i);
    }
    if (d.valuesObjectText.trim()) {
      try {
        const parsed: unknown = parseYAML(d.valuesObjectText);
        if (parsed !== null && (typeof parsed !== 'object' || Array.isArray(parsed))) errors.valuesObject = 'Values object must be a YAML mapping.';
      } catch (err) {
        errors.valuesObject = yamlErrorMessage(err);
      }
    }
    if (d.patchesText.trim()) {
      try {
        const parsed: unknown = parseYAML(d.patchesText);
        if (parsed !== null && !Array.isArray(parsed)) errors.patches = 'Patches must be a YAML list.';
      } catch (err) {
        errors.patches = yamlErrorMessage(err);
      }
    }
    if (s.helm?.values?.trim()) {
      try {
        parseYAML(s.helm.values);
      } catch (err) {
        errors.values = yamlErrorMessage(err);
      }
    }
    if (s.kustomize) {
      const labels = validateKVRows(d.commonLabels, 'label');
      if (Object.keys(labels).length > 0) errors.commonLabels = labels;
      const annotations = validateKVRows(d.commonAnnotations, 'annotation');
      if (Object.keys(annotations).length > 0) errors.commonAnnotations = annotations;
    }
    if (s.plugin) {
      const maps = Object.fromEntries(
        Object.entries(d.pluginMaps)
          .map(([name, rows]) => [name, validateKVRows(rows, 'map')] as const)
          .filter(([, rowErrors]) => Object.keys(rowErrors).length > 0),
      );
      if (Object.keys(maps).length > 0) errors.pluginMaps = maps;
    }
    return errors;
  });
}

type PluginParams = NonNullable<NonNullable<SpecSource['plugin']>['parameters']>;

/** Sets a map-type plugin parameter in place (or removes it when `map` is undefined), keeping order. */
export function setPluginMap(params: PluginParams | undefined, name: string, map: Record<string, string> | undefined): PluginParams {
  const list = params ?? [];
  if (!map) return list.filter((p) => p.name !== name);
  return list.some((p) => p.name === name) ? list.map((p) => (p.name === name ? { name, map } : p)) : [...list, { name, map }];
}

function compactSource(draft: SourceDraft): SpecSource {
  const s = cloneJSON(draft.source);
  s.repoURL = s.repoURL.trim();
  for (const key of ['path', 'chart', 'ref', 'targetRevision', 'name'] as const) {
    const value = s[key];
    if (typeof value === 'string' && value.trim() === '') delete s[key];
  }
  if (s.helm) {
    const values = draft.valuesObjectText.trim() ? (parseYAML(draft.valuesObjectText) as Record<string, unknown> | null) : null;
    if (values) s.helm.valuesObject = values;
    else delete s.helm.valuesObject;
    if (s.helm.parameters?.length === 0) delete s.helm.parameters;
    if (s.helm.valueFiles?.length === 0) delete s.helm.valueFiles;
    if (!s.helm.values) delete s.helm.values;
  }
  if (s.kustomize) {
    const patches = draft.patchesText.trim() ? (parseYAML(draft.patchesText) as NonNullable<SpecSource['kustomize']>['patches'] | null) : null;
    if (patches && patches.length > 0) s.kustomize.patches = patches;
    else delete s.kustomize.patches;
    s.kustomize.commonLabels = fromKV(draft.commonLabels);
    s.kustomize.commonAnnotations = fromKV(draft.commonAnnotations);
    const replicas = s.kustomize.replicas?.filter((r) => r.name.trim() !== '');
    s.kustomize.replicas = replicas && replicas.length > 0 ? replicas : undefined;
  }
  if (s.plugin) {
    let params = s.plugin.parameters;
    for (const [name, rows] of Object.entries(draft.pluginMaps)) params = setPluginMap(params, name, fromKV(rows));
    s.plugin.parameters = params && params.length > 0 ? params : undefined;
    const env = s.plugin.env?.filter((e) => e.name.trim() !== '');
    s.plugin.env = env && env.length > 0 ? env : undefined;
  }
  return dropUndefined(s);
}

/** Base application with the source drafts applied (single `source` or `sources`). */
export function applySources(base: Application, drafts: readonly SourceDraft[], multi: boolean): Application {
  const app = cloneJSON(base);
  const sources = drafts.map(compactSource);
  if (multi) {
    app.spec.sources = sources;
    delete app.spec.source;
  } else {
    app.spec.source = sources[0];
    delete app.spec.sources;
  }
  return app;
}

export function linesToList(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean);
}

export function listToLines(list: readonly string[] | undefined): string {
  return (list ?? []).join('\n');
}

// ---------------------------------------------------------------------------------------------
// Helm parameters with override markers

export interface HelmParamRow {
  name: string;
  /** Effective value: the override when present, else the chart default. */
  value: string;
  defaultValue: string | undefined;
  overridden: boolean;
  forceString: boolean;
}

export function helmParamRows(
  overrides: readonly { name?: string; value?: string; forceString?: boolean }[] | undefined,
  defaults: readonly { name?: string; value?: string }[] | undefined,
): HelmParamRow[] {
  const rows = new Map<string, HelmParamRow>();
  for (const d of defaults ?? []) {
    if (!d.name) continue;
    rows.set(d.name, { name: d.name, value: d.value ?? '', defaultValue: d.value ?? '', overridden: false, forceString: false });
  }
  for (const o of overrides ?? []) {
    if (!o.name) continue;
    const existing = rows.get(o.name);
    rows.set(o.name, {
      name: o.name,
      value: o.value ?? '',
      defaultValue: existing?.defaultValue,
      overridden: true,
      forceString: o.forceString ?? false,
    });
  }
  return [...rows.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/** Sets (or, with `value` undefined, removes) one Helm parameter override. */
export function setHelmParam(
  params: readonly { name?: string; value?: string; forceString?: boolean }[] | undefined,
  name: string,
  value: string | undefined,
  forceString = false,
): { name?: string; value?: string; forceString?: boolean }[] {
  const rest = (params ?? []).filter((p) => p.name !== name);
  if (value === undefined) return rest;
  return [...rest, { name, value, ...(forceString ? { forceString: true } : {}) }];
}

// ---------------------------------------------------------------------------------------------
// Plugin parameter announcements

export interface AnnouncedParam {
  name: string;
  title: string;
  tooltip: string | undefined;
  required: boolean;
  collectionType: 'string' | 'array' | 'map';
  string: string | undefined;
  array: string[] | undefined;
  map: Record<string, string> | undefined;
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

/** Reads `parametersAnnouncement` from the repo server's plugin details (untrusted shape). */
export function parseAnnouncement(plugin: Record<string, unknown> | undefined): AnnouncedParam[] {
  const raw = plugin?.parametersAnnouncement;
  if (!Array.isArray(raw)) return [];
  const out: AnnouncedParam[] = [];
  for (const item of raw as unknown[]) {
    if (item === null || typeof item !== 'object') continue;
    const p = item as Record<string, unknown>;
    const name = str(p.name);
    if (!name) continue;
    const type = str(p.collectionType);
    const map = p.map && typeof p.map === 'object' && !Array.isArray(p.map) ? (Object.fromEntries(Object.entries(p.map).filter(([, v]) => typeof v === 'string')) as Record<string, string>) : undefined;
    out.push({
      name,
      title: str(p.title) ?? name,
      tooltip: str(p.tooltip),
      required: p.required === true,
      collectionType: type === 'array' || type === 'map' ? type : 'string',
      string: str(p.string),
      array: Array.isArray(p.array) ? p.array.filter((v): v is string => typeof v === 'string') : undefined,
      map,
    });
  }
  return out;
}

type PluginParam = NonNullable<NonNullable<SpecSource['plugin']>['parameters']>[number];

export function setPluginParam(params: readonly PluginParam[] | undefined, next: PluginParam): PluginParam[] {
  const rest = (params ?? []).filter((p) => p.name !== next.name);
  const empty = next.string === undefined && next.array === undefined && next.map === undefined;
  return empty ? rest : [...rest, next];
}

// ---------------------------------------------------------------------------------------------
// Manifest (whole application as YAML)

/** Editable view of the application: no status and no server-managed metadata. */
export function editableManifest(app: Application): Record<string, unknown> {
  const metadata: Record<string, unknown> = { name: app.metadata.name };
  if (app.metadata.namespace) metadata.namespace = app.metadata.namespace;
  if (app.metadata.labels && Object.keys(app.metadata.labels).length > 0) metadata.labels = app.metadata.labels;
  if (app.metadata.annotations && Object.keys(app.metadata.annotations).length > 0) metadata.annotations = app.metadata.annotations;
  if (app.metadata.finalizers && app.metadata.finalizers.length > 0) metadata.finalizers = app.metadata.finalizers;
  return { apiVersion: app.apiVersion ?? 'cd.patrol.example/v1alpha1', kind: app.kind ?? 'Application', metadata, spec: app.spec };
}

export function manifestYAML(app: Application): string {
  return stringifyYAML(editableManifest(app));
}

export type ManifestResult = { ok: true; app: Application } | { ok: false; errors: string[] };

function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

function stringRecord(v: unknown): v is Record<string, string> {
  return isRecord(v) && Object.values(v).every((x) => typeof x === 'string');
}

/**
 * Parses and validates edited YAML against the base object. Name and namespace cannot change;
 * labels, annotations, finalizers and spec are taken from the text; resourceVersion and status
 * come from the base.
 */
export function parseManifest(text: string, base: Application): ManifestResult {
  let doc: unknown;
  try {
    doc = parseYAML(text);
  } catch (err) {
    return { ok: false, errors: [yamlErrorMessage(err)] };
  }
  const errors: string[] = [];
  if (!isRecord(doc)) return { ok: false, errors: ['The manifest must be a YAML mapping with apiVersion, kind, metadata and spec.'] };
  if (doc.kind !== undefined && doc.kind !== 'Application') errors.push('kind must be Application.');
  const metadata = isRecord(doc.metadata) ? doc.metadata : {};
  if (metadata.name !== base.metadata.name) errors.push('metadata.name cannot change; create a new application to rename it.');
  if ((metadata.namespace ?? base.metadata.namespace) !== base.metadata.namespace) errors.push('metadata.namespace cannot change.');
  if (metadata.labels !== undefined && metadata.labels !== null && !stringRecord(metadata.labels)) errors.push('metadata.labels must map strings to strings.');
  if (metadata.annotations !== undefined && metadata.annotations !== null && !stringRecord(metadata.annotations)) {
    errors.push('metadata.annotations must map strings to strings.');
  }
  if (metadata.finalizers !== undefined && metadata.finalizers !== null && !(Array.isArray(metadata.finalizers) && metadata.finalizers.every((f) => typeof f === 'string'))) {
    errors.push('metadata.finalizers must be a list of strings.');
  }
  const spec = doc.spec;
  if (!isRecord(spec)) errors.push('spec is required.');
  else {
    if (typeof spec.project !== 'string' || spec.project === '') errors.push('spec.project is required.');
    if (!isRecord(spec.destination)) errors.push('spec.destination is required (server or name, and namespace).');
    else if (!spec.destination.server && !spec.destination.name) errors.push('spec.destination needs a server or a name.');
    const hasSource = isRecord(spec.source) || (Array.isArray(spec.sources) && spec.sources.length > 0) || isRecord(spec.sourceHydrator);
    if (!hasSource) errors.push('spec.source or spec.sources is required.');
    if (isRecord(spec.source) && Array.isArray(spec.sources) && spec.sources.length > 0) errors.push('Set either spec.source or spec.sources, not both.');
    const sources = Array.isArray(spec.sources) ? spec.sources : isRecord(spec.source) ? [spec.source] : [];
    sources.forEach((s, i) => {
      if (!isRecord(s) || typeof s.repoURL !== 'string' || s.repoURL === '') errors.push(`Source ${i + 1}: repoURL is required.`);
    });
  }
  if (errors.length > 0) return { ok: false, errors };
  const app = cloneJSON(base);
  app.metadata.labels = (metadata.labels as Record<string, string> | null | undefined) ?? undefined;
  app.metadata.annotations = (metadata.annotations as Record<string, string> | null | undefined) ?? undefined;
  app.metadata.finalizers = (metadata.finalizers as string[] | null | undefined) ?? undefined;
  app.spec = spec as Application['spec'];
  return { ok: true, app: dropUndefined(app) };
}

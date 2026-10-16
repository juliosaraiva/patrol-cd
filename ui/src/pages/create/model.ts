/**
 * Application creation model (API-091): the draft is an Application object edited by both the
 * form and the YAML editor, so switching modes never loses data. Helpers here are pure.
 */
import { LineCounter, parseDocument, stringify } from 'yaml';
import type { Application, ApplicationSource, Repository } from '../../api/types';

export type Spec = Application['spec'];
export type SourceKind = 'git' | 'helm' | 'oci';
export type ToolType = 'Directory' | 'Helm' | 'Kustomize' | 'Plugin';
export type HydratorSpec = NonNullable<Spec['sourceHydrator']>;

export const API_VERSION = 'cd.patrol.example/v1alpha1';
/** Cascading deletion finalizer (api/v1alpha1 FinalizerResources). */
export const RESOURCES_FINALIZER = 'resources-finalizer.cd.patrol.example';

export function defaultSource(repoURL = ''): ApplicationSource {
  return { repoURL, path: '', targetRevision: 'HEAD' };
}

export function defaultApplication(initial: { repoURL?: string; project?: string } = {}): Application {
  return {
    apiVersion: API_VERSION,
    kind: 'Application',
    metadata: { name: '' },
    spec: {
      project: initial.project ?? 'default',
      source: defaultSource(initial.repoURL ?? ''),
      destination: { server: '', namespace: '' },
    },
  };
}

/** Deep copy for immutable updates through a mutating recipe. */
export function produce<T>(value: T, recipe: (draft: T) => void): T {
  const copy = structuredClone(value);
  recipe(copy);
  return copy;
}

/** Sources of a draft, whatever its mode (single source, sources, or none for the hydrator). */
export function sourcesOf(spec: Spec): ApplicationSource[] {
  if (Array.isArray(spec.sources) && spec.sources.length > 0) return spec.sources;
  return spec.source && typeof spec.source === 'object' ? [spec.source] : [];
}

/** String at a path of an untyped value, '' when absent or not a string (drafts may be partial). */
export function textAt(value: unknown, ...keys: (string | number)[]): string {
  let current: unknown = value;
  for (const key of keys) {
    if (!current || typeof current !== 'object') return '';
    current = (current as Record<string | number, unknown>)[key];
  }
  return typeof current === 'string' ? current : '';
}

export function isMultiSource(spec: Spec): boolean {
  return Array.isArray(spec.sources);
}

/** Switches between spec.source and spec.sources, keeping the first source. */
export function setMultiSource(spec: Spec, multi: boolean): Spec {
  return produce(spec, (s) => {
    const current = sourcesOf(s);
    if (multi) {
      s.sources = current.length > 0 ? current : [defaultSource()];
      delete s.source;
    } else {
      const first = current[0] ?? defaultSource();
      delete first.ref;
      delete first.name;
      s.source = first;
      delete s.sources;
    }
  });
}

/** Repository type from a registered repository, else from the URL. */
export function sourceKind(source: ApplicationSource, repos: readonly Repository[] = []): SourceKind {
  const repoURL = textAt(source, 'repoURL');
  const repo = repos.find((r) => r.spec.url === repoURL);
  if (repo?.spec.type) return repo.spec.type;
  if (repoURL.startsWith('oci://')) return 'oci';
  if (source.chart !== undefined) return 'helm';
  return 'git';
}

/** Tool section a source configures explicitly (helm/kustomize/directory/plugin keys present). */
export function explicitTool(source: ApplicationSource): ToolType | null {
  if (source.helm) return 'Helm';
  if (source.kustomize) return 'Kustomize';
  if (source.plugin) return 'Plugin';
  if (source.directory) return 'Directory';
  return null;
}

/** Puts the source in the shape a repository type needs (chart for Helm, path otherwise). */
export function applyKind(source: ApplicationSource, kind: SourceKind): ApplicationSource {
  return produce(source, (s) => {
    if (kind === 'helm') {
      s.chart ??= '';
      delete s.path;
      if (s.targetRevision === 'HEAD') s.targetRevision = '';
    } else {
      delete s.chart;
      s.path ??= kind === 'oci' ? '.' : '';
      if (kind === 'git' && !s.targetRevision) s.targetRevision = 'HEAD';
    }
  });
}

// ------------------------------------------------------------------------------------------
// Sync options

export interface SyncOptionDef {
  key: string;
  label: string;
  description: string;
  /** Option value written when checked. */
  on: string;
  /** Only shown when settings.syncWithReplaceAllowed. */
  replace?: boolean;
}

export const SYNC_OPTIONS: readonly SyncOptionDef[] = [
  { key: 'Validate', on: 'false', label: 'Skip schema validation', description: 'Apply without kubectl schema validation (Validate=false).' },
  { key: 'CreateNamespace', on: 'true', label: 'Auto-create namespace', description: 'Create the destination namespace if it does not exist.' },
  { key: 'PruneLast', on: 'true', label: 'Prune last', description: 'Delete pruned resources after all other resources are synced and healthy.' },
  { key: 'ApplyOutOfSyncOnly', on: 'true', label: 'Apply out of sync only', description: 'Apply only resources that are out of sync.' },
  { key: 'RespectIgnoreDifferences', on: 'true', label: 'Respect ignore differences', description: 'Keep fields listed in ignoreDifferences as they are live during sync.' },
  { key: 'ServerSideApply', on: 'true', label: 'Server-side apply', description: 'Use Kubernetes server-side apply.' },
  { key: 'FailOnSharedResource', on: 'true', label: 'Fail on shared resource', description: 'Fail the sync if a resource is managed by another application.' },
  { key: 'Replace', on: 'true', label: 'Replace', description: 'Use kubectl replace/create instead of apply. Destructive.', replace: true },
];

export const PRUNE_PROPAGATION = ['foreground', 'background', 'orphan'] as const;

/** Only the string entries of an options list (a hand-edited draft may hold anything). */
function optionList(options: readonly string[] | undefined): string[] {
  return Array.isArray(options) ? options.filter((o): o is string => typeof o === 'string') : [];
}

export function syncOptionValue(options: readonly string[] | undefined, key: string): string | undefined {
  const entry = optionList(options).find((o) => o.startsWith(`${key}=`));
  return entry?.slice(key.length + 1);
}

export function setSyncOption(options: readonly string[] | undefined, key: string, value: string | undefined): string[] {
  const rest = optionList(options).filter((o) => !o.startsWith(`${key}=`));
  return value === undefined ? rest : [...rest, `${key}=${value}`];
}

// ------------------------------------------------------------------------------------------
// YAML

export function toYAML(app: Application): string {
  return stringify(cleanApplication(app), { lineWidth: 0 });
}

/**
 * Expected shape of the fields the form reads. Fields outside it pass through untouched (the
 * server validates them). `required` strings and objects are filled in when missing, so a
 * half-edited document (a deleted `repoURL:` line, `sourceHydrator: {}`) becomes a draft with
 * empty fields that validation names, instead of a value the form cannot read.
 */
type Shape =
  | { t: 'string'; required?: boolean }
  | { t: 'number' }
  | { t: 'boolean' }
  | { t: 'list'; of: Shape }
  | { t: 'map'; fields: Record<string, Shape>; required?: boolean };

const str: Shape = { t: 'string' };
const reqStr: Shape = { t: 'string', required: true };
const num: Shape = { t: 'number' };
const bool: Shape = { t: 'boolean' };
const strList: Shape = { t: 'list', of: str };
const map = (fields: Record<string, Shape>, required = false): Shape => ({ t: 'map', fields, required });

const SOURCE_SHAPE = map({
  repoURL: reqStr,
  path: str,
  targetRevision: str,
  chart: str,
  ref: str,
  name: str,
  helm: map({
    releaseName: str,
    values: str,
    valueFiles: strList,
    parameters: { t: 'list', of: map({ name: str, value: str, forceString: bool }) },
  }),
  kustomize: map({ namePrefix: str, nameSuffix: str, namespace: str, images: strList }),
  directory: map({ recurse: bool, include: str, exclude: str }),
  plugin: map({ name: str, env: { t: 'list', of: map({ name: reqStr, value: reqStr }) } }),
});

const APPLICATION_SHAPE = map({
  metadata: map({ name: str, namespace: str, finalizers: strList }, true),
  spec: map(
    {
      project: reqStr,
      source: SOURCE_SHAPE,
      sources: { t: 'list', of: SOURCE_SHAPE },
      sourceHydrator: map({
        drySource: map({ repoURL: reqStr, targetRevision: reqStr, path: reqStr }, true),
        syncSource: map({ targetBranch: reqStr, path: reqStr }, true),
        hydrateTo: map({ targetBranch: reqStr }),
      }),
      destination: map({ server: str, name: str, namespace: str }, true),
      syncPolicy: map({
        automated: map({ prune: bool, selfHeal: bool, allowEmpty: bool, enabled: bool }),
        syncOptions: strList,
        retry: map({ limit: num, backoff: map({ duration: str, factor: num, maxDuration: str }) }),
      }),
    },
    true,
  ),
});

type Path = (string | number)[];
interface ShapeIssue {
  path: Path;
  message: string;
}

function pathLabel(path: Path): string {
  return path.reduce<string>((label, key) => (typeof key === 'number' ? `${label}[${key}]` : label ? `${label}.${key}` : key), '');
}

function describeMismatch(shape: Shape, value: unknown, source: string | undefined): string {
  switch (shape.t) {
    case 'string':
      return typeof value === 'number' || typeof value === 'boolean'
        ? `must be a string; put the value in quotes, e.g. "${source ?? String(value)}"`
        : 'must be a string';
    case 'number':
      return 'must be a number';
    case 'boolean':
      return 'must be true or false';
    case 'list':
      return 'must be a list';
    case 'map':
      return 'must be a mapping (key: value pairs)';
  }
}

/**
 * Checks `value` against `shape`; returns the normalized value (null and missing optional fields
 * removed, required ones filled in) and records the first mismatch.
 */
function conform(value: unknown, shape: Shape, path: Path, issues: ShapeIssue[], sourceOf: (path: Path) => string | undefined): unknown {
  if (value === null || value === undefined) {
    if (shape.t === 'string' && shape.required) return '';
    if (shape.t === 'map' && shape.required) return conform({}, shape, path, issues, sourceOf);
    return undefined;
  }
  const mismatch = () => {
    issues.push({ path, message: describeMismatch(shape, value, sourceOf(path)) });
    return value;
  };
  switch (shape.t) {
    case 'string':
      return typeof value === 'string' ? value : mismatch();
    case 'number':
      return typeof value === 'number' ? value : mismatch();
    case 'boolean':
      return typeof value === 'boolean' ? value : mismatch();
    case 'list':
      return Array.isArray(value) ? value.map((item, i) => conform(item, shape.of, [...path, i], issues, sourceOf)) : mismatch();
    case 'map': {
      if (typeof value !== 'object' || Array.isArray(value)) return mismatch();
      const out: Record<string, unknown> = { ...(value as Record<string, unknown>) };
      for (const [key, fieldShape] of Object.entries(shape.fields)) {
        const next = conform(out[key], fieldShape, [...path, key], issues, sourceOf);
        if (next === undefined) delete out[key];
        else out[key] = next;
      }
      return out;
    }
  }
}

/**
 * Parses the YAML editor content into an Application. Any document that parses yields either a
 * draft the form and validation can read safely or an error naming the line and field.
 */
export function parseApplicationYAML(text: string): { app: Application } | { error: string } {
  const lineCounter = new LineCounter();
  const doc = parseDocument(text, { lineCounter });
  const first = doc.errors[0];
  if (first) {
    const line = first.linePos?.[0]?.line;
    const message = (first.message.split('\n')[0] ?? 'invalid YAML').replace(/ at line \d+, column \d+:?$/, '');
    return { error: `${line ? `Line ${line}: ` : ''}${message}` };
  }
  const value: unknown = doc.toJS();
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { error: 'The document must be a YAML mapping describing an Application.' };
  const obj = value as Record<string, unknown>;
  if (obj.kind !== undefined && obj.kind !== 'Application') return { error: `kind must be Application, not ${typeof obj.kind === 'string' ? obj.kind : JSON.stringify(obj.kind)}.` };
  if (obj.metadata == null) return { error: 'metadata is required (with at least metadata.name).' };
  if (obj.spec == null) return { error: 'spec is required.' };
  if (typeof obj.spec === 'object' && !Array.isArray(obj.spec) && (obj.spec as Record<string, unknown>).destination == null) return { error: 'spec.destination is required.' };

  const nodeAt = (path: Path) => doc.getIn(path, true) as { range?: [number, number, number] } | undefined;
  const issues: ShapeIssue[] = [];
  const normalized = conform(obj, APPLICATION_SHAPE, [], issues, (path) => {
    const range = nodeAt(path)?.range;
    return range ? text.slice(range[0], range[1]).trim() : undefined;
  }) as Partial<Application>;
  const issue = issues[0];
  if (issue) {
    const offset = nodeAt(issue.path)?.range?.[0];
    const line = offset === undefined ? undefined : lineCounter.linePos(offset).line;
    return { error: `${line ? `Line ${line}: ` : ''}${pathLabel(issue.path)} ${issue.message}.` };
  }
  return { app: { apiVersion: API_VERSION, kind: 'Application', ...normalized } as Application };
}

/**
 * Removes empty strings, empty objects and empty arrays (except where meaningful) so the object
 * sent to the server contains only what the user set.
 */
export function cleanApplication(app: Application): Application {
  const clean = (value: unknown, path: string): unknown => {
    if (Array.isArray(value)) {
      const items = value.map((v, i) => clean(v, `${path}[${i}]`)).filter((v) => v !== undefined);
      // spec.sources must stay an array even when empty (the user chose multi-source).
      return items.length > 0 || path === 'spec.sources' ? items : undefined;
    }
    if (value && typeof value === 'object') {
      const entries = Object.entries(value)
        .map(([k, v]) => [k, clean(v, path ? `${path}.${k}` : k)] as const)
        .filter(([, v]) => v !== undefined);
      // Keep objects whose presence is meaningful even when empty.
      if (entries.length === 0 && !/^spec\.(source|sources\[\d+\])\.directory$|^spec\.syncPolicy\.automated$/.test(path)) return undefined;
      return Object.fromEntries(entries);
    }
    if (value === '' || value === null || value === undefined) {
      // repoURL is required by the schema; keep it so validation errors point at it.
      return path.endsWith('.repoURL') ? '' : undefined;
    }
    return value;
  };
  const cleaned = clean(app, '') as Application;
  cleaned.spec ??= { project: app.spec.project, destination: {} };
  cleaned.spec.destination ??= {};
  cleaned.metadata ??= {};
  return cleaned;
}

// ------------------------------------------------------------------------------------------
// Validation

export type FieldErrors = Record<string, string>;

const NAME_RE = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;
const REF_RE = /^[A-Za-z0-9_-]+$/;
const NS_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

/**
 * Client-side checks before POST /applications; the server validates the rest (repository
 * access, project permissions) when "Validate" is on. Keys are field ids used by the form.
 */
export function validateApplication(app: Application, options: { repos?: readonly Repository[]; appNamespaceAllowed?: boolean } = {}): FieldErrors {
  // Reads go through textAt so a partial or hand-edited draft never throws during render.
  const errors: FieldErrors = {};
  const name = textAt(app, 'metadata', 'name');
  if (name === '') errors.name = 'Enter a name.';
  else if (name.length > 253 || !NAME_RE.test(name)) errors.name = 'Use lowercase letters, digits, "-" and "."; start and end with a letter or digit.';
  if (!textAt(app, 'spec', 'project')) errors.project = 'Choose a project.';
  const ns = textAt(app, 'metadata', 'namespace');
  if (ns) {
    if (!options.appNamespaceAllowed) errors.appNamespace = 'Applications in other namespaces are disabled on this instance.';
    else if (!NS_RE.test(ns) || ns.length > 63) errors.appNamespace = 'Enter a valid namespace name.';
  }
  const hydrator: unknown = app.spec.sourceHydrator;
  if (hydrator) {
    if (!textAt(hydrator, 'drySource', 'repoURL')) errors['hydrator.repoURL'] = 'Enter the dry source repository URL.';
    if (!textAt(hydrator, 'syncSource', 'targetBranch')) errors['hydrator.syncBranch'] = 'Enter the branch the hydrated manifests are written to.';
    if (!textAt(hydrator, 'syncSource', 'path')) errors['hydrator.syncPath'] = 'Enter the path the hydrated manifests are written to.';
  } else {
    const sources = sourcesOf(app.spec);
    const multi = isMultiSource(app.spec);
    if (sources.length === 0) errors.sources = 'Add at least one source.';
    const refs = new Map<string, number>();
    sources.forEach((src: unknown, i) => {
      const p = `sources.${i}`;
      if (!textAt(src, 'repoURL')) errors[`${p}.repoURL`] = 'Enter a repository URL.';
      const kind = src && typeof src === 'object' ? sourceKind(src as ApplicationSource, options.repos) : 'git';
      const ref = textAt(src, 'ref');
      const path = textAt(src, 'path');
      const chart = textAt(src, 'chart');
      if (multi && ref !== '') {
        if (!REF_RE.test(ref)) errors[`${p}.ref`] = 'Use letters, digits, "-" and "_".';
        else if (refs.has(ref)) errors[`${p}.ref`] = `Ref "${ref}" is already used by source ${(refs.get(ref) ?? 0) + 1}.`;
        else refs.set(ref, i);
      }
      const valuesOnly = multi && ref !== '' && !path && !chart;
      if (kind === 'helm') {
        if (!chart && !valuesOnly) errors[`${p}.chart`] = 'Choose a chart.';
        if (!textAt(src, 'targetRevision') && !valuesOnly) errors[`${p}.targetRevision`] = 'Enter a chart version (or a semver range such as 1.*).';
      } else if (path === '' && !valuesOnly) {
        errors[`${p}.path`] = kind === 'oci' ? 'Enter a path inside the artifact ("." for its root).' : 'Enter a path in the repository ("." for its root).';
      } else if (path.startsWith('/') || path.split('/').includes('..')) {
        errors[`${p}.path`] = 'Use a relative path inside the repository, without "..".';
      }
    });
  }
  const server = textAt(app, 'spec', 'destination', 'server');
  const clusterName = textAt(app, 'spec', 'destination', 'name');
  const destNamespace = textAt(app, 'spec', 'destination', 'namespace');
  if (!server && !clusterName) errors['destination.target'] = 'Choose a destination cluster.';
  if (server && clusterName) errors['destination.target'] = 'Set the cluster URL or the cluster name, not both.';
  if (destNamespace && (!NS_RE.test(destNamespace) || destNamespace.length > 63)) errors['destination.namespace'] = 'Enter a valid namespace name.';
  return errors;
}

/** Where a validation error (keyed by form field id) lives in the YAML document. */
export function yamlFieldPath(key: string, spec: Spec): string {
  const sourceBase = (index: string) => (isMultiSource(spec) ? `spec.sources[${index}]` : 'spec.source');
  const fixed: Record<string, string> = {
    name: 'metadata.name',
    project: 'spec.project',
    appNamespace: 'metadata.namespace',
    sources: isMultiSource(spec) ? 'spec.sources' : 'spec.source',
    'hydrator.repoURL': 'spec.sourceHydrator.drySource.repoURL',
    'hydrator.syncBranch': 'spec.sourceHydrator.syncSource.targetBranch',
    'hydrator.syncPath': 'spec.sourceHydrator.syncSource.path',
    'destination.target': 'spec.destination',
    'destination.namespace': 'spec.destination.namespace',
  };
  const known = fixed[key];
  if (known) return known;
  const match = /^sources\.(\d+)\.(.+)$/.exec(key);
  return match ? `${sourceBase(match[1] ?? '0')}.${match[2] ?? ''}` : key;
}

/** Router path of a created application (namespace segment outside the control plane). */
export function createdApplicationPath(app: Application, controlPlaneNamespace: string | undefined): string {
  const name = encodeURIComponent(app.metadata.name ?? '');
  const ns = app.metadata.namespace;
  return ns && ns !== controlPlaneNamespace ? `/applications/${encodeURIComponent(ns)}/${name}` : `/applications/${name}`;
}

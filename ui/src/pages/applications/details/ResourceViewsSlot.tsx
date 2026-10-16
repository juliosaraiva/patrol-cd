/**
 * Contract between the application details shell (apps-core) and the resource views (tree, pods,
 * network, list, resource details drawer, diff, logs) built by the apps-resources workstream.
 *
 * Integration
 * -----------
 * The resource views publish `src/pages/applications/resources/contract.ts`, which exports three
 * lazy components (see `ResourcesContractModule` below for the exact props this shell relies on):
 *
 *   ResourceViewsSlot:  ComponentType<{ app; view?; onOpenApplication?; onSyncResources?; fullscreenLogs? }>
 *   AppDiffPanel:       ComponentType<{ app; onOpenResource?(key) }>
 *   ResourceLogsViewer: ComponentType<{ app; target; container?; fullscreen? }>
 *
 * This file discovers that module at build time (`import.meta.glob`, never a hard import, so the
 * details page builds and works before the module exists or while it is being changed) and mounts
 * it inside <Suspense>. Without it, a built-in resource table (live tree + status.resources), a
 * side-by-side diff and a "logs unavailable" notice are shown instead.
 * `ResourceViewsSlot.test.tsx` renders the real module (no mock) to keep both sides honest.
 *
 * What the shell passes (`ResourceViewsProps`)
 * --------------------------------------------
 * - `app`: the Application, live through the watch stream.
 * - `onSyncResources(keys)`: selection callback; opens the sync panel with those resource keys
 *   preselected (selective sync). The resource drawer shows its "Sync" button only when set.
 * - `onOpenApplication()`: selection callback for the application root node; opens the
 *   application details panel.
 * - `fullscreenLogs`: always set, because ApplicationLogsPage mounts `ResourceLogsViewer` on the
 *   full-screen logs route; the Logs tab then offers "Open full screen".
 * - Tree data is not passed as a prop: both sides read `useResourceTree(name, scope)`
 *   (src/api/queries), one shared cache entry keyed under the application's detail key, kept
 *   live by its watch stream and invalidated by the shell's mutations (sync, refresh, rollback).
 * - Resource selection is not passed as a prop either: it is the `node` URL parameter
 *   ("group/kind/namespace/name", `resourceKey()`), read and written by the resource views (and
 *   by the built-in fallback), so selections are deep-linkable and survive reloads.
 *
 * URL ownership
 * -------------
 * - The shell owns `panel` (sync | operation | history | details | diff | delete), `op`
 *   (operation shown), `appTab` (details drawer tab), `resources` (keys preselected in the sync
 *   panel) and the upstream `deploy` deep link.
 * - The resource views own `view`, `node`, `tab` (resource drawer tab) and their filter
 *   parameters (`name`, `kind`, `sync`, `health`, `namespace`, `orphaned`, `hooks`, `podSortMode`).
 *   Exception: when app view extensions are registered (API-123), ApplicationViews renders the
 *   switcher, owns `view` (built-in id or extension title) and passes the built-in view down.
 */
import { lazy, Suspense, useMemo, useState, type ComponentType, type ReactNode } from 'react';
import { useSearchParams } from 'react-router';
import { useResourceTree } from '../../../api/queries/applications';
import type { Application, AppScope, ResourceNode } from '../../../api/types';
import { Banner, Button, HealthStatus, KeyValueList, Mono, SearchInput, SkeletonText, SyncStatus, Table, type Column } from '../../../design';
import { ErrorBanner } from '../../../shell/errors';
import { resourceKey } from '../shared/app';
import styles from './Details.module.css';

export { resourceKey };

/** URL parameter holding the selected resource key (shared with the resource views). */
export const NODE_PARAM = 'node';

/** URL parameter holding the selected view: a built-in view id or an app view extension title. */
export const VIEW_PARAM = 'view';

/** Built-in resource views, in switcher order (resources/contract.ts RESOURCE_VIEW_IDS/LABELS). */
export const BUILT_IN_VIEWS = [
  { id: 'tree', label: 'Tree' },
  { id: 'pods', label: 'Pods' },
  { id: 'network', label: 'Network' },
  { id: 'list', label: 'List' },
] as const;

export type BuiltInViewId = (typeof BUILT_IN_VIEWS)[number]['id'];

/** Application annotation naming the default view (resources/contract.ts DEFAULT_VIEW_ANNOTATION). */
export const DEFAULT_VIEW_ANNOTATION = 'pref.cd.patrol.example/default-view';

export function isBuiltInView(value: string | null | undefined): value is BuiltInViewId {
  return BUILT_IN_VIEWS.some((v) => v.id === value);
}

/** Built-in view from `?view=`, else the default-view annotation, else the tree (as the resource views resolve it). */
export function resolveBuiltInView(app: Application, param: string | null | undefined): BuiltInViewId {
  if (isBuiltInView(param)) return param;
  const annotated = app.metadata.annotations?.[DEFAULT_VIEW_ANNOTATION];
  return isBuiltInView(annotated) ? annotated : 'tree';
}

export interface ResourceViewsProps {
  app: Application;
  /** Scope for application API calls (appNamespace, project). */
  scope: AppScope;
  /** Opens the sync panel with these resources preselected (selective sync). */
  onSyncResources: (keys: readonly string[]) => void;
  /** Opens the application details panel (e.g. from the tree's root node). */
  onOpenApplication: () => void;
  /**
   * Controlled view, set when the page renders its own view switcher (app view extensions);
   * the resource views then show only this view. Omitted: they render their own switcher.
   */
  view?: BuiltInViewId | undefined;
}

export interface ResourceDiffProps {
  app: Application;
  scope: AppScope;
  /** Called when the user opens one resource from the diff. */
  onOpenResource: (key: string) => void;
}

/** Resource whose logs are shown: a Pod, or a workload (logs of all its pods). */
export interface LogsTarget {
  group?: string | undefined;
  kind: string;
  namespace: string;
  name: string;
}

export interface ResourceLogsProps {
  app: Application;
  target: LogsTarget;
  /** Container to start with; the viewer picks the first container when omitted. */
  container?: string | undefined;
  fullscreen?: boolean | undefined;
}

/**
 * Structural view of resources/contract.ts: the props this shell passes. No type-level import,
 * so the shell builds without the module; ResourceViewsSlot.test.tsx checks assignability.
 */
export interface ResourcesContractModule {
  ResourceViewsSlot?: ComponentType<{
    app: Application;
    view?: BuiltInViewId | undefined;
    onOpenApplication?: (() => void) | undefined;
    onSyncResources?: ((keys: readonly string[]) => void) | undefined;
    /** The full-screen logs route (ApplicationLogsPage) mounts the viewer: the Logs tab may link to it. */
    fullscreenLogs?: boolean | undefined;
  }>;
  AppDiffPanel?: ComponentType<{ app: Application; onOpenResource?: ((key: string) => void) | undefined }>;
  ResourceLogsViewer?: ComponentType<ResourceLogsProps>;
}

const discovered = import.meta.glob<ResourcesContractModule>('../resources/contract.ts');
const loadModule = Object.values(discovered)[0];

/** True when the resource views module exists in this build. */
export const hasResourceViewsModule = loadModule !== undefined;

const LazyViews = loadModule
  ? lazy(async () => {
      const mod = await loadModule();
      const Views = mod.ResourceViewsSlot;
      const Component: ComponentType<ResourceViewsProps> = Views
        ? ({ app, view, onOpenApplication, onSyncResources }) => (
            <Views app={app} view={view} onOpenApplication={onOpenApplication} onSyncResources={onSyncResources} fullscreenLogs />
          )
        : FallbackResourceList;
      return { default: Component };
    })
  : null;

type DiffSlotProps = ResourceDiffProps & { fallback: ReactNode };

const LazyDiff = loadModule
  ? lazy(async () => {
      const mod = await loadModule();
      const Diff = mod.AppDiffPanel;
      const Component: ComponentType<DiffSlotProps> = Diff
        ? ({ app, onOpenResource }) => <Diff app={app} onOpenResource={onOpenResource} />
        : ({ fallback }) => <>{fallback}</>;
      return { default: Component };
    })
  : null;

const LazyLogs = loadModule
  ? lazy(async () => {
      const mod = await loadModule();
      const Viewer = mod.ResourceLogsViewer;
      // The module exports lazy components; a lazy() default must be a plain component.
      const Component: ComponentType<ResourceLogsProps> = Viewer ? (props) => <Viewer {...props} /> : LogsUnavailable;
      return { default: Component };
    })
  : null;

function Loading() {
  return (
    <div aria-busy="true">
      <SkeletonText lines={6} />
    </div>
  );
}

function LogsUnavailable() {
  return (
    <Banner tone="info" title="Log viewer not available in this build">
      Read the logs with <Mono>patrol app logs</Mono> or <Mono>kubectl logs</Mono> instead.
    </Banner>
  );
}

/** Mount point of the resource views inside the details page. */
export function ResourceViewsSlot(props: ResourceViewsProps) {
  if (LazyViews) {
    return (
      <Suspense fallback={<Loading />}>
        <LazyViews {...props} />
      </Suspense>
    );
  }
  return <FallbackResourceList {...props} />;
}

/** Diff from the resource views module when present; `fallback` otherwise. */
export function ResourceDiffSlot(props: DiffSlotProps) {
  if (LazyDiff) {
    return (
      <Suspense fallback={<Loading />}>
        <LazyDiff {...props} />
      </Suspense>
    );
  }
  return <>{props.fallback}</>;
}

/** Log viewer from the resource views module (full-screen logs route). */
export function ResourceLogsSlot(props: ResourceLogsProps) {
  if (LazyLogs) {
    return (
      <Suspense fallback={<Loading />}>
        <LazyLogs {...props} />
      </Suspense>
    );
  }
  return <LogsUnavailable />;
}

const PAGE = 100;

interface Row {
  key: string;
  group: string | undefined;
  version: string | undefined;
  kind: string;
  namespace: string | undefined;
  name: string;
  sync: string | undefined;
  health: string | undefined;
  healthMessage: string | undefined;
  wave: number | undefined;
}

/** Managed resources from status.resources, with health from the live tree when it is newer. */
function rowsOf(app: Application, nodes: readonly ResourceNode[] | undefined): Row[] {
  const byKey = new Map((nodes ?? []).map((n) => [resourceKey(n), n]));
  return (app.status?.resources ?? []).map((r) => {
    const key = resourceKey(r);
    const node = byKey.get(key);
    return {
      key,
      group: r.group,
      version: r.version,
      kind: r.kind ?? '',
      namespace: r.namespace,
      name: r.name ?? '',
      sync: r.status,
      health: node?.health?.status ?? r.health?.status,
      healthMessage: node?.health?.message ?? r.health?.message,
      wave: r.syncWave,
    };
  });
}

const COLUMNS: Column<Row>[] = [
  { id: 'kind', header: 'Kind', cell: (r) => r.kind, sortValue: (r) => r.kind },
  { id: 'namespace', header: 'Namespace', cell: (r) => (r.namespace ? <Mono>{r.namespace}</Mono> : '—'), sortValue: (r) => r.namespace },
  { id: 'name', header: 'Name', cell: (r) => <Mono>{r.name}</Mono>, sortValue: (r) => r.name },
  { id: 'sync', header: 'Sync', cell: (r) => <SyncStatus status={r.sync} />, sortValue: (r) => r.sync },
  { id: 'health', header: 'Health', cell: (r) => (r.health ? <HealthStatus status={r.health} /> : '—'), sortValue: (r) => r.health },
];

/** Built-in resource table used until the resource views module exists. */
function FallbackResourceList({ app, scope, onSyncResources }: ResourceViewsProps) {
  const tree = useResourceTree(app.metadata.name ?? '', scope);
  const [params, setParams] = useSearchParams();
  const selectedNode = params.get(NODE_PARAM);
  const onSelectNode = (key: string | null) =>
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      if (key) next.set(NODE_PARAM, key);
      else next.delete(NODE_PARAM);
      return next;
    });
  const [query, setQuery] = useState('');
  const [limit, setLimit] = useState(PAGE);
  const all = useMemo(() => rowsOf(app, tree.data?.nodes), [app, tree.data]);
  const rows = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? all.filter((r) => `${r.kind} ${r.namespace ?? ''} ${r.name}`.toLowerCase().includes(q)) : all;
  }, [all, query]);
  const selected = selectedNode ? all.find((r) => r.key === selectedNode) : undefined;
  return (
    <section className={styles.section} aria-labelledby="resources-heading">
      <div className={styles.sectionHeader}>
        <h2 id="resources-heading">Resources</h2>
        <SearchInput size="sm" aria-label="Filter resources by kind, namespace or name" value={query} onValueChange={setQuery} />
      </div>
      {tree.isError ? <ErrorBanner error={tree.error} onRetry={() => void tree.refetch()} title="The live resource tree could not be loaded" /> : null}
      {all.length === 0 ? (
        <p className={styles.muted}>No resources yet. They appear after the controller compares the source with the cluster; use Refresh if this persists.</p>
      ) : (
        <>
          <Table
            caption="Managed resources"
            columns={COLUMNS}
            rows={rows.slice(0, limit)}
            getRowKey={(r) => r.key}
            getRowLabel={(r) => `${r.kind} ${r.name}`}
            onRowActivate={(r) => onSelectNode(r.key)}
            empty="No resources match the filter."
          />
          {rows.length > limit ? (
            <p className={styles.muted}>
              Showing {limit} of {rows.length}.{' '}
              <Button size="sm" variant="ghost" onClick={() => setLimit(limit + PAGE)}>
                Show more
              </Button>
            </p>
          ) : null}
        </>
      )}
      {selected ? (
        <div className={styles.selected} role="region" aria-label={`Resource ${selected.kind} ${selected.name}`}>
          <KeyValueList
            items={[
              { key: 'kind', label: 'Kind', value: `${selected.group ? `${selected.group}/` : ''}${selected.version ?? ''} ${selected.kind}` },
              { key: 'name', label: 'Name', value: <Mono>{selected.name}</Mono> },
              { key: 'namespace', label: 'Namespace', value: selected.namespace ? <Mono>{selected.namespace}</Mono> : '' },
              { key: 'sync', label: 'Sync', value: <SyncStatus status={selected.sync} /> },
              { key: 'health', label: 'Health', value: selected.health ? <HealthStatus status={selected.health} /> : '' },
              { key: 'message', label: 'Health message', value: selected.healthMessage },
              { key: 'wave', label: 'Sync wave', value: selected.wave !== undefined ? String(selected.wave) : '' },
            ]}
          />
          <div className={styles.actions}>
            <Button size="sm" onClick={() => onSyncResources([selected.key])}>
              Sync this resource
            </Button>
            <Button size="sm" variant="ghost" onClick={() => onSelectNode(null)}>
              Close
            </Button>
          </div>
        </div>
      ) : null}
    </section>
  );
}

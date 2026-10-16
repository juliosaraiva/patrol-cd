/**
 * Slot contract between the application details page (workstream apps-core) and the
 * application resource views (workstream apps-resources, this folder).
 *
 * The details page owns the page chrome (header, action bar, summary panel, sync/rollback
 * panels). It mounts the resource views through the lazy components below, inside a
 * <Suspense> boundary:
 *
 *   import { Suspense } from 'react';
 *   import { ResourceViewsSlot, AppDiffPanel, ResourceLogsViewer } from './resources/contract';
 *
 *   <Suspense fallback={<Skeleton height={320} />}>
 *     <ResourceViewsSlot app={app} onOpenApplication={() => openSummary()} />
 *   </Suspense>
 *
 * - `ResourceViewsSlot` renders the Tree / Pods / Network / List views, their filters and the
 *   resource details drawer. It reads and writes the URL search params listed in
 *   RESOURCE_PARAMS (shareable deep links). By default it renders its own view switcher bound to
 *   `?view=`; a page that renders its own switcher passes `view` (and keeps `?view=` in sync
 *   itself) — the slot then shows only that view.
 * - `AppDiffPanel` lists every out-of-sync resource with its diff (the "Diff" action). Rendered
 *   inside a Drawer/Dialog by the details page. "Open details" on a resource sets `?node=` and
 *   `?tab=diff` unless `onOpenResource` is given.
 * - `ResourceLogsViewer` is the log viewer used by the drawer's Logs tab; the full-screen logs
 *   route (/applications/[:appNamespace/]:name/:namespace/:container/logs) can mount it with
 *   `fullscreen`, reading ?podName= (a Pod) or ?kind=&group=&resourceName= (aggregated workload
 *   logs) from the query and the namespace and container from the path ("-" means none).
 *   The drawer offers "Open full screen" (a link to that route) only when the details page
 *   passes `fullscreenLogs` to ResourceViewsSlot: pass it once that route renders the viewer,
 *   never while it is a placeholder.
 *
 * Only the names exported here are a stable contract; everything else in this folder is private.
 */
import { lazy } from 'react';
import type { Application } from '../../../api/types';

export type ResourceViewId = 'tree' | 'pods' | 'network' | 'list';
export const RESOURCE_VIEW_IDS: readonly ResourceViewId[] = ['tree', 'pods', 'network', 'list'];
export const RESOURCE_VIEW_LABELS: Readonly<Record<ResourceViewId, string>> = {
  tree: 'Tree',
  pods: 'Pods',
  network: 'Network',
  list: 'List',
};

/** Application annotations that pick the default view and pod grouping (API-092). */
export const DEFAULT_VIEW_ANNOTATION = 'pref.cd.patrol.example/default-view';
export const DEFAULT_POD_SORT_ANNOTATION = 'pref.cd.patrol.example/default-pod-sort';

/** URL search params used by the resource views. */
export const RESOURCE_PARAMS = {
  view: 'view',
  /** Selected resource: "group/kind/namespace/name" (empty group/namespace allowed). */
  node: 'node',
  /** Details drawer tab: summary | live | desired | diff | events | logs | terminal | actions. */
  tab: 'tab',
  /** Pods view grouping: node | parentResource | topLevelResource. */
  podSortMode: 'podSortMode',
  name: 'name',
  kind: 'kind',
  sync: 'sync',
  health: 'health',
  namespace: 'namespace',
  orphaned: 'orphaned',
  hooks: 'hooks',
} as const;

export type ResourceTabId = 'summary' | 'live' | 'desired' | 'diff' | 'events' | 'logs' | 'terminal' | 'actions';

export function isResourceViewId(value: string | null | undefined): value is ResourceViewId {
  return value === 'tree' || value === 'pods' || value === 'network' || value === 'list';
}

/** View from `?view=`, else the application's default-view annotation, else the tree. */
export function resolveResourceView(app: Application, param: string | null | undefined): ResourceViewId {
  if (isResourceViewId(param)) return param;
  const annotated = app.metadata.annotations?.[DEFAULT_VIEW_ANNOTATION];
  return isResourceViewId(annotated) ? annotated : 'tree';
}

/** Search string that opens a resource's details (e.g. for links from other pages). */
export function resourceSearch(key: string, tab?: ResourceTabId): string {
  const params = new URLSearchParams({ [RESOURCE_PARAMS.node]: key });
  if (tab) params.set(RESOURCE_PARAMS.tab, tab);
  return `?${params.toString()}`;
}

export interface ResourceViewsSlotProps {
  app: Application;
  /** Controlled view. When omitted the slot renders its own switcher bound to `?view=`. */
  view?: ResourceViewId | undefined;
  /** Activating the application root node (e.g. open the application summary panel). */
  onOpenApplication?: (() => void) | undefined;
  /** Opens the sync panel with these resource keys preselected; adds "Sync" to the details drawer. */
  onSyncResources?: ((keys: readonly string[]) => void) | undefined;
  /**
   * The full-screen logs route mounts ResourceLogsViewer: the Logs tab then links to it
   * ("Open full screen"). Default false, so the drawer never links to a placeholder page.
   */
  fullscreenLogs?: boolean | undefined;
}

export interface AppDiffPanelProps {
  app: Application;
  /** Open a resource's details; defaults to setting `?node=<key>&tab=diff`. */
  onOpenResource?: ((key: string) => void) | undefined;
}

export interface ResourceLogsViewerProps {
  app: Application;
  /** The resource whose logs are shown: a Pod, or a workload (logs of all its pods). */
  target: { group?: string | undefined; kind: string; namespace: string; name: string };
  /** Container to start with (defaults to the first container). */
  container?: string | undefined;
  /** Container names offered in the picker. Loaded from the live manifest when omitted. */
  containers?: readonly string[] | undefined;
  initContainers?: readonly string[] | undefined;
  /** Taller output for the full-screen route. */
  fullscreen?: boolean | undefined;
  /** Offer "Open full screen" (a link to the full-screen logs route). Ignored with `fullscreen`. */
  fullscreenLink?: boolean | undefined;
}

export const ResourceViewsSlot = lazy(() => import('./ResourceViews').then((m) => ({ default: m.ResourceViews })));
export const AppDiffPanel = lazy(() => import('./views/AppDiffPanel').then((m) => ({ default: m.AppDiffPanel })));
export const ResourceLogsViewer = lazy(() => import('./details/LogsViewer').then((m) => ({ default: m.ResourceLogsViewer })));

import { useCallback, useMemo, useState, type ReactNode } from 'react';
import { useSearchParams } from 'react-router';
import { useProject, useResourceTree } from '../../../api/queries';
import { Banner, Button, EmptyState, Skeleton, Tabs, type SortState } from '../../../design';
import { ErrorBanner } from '../../../shell/errors';
import {
  DEFAULT_POD_SORT_ANNOTATION,
  RESOURCE_PARAMS,
  RESOURCE_VIEW_IDS,
  RESOURCE_VIEW_LABELS,
  resolveResourceView,
  type ResourceTabId,
  type ResourceViewId,
  type ResourceViewsSlotProps,
} from './contract';
import { ResourceDetailsDrawer } from './details/ResourceDetailsDrawer';
import { applyFiltersToParams, createMatcher, filterOptions, filtersFromParams, hasNarrowingFilters, type ResourceFilters } from './model/filters';
import { isPodGroupMode, type PodGroupMode } from './model/pods';
import { buildResourceModel, type ResourceItem } from './model/resources';
import { NetworkView } from './views/NetworkView';
import { PodsView } from './views/PodsView';
import { ResourceFiltersBar } from './views/ResourceFilters';
import { ResourceListView } from './views/ResourceListView';
import { TreeView } from './views/TreeView';
import styles from './ResourceViews.module.css';

const STREAM_TEXT = {
  connecting: 'Connecting to live updates…',
  open: 'Live: the views update as resources change.',
  reconnecting: 'Live updates interrupted. Reconnecting…',
  closed: '',
} as const;

/**
 * Tree, Pods, Network and List views of an application's resources with shared filters and
 * the resource details drawer. Mounted by the application details page (see ./contract.ts).
 */
export function ResourceViews({ app, view: controlledView, onOpenApplication, onSyncResources, fullscreenLogs = false }: ResourceViewsSlotProps) {
  const name = app.metadata.name ?? '';
  const scope = useMemo(() => ({ appNamespace: app.metadata.namespace, project: app.spec.project }), [app.metadata.namespace, app.spec.project]);
  const [params, setParams] = useSearchParams();
  const tree = useResourceTree(name, scope);
  const project = useProject(app.spec.project);
  const [listSort, setListSort] = useState<SortState | null>(null);

  const paramString = params.toString();
  const filters = useMemo(() => filtersFromParams(new URLSearchParams(paramString)), [paramString]);
  // Offered when the project monitors orphaned resources; when the project cannot be read
  // (e.g. no permission) the toggle is offered and the tree simply has no orphaned nodes.
  const orphanedAvailable = project.isError || project.data?.spec.orphanedResources !== undefined;
  const includeOrphaned = orphanedAvailable && filters.showOrphaned;
  const model = useMemo(() => buildResourceModel(app, tree.data, includeOrphaned), [app, tree.data, includeOrphaned]);
  const matcher = useMemo(() => createMatcher(filters), [filters]);
  const filtering = hasNarrowingFilters(filters);
  const filtered = useMemo(() => model.items.filter(matcher), [model, matcher]);
  const options = useMemo(() => filterOptions(model.items), [model]);

  const view: ResourceViewId = controlledView ?? resolveResourceView(app, params.get(RESOURCE_PARAMS.view));
  const podParam = params.get(RESOURCE_PARAMS.podSortMode);
  const annotatedPodMode = app.metadata.annotations?.[DEFAULT_POD_SORT_ANNOTATION];
  const podMode: PodGroupMode = isPodGroupMode(podParam) ? podParam : isPodGroupMode(annotatedPodMode) ? annotatedPodMode : 'node';
  const selectedKey = params.get(RESOURCE_PARAMS.node);
  const tab = params.get(RESOURCE_PARAMS.tab);

  const update = useCallback(
    (mutate: (next: URLSearchParams) => void, replace: boolean) =>
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          mutate(next);
          return next;
        },
        { replace },
      ),
    [setParams],
  );

  const setFilters = useCallback(
    (next: ResourceFilters) =>
      setParams((prev) => applyFiltersToParams(prev, next), { replace: true }),
    [setParams],
  );
  const select = useCallback(
    (item: ResourceItem) =>
      update((p) => p.set(RESOURCE_PARAMS.node, item.key), false),
    [update],
  );
  const selectKey = useCallback((key: string) => update((p) => p.set(RESOURCE_PARAMS.node, key), false), [update]);
  const closeDrawer = useCallback(
    () =>
      update((p) => {
        p.delete(RESOURCE_PARAMS.node);
        p.delete(RESOURCE_PARAMS.tab);
      }, true),
    [update],
  );
  const setTab = useCallback((id: ResourceTabId) => update((p) => p.set(RESOURCE_PARAMS.tab, id), true), [update]);
  const setView = (id: ResourceViewId) => update((p) => p.set(RESOURCE_PARAMS.view, id), false);
  const setPodMode = (mode: PodGroupMode) => update((p) => p.set(RESOURCE_PARAMS.podSortMode, mode), true);

  const selectedId = selectedKey ? model.byKey.get(selectedKey)?.id : undefined;

  let body: ReactNode;
  if (tree.isPending) {
    body = (
      <div aria-busy="true" className={styles.loading}>
        <span className="sr-only">Loading resources</span>
        <Skeleton height={32} width="60%" />
        <Skeleton height={360} />
      </div>
    );
  } else if (tree.isError) {
    body = <ErrorBanner error={tree.error} onRetry={() => void tree.refetch()} title="Could not load the resource tree" />;
  } else if (model.items.length === 0) {
    body = (
      <EmptyState title="No resources yet" headingLevel={3}>
        <p>
          The application has not created any resources. Sync it to apply the manifests from its source, or check that the source path renders
          manifests (an empty directory or chart renders nothing).
        </p>
      </EmptyState>
    );
  } else {
    const viewContent = (id: ResourceViewId): ReactNode => {
      switch (id) {
        case 'tree':
          return (
            <TreeView
              app={app}
              model={model}
              matcher={matcher}
              filtering={filtering}
              selectedId={selectedId}
              onSelect={select}
              onOpenApplication={onOpenApplication}
            />
          );
        case 'pods':
          return <PodsView model={model} hosts={tree.data.hosts ?? []} matcher={matcher} mode={podMode} onModeChange={setPodMode} onSelect={select} />;
        case 'network':
          return <NetworkView appName={name} model={model} matcher={matcher} selectedId={selectedId} onSelect={select} />;
        case 'list':
          return <ResourceListView items={filtered} onSelect={select} sort={listSort} onSortChange={setListSort} />;
      }
    };
    body = (
      <>
        <ResourceFiltersBar
          filters={filters}
          onChange={setFilters}
          options={options}
          orphanedAvailable={orphanedAvailable}
          orphanedCount={model.orphanedCount}
        />
        <p className={styles.summary} role="status">
          {filtering ? `${filtered.length} of ${model.items.length} resources match the filters.` : `${model.items.length} resources.`}{' '}
          {STREAM_TEXT[tree.streamState]}
        </p>
        {filtering && filtered.length === 0 ? (
          <Banner
            tone="info"
            title="No resources match the filters"
            actions={
              <Button size="sm" onClick={() => setFilters({ ...filters, name: '', kinds: [], sync: [], health: [], namespaces: [] })}>
                Clear filters
              </Button>
            }
          >
            Change or clear the filters to see resources again.
          </Banner>
        ) : null}
        {controlledView ? (
          viewContent(view)
        ) : (
          <Tabs
            label="Resource views"
            value={view}
            onValueChange={(id) => setView(id as ResourceViewId)}
            items={RESOURCE_VIEW_IDS.map((id) => ({ id, label: RESOURCE_VIEW_LABELS[id], content: id === view ? viewContent(id) : null }))}
          />
        )}
      </>
    );
  }

  return (
    <section className={styles.root} aria-label={`Resources of ${name}`}>
      {body}
      {!tree.isPending && !tree.isError ? (
        <ResourceDetailsDrawer
          app={app}
          model={model}
          selectedKey={selectedKey}
          tab={tab}
          onTabChange={setTab}
          onClose={closeDrawer}
          onSelectResource={selectKey}
          onSyncResources={onSyncResources}
          fullscreenLogs={fullscreenLogs}
        />
      ) : null}
    </section>
  );
}

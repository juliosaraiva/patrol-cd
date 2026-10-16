import { lazy, Suspense, useMemo, useState, type ReactNode } from 'react';
import { useLocation } from 'react-router';
import { useCanIOnApp, useCanIOnAppResource, useLiveResource } from '../../../../api/hooks/apps-resources';
import { useApplicationEvents, useManagedResources, useSettings } from '../../../../api/queries';
import type { Application } from '../../../../api/types';
import { absoluteAppURL } from '../../../../app/session';
import { Badge, Button, CopyButton, Drawer, Mono, Skeleton, Tabs, type TabItem } from '../../../../design';
import type { ResourceTabId } from '../contract';
import { kindHasLogs } from '../model/kinds';
import { containerNames, parseJSONManifest } from '../model/manifest';
import { identifierOf, resourceKey, type ResourceItem, type ResourceModel } from '../model/resources';
import { KindIcon } from '../shared/KindIcon';
import { ActionsTab } from './ActionsTab';
import { DeleteResourceDialog } from './DeleteResourceDialog';
import { DiffTab } from './DiffTab';
import { EventsTab, warningCount } from './EventsTab';
import { ResourceLogsViewer } from './LogsViewer';
import { DesiredManifestTab, LiveManifestTab } from './ManifestTabs';
import { SummaryTab } from './SummaryTab';
import styles from './Details.module.css';

// xterm.js is loaded only when a terminal is opened.
const TerminalPanel = lazy(() => import('./TerminalPanel').then((m) => ({ default: m.TerminalPanel })));

export interface ResourceDetailsDrawerProps {
  app: Application;
  model: ResourceModel;
  /** Selected resource key ("group/kind/namespace/name"), or null when closed. */
  selectedKey: string | null;
  tab: string | null;
  onTabChange: (tab: ResourceTabId) => void;
  onClose: () => void;
  onSelectResource: (key: string) => void;
  onSyncResources?: ((keys: readonly string[]) => void) | undefined;
  /** The full-screen logs route renders the viewer; the Logs tab may link to it. */
  fullscreenLogs?: boolean | undefined;
}

const TAB_ORDER: ResourceTabId[] = ['summary', 'live', 'desired', 'diff', 'events', 'logs', 'terminal', 'actions'];

/** Which tabs a resource offers, given its state and the caller's permissions. */
export function availableTabs(item: ResourceItem, opts: { canLogs: boolean; canExec: boolean; execEnabled: boolean }): ResourceTabId[] {
  return TAB_ORDER.filter((tab) => {
    switch (tab) {
      case 'summary':
        return true;
      case 'live':
      case 'events':
      case 'actions':
        return !item.missing;
      case 'desired':
      case 'diff':
        return item.managed;
      case 'logs':
        return !item.missing && kindHasLogs(item.kind) && opts.canLogs;
      case 'terminal':
        return !item.missing && item.kind === 'Pod' && item.group === '' && opts.execEnabled && opts.canExec;
    }
  });
}

function DrawerBody({ app, item, model, tab, onTabChange, onSelectResource, onDeleted, onSyncResources, fullscreenLogs }: {
  app: Application;
  item: ResourceItem;
  model: ResourceModel;
  tab: string | null;
  onTabChange: (tab: ResourceTabId) => void;
  onSelectResource: (key: string) => void;
  onDeleted: () => void;
  onSyncResources?: ((keys: readonly string[]) => void) | undefined;
  fullscreenLogs: boolean;
}) {
  const name = app.metadata.name ?? '';
  const scope = useMemo(() => ({ appNamespace: app.metadata.namespace, project: app.spec.project }), [app.metadata.namespace, app.spec.project]);
  const location = useLocation();
  const settings = useSettings();
  const execEnabled = settings.data?.execEnabled === true;
  const canLogs = useCanIOnApp(app, 'logs', 'get', kindHasLogs(item.kind) && !item.missing);
  const canExec = useCanIOnApp(app, 'exec', 'create', execEnabled && item.kind === 'Pod' && !item.missing);
  const resource = useMemo(() => identifierOf(item), [item]);
  // Patching checks applications, update/<group>/<kind>/<namespace>/<name> (not plain update).
  const canEdit = useCanIOnAppResource(app, 'update', resource, !item.missing);
  const live = useLiveResource(name, item.missing ? null : resource, scope);
  const liveObject = useMemo(() => parseJSONManifest(live.data?.manifest), [live.data]);
  const managed = useManagedResources(name, scope);
  const diff = useMemo(() => managed.data?.items.find((d) => resourceKey(d) === item.key), [managed.data, item.key]);
  const events = useApplicationEvents(name, scope, item.missing ? {} : { resourceNamespace: item.namespace, resourceName: item.name, resourceUID: item.uid });
  const [deleting, setDeleting] = useState(false);
  const label = `${item.kind} ${item.name}`;

  const tabs = availableTabs(item, { canLogs: canLogs.data === true, canExec: canExec.data === true, execEnabled });
  const active: ResourceTabId = tabs.includes(tab as ResourceTabId) ? (tab as ResourceTabId) : 'summary';
  const warnings = warningCount(item.missing ? [] : events.data?.items);
  const names = containerNames(liveObject);

  const contentFor = (id: ResourceTabId): ReactNode => {
    switch (id) {
      case 'summary':
        return <SummaryTab app={app} item={item} model={model} live={liveObject} liveLoading={live.isPending && !item.missing} onSelectResource={onSelectResource} />;
      case 'live':
        return <LiveManifestTab app={app} item={item} live={live} canEdit={canEdit.data} permission={{ action: canEdit.action, object: canEdit.object }} />;
      case 'desired':
        return <DesiredManifestTab item={item} diff={diff} loading={managed.isPending} error={managed.error} onRetry={() => void managed.refetch()} />;
      case 'diff':
        return <DiffTab item={item} diff={diff} loading={managed.isPending} error={managed.error} onRetry={() => void managed.refetch()} />;
      case 'events':
        return <EventsTab label={label} events={events.data?.items} loading={events.isPending} error={events.error} onRetry={() => void events.refetch()} />;
      case 'logs':
        return (
          <ResourceLogsViewer
            app={app}
            target={{ group: item.group, kind: item.kind, namespace: item.namespace, name: item.name }}
            containers={live.isPending ? undefined : names.containers}
            initContainers={names.initContainers}
            fullscreenLink={fullscreenLogs}
          />
        );
      case 'terminal':
        return (
          <Suspense fallback={<Skeleton height={420} />}>
            <TerminalPanel app={app} namespace={item.namespace} pod={item.name} containers={names.containers} />
          </Suspense>
        );
      case 'actions':
        return <ActionsTab app={app} item={item} />;
    }
  };

  const TAB_LABELS: Record<ResourceTabId, ReactNode> = {
    summary: 'Summary',
    live: 'Live manifest',
    desired: 'Desired manifest',
    diff: 'Diff',
    events:
      warnings > 0 ? (
        <span className={styles.tabLabel}>
          Events <Badge tone="warning">{`${warnings} ${warnings === 1 ? 'warning' : 'warnings'}`}</Badge>
        </span>
      ) : (
        'Events'
      ),
    logs: 'Logs',
    terminal: 'Terminal',
    actions: 'Actions',
  };
  const items: TabItem[] = tabs.map((id) => ({ id, label: TAB_LABELS[id], content: id === active ? contentFor(id) : null }));

  return (
    <div className={styles.drawerBody}>
      <div className={styles.drawerToolbar}>
        <CopyButton value={absoluteAppURL(`${location.pathname}${location.search}`)} label={`Copy link to ${label}`} showLabel size="sm" />
        {item.managed && onSyncResources ? (
          <Button size="sm" onClick={() => onSyncResources([item.key])}>
            Sync
          </Button>
        ) : null}
        {!item.missing ? (
          <Button size="sm" variant="danger" onClick={() => setDeleting(true)}>
            Delete
          </Button>
        ) : null}
      </div>
      <Tabs label={`Details of ${label}`} items={items} value={active} onValueChange={(id) => onTabChange(id as ResourceTabId)} />
      {deleting ? (
        <DeleteResourceDialog
          open
          onClose={() => setDeleting(false)}
          app={app}
          item={item}
          onDeleted={() => {
            setDeleting(false);
            onDeleted();
          }}
        />
      ) : null}
    </div>
  );
}

/** Details of the selected resource in a drawer, deep-linked by ?node= and ?tab=. */
export function ResourceDetailsDrawer({
  app,
  model,
  selectedKey,
  tab,
  onTabChange,
  onClose,
  onSelectResource,
  onSyncResources,
  fullscreenLogs = false,
}: ResourceDetailsDrawerProps) {
  const item = selectedKey ? model.byKey.get(selectedKey) : undefined;
  const open = selectedKey !== null;
  const title = item ? (
    <span className={styles.drawerTitle}>
      <KindIcon kind={item.kind} size={20} />
      <span>{item.kind}</span>
      <Mono>{item.name}</Mono>
    </span>
  ) : (
    'Resource not found'
  );
  return (
    <Drawer
      open={open}
      onClose={onClose}
      width="960px"
      title={title}
      description={
        item
          ? [item.namespace ? `Namespace ${item.namespace}` : 'Cluster-scoped', [item.group, item.version].filter(Boolean).join('/') || 'core/v1'].join(' · ')
          : undefined
      }
    >
      {item ? (
        <DrawerBody
          key={item.key}
          app={app}
          item={item}
          model={model}
          tab={tab}
          onTabChange={onTabChange}
          onSelectResource={onSelectResource}
          onDeleted={onClose}
          onSyncResources={onSyncResources}
          fullscreenLogs={fullscreenLogs}
        />
      ) : (
        <div className={styles.tabBody}>
          <p>
            No resource <Mono>{selectedKey ?? ''}</Mono> exists in this application&apos;s resource tree. It may have been deleted or renamed, or it
            is an orphaned resource that is currently hidden.
          </p>
          <Button onClick={onClose}>Close details</Button>
        </div>
      )}
    </Drawer>
  );
}

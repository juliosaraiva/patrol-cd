import { useMemo, useState } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router';
import { isApiError } from '../../api/errors';
import { useCachedResourceTree } from '../../api/hooks/apps-core';
import { useApplication, useApplicationLinks, useRefreshApplication } from '../../api/queries/applications';
import type { Application, AppScope } from '../../api/types';
import { ButtonLink, EmptyState, Mono, PageHeader, Skeleton, SkeletonText, useToast } from '../../design';
import { StatusPanelExtensions, TopBarExtensionActions } from '../../extensions';
import { IconWarning } from '../../design/icons';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import { ErrorBanner } from '../../shell/errors';
import { ActionBar } from './details/ActionBar';
import { ApplicationViews } from './details/ApplicationViews';
import { AutoSyncDialog } from './details/AutoSyncDialog';
import { ConditionsPanel } from './details/ConditionsPanel';
import styles from './details/Details.module.css';
import { DiffPanel } from './details/DiffPanel';
import { NoticeBanner } from './details/NoticeBanner';
import { StatusPanel } from './details/StatusPanel';
import { DeleteDialog } from './delete/DeleteDialog';
import { HistoryPanel } from './history/HistoryPanel';
import { OperationPanel } from './operations/OperationPanel';
import { ApproveDeletionDialog, resourcesAwaitingApproval } from './operations/ApproveDeletionDialog';
import { TerminateDialog } from './operations/TerminateDialog';
import { AppDetailsPanel, parseDetailsTab, type DetailsTab } from './parameters/AppDetailsPanel';
import { appName, appScope, destinationLabel, isRefreshing, isRunningPhase, operationPhaseOf } from './shared/app';
import { ActionError } from './shared/errors';
import { applicationExternalLinks } from './shared/links';
import { SyncPanel } from './sync/SyncPanel';

/** Drawer or dialog open over the details page (URL param `panel`). */
export type DetailsPanel = 'sync' | 'operation' | 'history' | 'details' | 'diff' | 'delete';

const PANELS: readonly DetailsPanel[] = ['sync', 'operation', 'history', 'details', 'diff', 'delete'];

function parsePanel(value: string | null): DetailsPanel | null {
  return PANELS.find((p) => p === value) ?? null;
}

/** /applications/[:appNamespace/]:name — application details (parity API-092..100, API-114..117). */
export function Component() {
  const { name = '', appNamespace } = useParams();
  const scope = useMemo<AppScope | undefined>(() => (appNamespace ? { appNamespace } : undefined), [appNamespace]);
  const query = useApplication(name, scope, { watch: true });
  useDocumentTitle(name, 'Applications');

  if (query.isPending) {
    return (
      <>
        <PageHeader title={<Mono>{name}</Mono>} description="Loading application…" />
        <div aria-busy="true" className={styles.page}>
          <Skeleton height={32} width="60%" />
          <Skeleton height={120} />
          <SkeletonText lines={6} />
        </div>
      </>
    );
  }
  if (query.isError) {
    const error = query.error;
    return (
      <>
        <PageHeader title={<Mono>{name}</Mono>} description={appNamespace ? `Namespace ${appNamespace}` : undefined} />
        {isApiError(error) && (error.code === 'not_found' || error.code === 'permission_denied') ? (
          <EmptyState
            icon={<IconWarning size={24} />}
            title={error.code === 'not_found' ? 'Application not found' : 'You cannot view this application'}
            action={
              <ButtonLink variant="primary" to="/applications">
                Back to applications
              </ButtonLink>
            }
          >
            <p>
              {error.code === 'not_found'
                ? 'It may have been deleted, renamed, or it lives in another namespace.'
                : 'It may not exist, or your account lacks the applications get permission for its project. Ask an administrator for access.'}
            </p>
          </EmptyState>
        ) : (
          <ErrorBanner error={error} onRetry={() => void query.refetch()} />
        )}
      </>
    );
  }
  return <ApplicationDetails app={query.data} reloadApp={() => query.refetch().then((r) => r.data)} />;
}

function ApplicationDetails({ app, reloadApp }: { app: Application; reloadApp: () => Promise<Application | undefined> }) {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const { toast } = useToast();
  const name = appName(app);
  const scope = appScope(app);
  const links = useApplicationLinks(name, scope);
  const tree = useCachedResourceTree(name, scope);
  const refresh = useRefreshApplication();
  const [terminateOpen, setTerminateOpen] = useState(false);
  const [autoSyncOpen, setAutoSyncOpen] = useState(false);
  const [approveOpen, setApproveOpen] = useState(false);
  // `?deploy=<resource key>` (upstream deep link) opens the sync panel for that one resource.
  const deploy = params.get('deploy');
  const panel = deploy ? 'sync' : parsePanel(params.get('panel'));
  const running = isRunningPhase(operationPhaseOf(app));
  const preselected = deploy ? [deploy] : params.get('resources')?.split(',').filter(Boolean);

  const update = (mutate: (p: URLSearchParams) => void) =>
    setParams((prev) => {
      const next = new URLSearchParams(prev);
      mutate(next);
      return next;
    });
  const openPanel = (p: DetailsPanel, extra: Record<string, string> = {}) =>
    update((next) => {
      next.set('panel', p);
      for (const [k, v] of Object.entries(extra)) next.set(k, v);
    });
  const closePanel = () =>
    update((next) => {
      for (const key of ['panel', 'op', 'appTab', 'resources', 'deploy']) next.delete(key);
    });

  const doRefresh = async (type: 'normal' | 'hard') => {
    try {
      await refresh.mutateAsync({ name, scope, type });
      toast({ tone: 'success', title: type === 'hard' ? 'Hard refresh complete' : 'Refresh complete', description: `${name} was compared with its source again.` });
    } catch {
      // Rendered below.
    }
  };

  const d = app.spec.destination;
  return (
    <>
      <PageHeader
        title={<Mono>{name}</Mono>}
        description={
          <span className={styles.subtitle}>
            <span>Project {app.spec.project}</span>
            <span>
              Destination <Mono>{destinationLabel(app)}</Mono>
              {d.namespace ? (
                <>
                  {' / '}
                  <Mono>{d.namespace}</Mono>
                </>
              ) : null}
            </span>
          </span>
        }
      />
      <div className={styles.page}>
        <ActionBar
          app={app}
          refreshing={refresh.isPending || isRefreshing(app)}
          running={running}
          deepLinks={links.data?.items ?? []}
          externalLinks={applicationExternalLinks(app)}
          onSync={() => openPanel('sync')}
          onRefresh={(type) => void doRefresh(type)}
          onDiff={() => openPanel('diff')}
          onHistory={() => openPanel('history')}
          onDetails={() => openPanel('details')}
          onTerminate={() => setTerminateOpen(true)}
          onDelete={() => openPanel('delete')}
          onToggleAutoSync={() => setAutoSyncOpen(true)}
          awaitingDeletionApproval={resourcesAwaitingApproval(app).length > 0}
          onConfirmDeletion={() => setApproveOpen(true)}
          extensions={<TopBarExtensionActions application={app} tree={tree.data} />}
        />
        {refresh.isError ? <ActionError error={refresh.error} action="refresh" onRetry={() => void doRefresh('normal')} /> : null}
        <NoticeBanner app={app} />
        <StatusPanel app={app} onOpenOperation={() => openPanel('operation')} extensions={<StatusPanelExtensions application={app} tree={tree.data} />} />
        <ConditionsPanel app={app} />
        <ApplicationViews
          app={app}
          scope={scope}
          onSyncResources={(keys) =>
            update((next) => {
              // One drawer at a time: the resource drawer (node, tab) closes when the sync panel opens.
              next.delete('node');
              next.delete('tab');
              next.set('panel', 'sync');
              next.set('resources', keys.join(','));
            })
          }
          onOpenApplication={() => openPanel('details')}
        />
      </div>

      <SyncPanel app={app} open={panel === 'sync'} onClose={closePanel} preselected={preselected} />
      <OperationPanel
        app={app}
        open={panel === 'operation'}
        onClose={closePanel}
        selected={params.get('op') ?? undefined}
        onSelect={(op) => update((next) => next.set('op', op))}
      />
      <HistoryPanel app={app} open={panel === 'history'} onClose={closePanel} />
      <AppDetailsPanel
        app={app}
        open={panel === 'details'}
        onClose={closePanel}
        tab={parseDetailsTab(params.get('appTab'))}
        tree={tree.data}
        onTabChange={(tab: DetailsTab) => update((next) => next.set('appTab', tab))}
        reloadApp={reloadApp}
      />
      <DiffPanel
        app={app}
        open={panel === 'diff'}
        onClose={closePanel}
        onOpenResource={(key) =>
          update((next) => {
            next.delete('panel');
            next.set('node', key);
            next.set('tab', 'diff');
          })
        }
      />
      <DeleteDialog app={app} open={panel === 'delete'} onClose={closePanel} onDeleted={() => void navigate('/applications')} />
      <TerminateDialog app={app} open={terminateOpen} onClose={() => setTerminateOpen(false)} />
      <AutoSyncDialog app={app} open={autoSyncOpen} onClose={() => setAutoSyncOpen(false)} />
      <ApproveDeletionDialog app={app} open={approveOpen} onClose={() => setApproveOpen(false)} />
    </>
  );
}

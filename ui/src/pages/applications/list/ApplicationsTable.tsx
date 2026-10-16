import { useMemo, type RefObject } from 'react';
import { Link } from 'react-router';
import type { Application } from '../../../api/types';
import { HealthStatus, IconButton, Mono, OperationPhaseStatus, SyncStatus, Table, Timestamp, type Column } from '../../../design';
import { IconRefresh } from '../../../design/icons';
import { appDetailsPath, appKey, destinationLabel, healthOf, syncOf } from '../shared/app';
import { IconSync, IconTrash } from '../shared/icons';
import { FavoriteButton, NoticeIcon, SourceSummary } from './AppRow';
import type { ItemActions } from './ApplicationTiles';

export interface ApplicationsTableProps {
  apps: readonly Application[];
  selected: ReadonlySet<string>;
  onSelectionChange: (keys: Set<string>) => void;
  favorites: ReadonlySet<string>;
  onToggleFavorite: (key: string) => void;
  onOpen: (app: Application) => void;
  actions: ItemActions;
  containerRef: RefObject<HTMLDivElement | null>;
}

/** List (table) view; rows virtualize above 200 (parity API-088, keyboard per API-135). */
export function ApplicationsTable({ apps, selected, onSelectionChange, favorites, onToggleFavorite, onOpen, actions, containerRef }: ApplicationsTableProps) {
  const columns = useMemo<Column<Application>[]>(
    () => [
      {
        id: 'favorite',
        header: 'Favorite',
        hideHeader: true,
        width: '44px',
        cell: (app) => <FavoriteButton app={app} favorite={favorites.has(appKey(app))} onToggle={() => onToggleFavorite(appKey(app))} />,
      },
      {
        id: 'name',
        header: 'Name',
        cell: (app) => (
          <span>
            <Link to={appDetailsPath(app)}>
              <Mono>{app.metadata.name}</Mono>
            </Link>{' '}
            <NoticeIcon app={app} />
            {app.metadata.namespace ? <span className="sr-only"> in namespace {app.metadata.namespace}</span> : null}
          </span>
        ),
      },
      { id: 'project', header: 'Project', cell: (app) => app.spec.project },
      { id: 'health', header: 'Health', cell: (app) => <HealthStatus status={healthOf(app)} /> },
      { id: 'sync', header: 'Sync', cell: (app) => <SyncStatus status={syncOf(app)} /> },
      {
        id: 'operation',
        header: 'Last operation',
        cell: (app) => (app.status?.operationState?.phase ? <OperationPhaseStatus phase={app.status.operationState.phase} /> : '—'),
      },
      { id: 'source', header: 'Source', cell: (app) => <SourceSummary app={app} /> },
      {
        id: 'destination',
        header: 'Destination',
        cell: (app) => (
          <Mono>
            {destinationLabel(app)}
            {app.spec.destination.namespace ? ` / ${app.spec.destination.namespace}` : ''}
          </Mono>
        ),
      },
      { id: 'created', header: 'Created', cell: (app) => <Timestamp value={app.metadata.creationTimestamp} /> },
      {
        id: 'actions',
        header: 'Actions',
        hideHeader: true,
        cell: (app) => (
          <span style={{ display: 'inline-flex', gap: 'var(--space-2)' }}>
            <IconButton size="sm" icon={<IconSync />} onClick={() => actions.onSync(app)} aria-label={`Sync ${app.metadata.name ?? ''}`} />
            <IconButton
              size="sm"
              icon={<IconRefresh />}
              disabled={actions.refreshing === appKey(app)}
              aria-busy={actions.refreshing === appKey(app) || undefined}
              onClick={() => actions.onRefresh(app)}
              aria-label={`Refresh ${app.metadata.name ?? ''}`}
            />
            <IconButton size="sm" icon={<IconTrash />} onClick={() => actions.onDelete(app)} aria-label={`Delete ${app.metadata.name ?? ''}`} />
          </span>
        ),
      },
    ],
    [favorites, onToggleFavorite, actions],
  );
  return (
    <div ref={containerRef}>
      <Table
        caption="Applications"
        columns={columns}
        rows={apps}
        getRowKey={appKey}
        getRowLabel={(app) => app.metadata.name ?? ''}
        manualSort
        selectable
        selectedKeys={selected}
        onSelectionChange={onSelectionChange}
        onRowActivate={onOpen}
        height={apps.length > 200 ? 640 : undefined}
      />
    </div>
  );
}

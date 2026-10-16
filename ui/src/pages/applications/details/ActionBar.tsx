import type { ReactNode } from 'react';
import type { Application, LinkInfo } from '../../../api/types';
import { Button, Menu } from '../../../design';
import { IconChevronDown, IconRefresh } from '../../../design/icons';
import { isHttpURL } from '../../../lib/url';
import { appSources, isAutoSyncEnabled, isDeleting, syncOf } from '../shared/app';
import { IconDetails, IconDiff, IconHistory, IconLink, IconStop, IconSync, IconTrash } from '../shared/icons';
import { openExternal, type ExternalLink } from '../shared/links';
import styles from './Details.module.css';

export interface ActionBarProps {
  app: Application;
  refreshing: boolean;
  running: boolean;
  deepLinks: readonly LinkInfo[];
  externalLinks: readonly ExternalLink[];
  onSync: () => void;
  onRefresh: (type: 'normal' | 'hard') => void;
  onDiff: () => void;
  onHistory: () => void;
  onDetails: () => void;
  onTerminate: () => void;
  onDelete: () => void;
  onToggleAutoSync: () => void;
  /** Deletion is waiting for Delete=confirm approval. */
  awaitingDeletionApproval: boolean;
  onConfirmDeletion: () => void;
  /** Top-bar action extensions (API-125). */
  extensions?: ReactNode;
}

/** App-level actions (parity API-093). */
export function ActionBar({
  app,
  refreshing,
  running,
  deepLinks,
  externalLinks,
  onSync,
  onRefresh,
  onDiff,
  onHistory,
  onDetails,
  onTerminate,
  onDelete,
  onToggleAutoSync,
  awaitingDeletionApproval,
  onConfirmDeletion,
  extensions,
}: ActionBarProps) {
  const noSource = appSources(app).length === 0;
  const deleting = isDeleting(app);
  const synced = syncOf(app) === 'Synced';
  const links = [
    ...deepLinks.filter((l) => isHttpURL(l.url)).map((l) => ({ id: `deep:${l.title}:${l.url}`, label: l.title, url: l.url })),
    ...externalLinks.map((l) => ({ id: `ext:${l.url}`, label: l.title ?? l.url, url: l.url })),
  ];
  return (
    <div role="group" aria-label="Application actions" className={styles.actionBar}>
      <Button variant="primary" icon={<IconSync />} onClick={onSync} disabled={noSource || deleting}>
        Sync
      </Button>
      <Button icon={<IconRefresh />} onClick={() => onRefresh('normal')} loading={refreshing}>
        Refresh
      </Button>
      <Button onClick={() => onRefresh('hard')} disabled={refreshing}>
        Hard refresh
      </Button>
      <Button icon={<IconDiff />} onClick={onDiff} disabled={noSource || synced} aria-describedby={synced ? 'diff-disabled-reason' : undefined}>
        Diff
      </Button>
      {synced ? (
        <span id="diff-disabled-reason" className="sr-only">
          Disabled: the application is synced, so there is nothing to compare.
        </span>
      ) : null}
      <Button icon={<IconHistory />} onClick={onHistory}>
        History and rollback
      </Button>
      <Button icon={<IconDetails />} onClick={onDetails}>
        Details
      </Button>
      <Button onClick={onToggleAutoSync} disabled={noSource || deleting}>
        {isAutoSyncEnabled(app) ? 'Disable auto-sync' : 'Enable auto-sync'}
      </Button>
      {links.length > 0 ? (
        <Menu
          label="Links"
          trigger={(props) => (
            <Button {...props} icon={<IconLink />}>
              Links <IconChevronDown size={12} />
            </Button>
          )}
          items={links.map((l) => ({ id: l.id, label: l.label, onSelect: () => openExternal(l.url) }))}
        />
      ) : null}
      {running ? (
        <Button variant="danger" icon={<IconStop />} onClick={onTerminate}>
          Terminate operation
        </Button>
      ) : null}
      {extensions}
      <span className={styles.actionSpacer} />
      {deleting && awaitingDeletionApproval ? (
        <Button variant="danger" icon={<IconTrash />} onClick={onConfirmDeletion}>
          Confirm deletion
        </Button>
      ) : (
        <Button variant="ghost" icon={<IconTrash />} onClick={onDelete} disabled={deleting}>
          {deleting ? 'Deleting…' : 'Delete'}
        </Button>
      )}
    </div>
  );
}

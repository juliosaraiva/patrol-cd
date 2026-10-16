import type { RefObject } from 'react';
import { Link } from 'react-router';
import type { Application } from '../../../api/types';
import { Button, HealthStatus, Mono, OperationPhaseStatus, SyncStatus } from '../../../design';
import { IconRefresh } from '../../../design/icons';
import { appDetailsPath, appKey, destinationLabel, healthOf, syncOf } from '../shared/app';
import { IconSync, IconTrash } from '../shared/icons';
import { FavoriteButton, LabelBadges, LinksMenu, NoticeIcon, SourceSummary } from './AppRow';
import styles from './List.module.css';

export interface ItemActions {
  onSync: (app: Application) => void;
  onRefresh: (app: Application) => void;
  onDelete: (app: Application) => void;
  /** Key of the application being refreshed from the list, if any. */
  refreshing: string | null;
}

export interface ApplicationTilesProps {
  apps: readonly Application[];
  selected: ReadonlySet<string>;
  onToggleSelect: (key: string, on: boolean) => void;
  favorites: ReadonlySet<string>;
  onToggleFavorite: (key: string) => void;
  /** Keyboard cursor (index within `apps`), -1 for none. */
  cursor: number;
  onCursorChange: (index: number) => void;
  actions: ItemActions;
  containerRef: RefObject<HTMLUListElement | null>;
}

/** Tiles view (parity API-088, keyboard per API-134). */
export function ApplicationTiles({ apps, selected, onToggleSelect, favorites, onToggleFavorite, cursor, onCursorChange, actions, containerRef }: ApplicationTilesProps) {
  return (
    <ul ref={containerRef} className={styles.tiles} aria-label="Applications">
      {apps.map((app, i) => {
        const key = appKey(app);
        const name = app.metadata.name ?? '';
        const checked = selected.has(key);
        const op = app.status?.operationState?.phase;
        return (
          <li key={key} data-tile-index={i}>
            <article
              className={[styles.tile, i === cursor ? styles.tileSelected : undefined, checked ? styles.tileChecked : undefined].filter(Boolean).join(' ')}
              data-health={healthOf(app)}
              aria-label={name}
              aria-current={i === cursor ? 'true' : undefined}
            >
              <div className={styles.tileHeader}>
                <input
                  type="checkbox"
                  className={styles.checkbox}
                  aria-label={`Select ${name}`}
                  checked={checked}
                  onChange={(e) => onToggleSelect(key, e.target.checked)}
                />
                <h2 className={styles.tileTitle}>
                  <Link to={appDetailsPath(app)} data-tile-link="" onFocus={() => onCursorChange(i)}>
                    <Mono>{name}</Mono>
                  </Link>
                  {app.metadata.namespace ? <span className={styles.tileNamespace}>{app.metadata.namespace}</span> : null}
                </h2>
                <NoticeIcon app={app} />
                <LinksMenu app={app} />
                <FavoriteButton app={app} favorite={favorites.has(key)} onToggle={() => onToggleFavorite(key)} />
              </div>
              <div className={styles.statusRow}>
                <HealthStatus status={healthOf(app)} />
                <SyncStatus status={syncOf(app)} />
                {op ? <OperationPhaseStatus phase={op} /> : null}
              </div>
              <dl className={styles.facts}>
                <dt>Project</dt>
                <dd>{app.spec.project}</dd>
                <dt>Source</dt>
                <dd>
                  <SourceSummary app={app} />
                </dd>
                <dt>Destination</dt>
                <dd>
                  <Mono>{destinationLabel(app)}</Mono>
                  {app.spec.destination.namespace ? (
                    <>
                      {' / '}
                      <Mono>{app.spec.destination.namespace}</Mono>
                    </>
                  ) : null}
                </dd>
                {Object.keys(app.metadata.labels ?? {}).length > 0 ? (
                  <>
                    <dt>Labels</dt>
                    <dd>
                      <LabelBadges app={app} />
                    </dd>
                  </>
                ) : null}
              </dl>
              <div className={styles.tileActions}>
                <Button size="sm" icon={<IconSync />} onClick={() => actions.onSync(app)}>
                  Sync<span className="sr-only"> {name}</span>
                </Button>
                <Button size="sm" icon={<IconRefresh />} loading={actions.refreshing === key} onClick={() => actions.onRefresh(app)}>
                  Refresh<span className="sr-only"> {name}</span>
                </Button>
                <Button size="sm" variant="ghost" icon={<IconTrash />} onClick={() => actions.onDelete(app)}>
                  Delete<span className="sr-only"> {name}</span>
                </Button>
              </div>
            </article>
          </li>
        );
      })}
    </ul>
  );
}

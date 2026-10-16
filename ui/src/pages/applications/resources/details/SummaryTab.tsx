import type { Application } from '../../../../api/types';
import { useResourceLinks } from '../../../../api/queries';
import { Badge, HealthStatus, KeyValueList, Mono, Skeleton, SyncStatus, Timestamp, type KeyValueItem } from '../../../../design';
import { IconExternal } from '../../../../design/icons';
import { isHttpURL } from '../../../../lib/url';
import { metadataMap } from '../model/manifest';
import { identifierOf, ownerChain, type ResourceItem, type ResourceModel } from '../model/resources';
import { KindIcon } from '../shared/KindIcon';
import styles from './Details.module.css';

export interface SummaryTabProps {
  app: Application;
  item: ResourceItem;
  model: ResourceModel;
  live: Record<string, unknown> | null;
  liveLoading: boolean;
  onSelectResource: (key: string) => void;
}

function creationTimestamp(live: Record<string, unknown> | null): string | undefined {
  const metadata = live?.metadata;
  if (typeof metadata !== 'object' || metadata === null) return undefined;
  const value = (metadata as Record<string, unknown>).creationTimestamp;
  return typeof value === 'string' ? value : undefined;
}

function MapList({ map, label }: { map: Record<string, string>; label: string }) {
  const entries = Object.entries(map).sort(([a], [b]) => a.localeCompare(b));
  if (entries.length === 0) return <p className={styles.muted}>No {label.toLowerCase()}.</p>;
  return (
    <ul className={styles.mapList} aria-label={label}>
      {entries.map(([k, v]) => (
        <li key={k}>
          <Mono>{k}</Mono>
          <span aria-hidden="true">=</span>
          <span className="sr-only"> equals </span>
          <Mono className={styles.mapValue}>{v}</Mono>
        </li>
      ))}
    </ul>
  );
}

/** Fields, status, owner chain, labels, annotations, links, images and info items of a resource. */
export function SummaryTab({ app, item, model, live, liveLoading, onSelectResource }: SummaryTabProps) {
  const name = app.metadata.name ?? '';
  const scope = { appNamespace: app.metadata.namespace, project: app.spec.project };
  const links = useResourceLinks(name, item.missing ? null : identifierOf(item), scope);
  const chain = ownerChain(item, model.byId);
  const created = item.createdAt ?? creationTimestamp(live);

  const fields: KeyValueItem[] = [
    { key: 'kind', label: 'Kind', value: item.kind },
    { key: 'apiVersion', label: 'API version', value: <Mono>{[item.group, item.version].filter(Boolean).join('/') || 'v1'}</Mono> },
    { key: 'name', label: 'Name', value: <Mono>{item.name}</Mono> },
    { key: 'namespace', label: 'Namespace', value: item.namespace ? <Mono>{item.namespace}</Mono> : 'Cluster-scoped' },
    { key: 'created', label: 'Created', value: <Timestamp value={created} fallback={item.missing ? 'Not created' : '—'} /> },
    {
      key: 'health',
      label: 'Health',
      value: item.health?.status ? (
        <span className={styles.stack}>
          <HealthStatus status={item.health.status} />
          {item.health.message ? <span className={styles.message}>{item.health.message}</span> : null}
        </span>
      ) : (
        'No health assessment'
      ),
    },
    {
      key: 'sync',
      label: 'Sync status',
      value: item.managed ? <SyncStatus status={item.sync} /> : 'Not managed directly (created by another resource)',
    },
  ];
  if (item.syncWave !== undefined) fields.push({ key: 'wave', label: 'Sync wave', value: String(item.syncWave) });
  if (item.hook) fields.push({ key: 'hook', label: 'Hook', value: <Badge tone="info">Sync hook</Badge> });
  if (item.requiresPruning) {
    fields.push({
      key: 'prune',
      label: 'Pruning',
      value: <span className={styles.message}>No longer in the source. A sync with pruning enabled deletes it.</span>,
    });
  }
  if (item.orphaned) {
    fields.push({
      key: 'orphaned',
      label: 'Orphaned',
      value: <span className={styles.message}>In the destination namespace but not managed by any application.</span>,
    });
  }
  if (item.uid) fields.push({ key: 'uid', label: 'UID', value: <Mono>{item.uid}</Mono> });
  for (const info of item.info) {
    if (info.name) fields.push({ key: `info:${info.name}`, label: info.name, value: info.value ?? '—' });
  }

  const linkItems = (links.data?.items ?? []).filter((l) => isHttpURL(l.url));

  return (
    <div className={styles.tabBody}>
      {item.missing ? (
        <p className={styles.callout}>
          This resource is declared in the source but does not exist in the cluster. Sync the application to create it.
        </p>
      ) : null}
      <KeyValueList items={fields} />

      <section className={styles.section} aria-labelledby="summary-owners">
        <h3 id="summary-owners" className={styles.sectionTitle}>
          Owner chain
        </h3>
        <ol className={styles.chain}>
          <li>
            <span className={styles.chainItem}>
              <KindIcon kind="Application" size={16} />
              Application <Mono>{name}</Mono>
            </span>
          </li>
          {[...chain].reverse().map((owner) => (
            <li key={owner.key}>
              <button type="button" className={styles.chainLink} onClick={() => onSelectResource(owner.key)}>
                <KindIcon kind={owner.kind} size={16} />
                {owner.kind} <Mono>{owner.name}</Mono>
              </button>
            </li>
          ))}
          <li aria-current="true">
            <span className={styles.chainItem}>
              <KindIcon kind={item.kind} size={16} />
              {item.kind} <Mono>{item.name}</Mono>
            </span>
          </li>
        </ol>
      </section>

      {item.images.length > 0 ? (
        <section className={styles.section} aria-labelledby="summary-images">
          <h3 id="summary-images" className={styles.sectionTitle}>
            Images
          </h3>
          <ul className={styles.plainList}>
            {item.images.map((image) => (
              <li key={image}>
                <Mono>{image}</Mono>
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {linkItems.length > 0 ? (
        <section className={styles.section} aria-labelledby="summary-links">
          <h3 id="summary-links" className={styles.sectionTitle}>
            Links
          </h3>
          <ul className={styles.plainList}>
            {linkItems.map((link) => (
              <li key={`${link.title}-${link.url}`}>
                <a href={link.url} target="_blank" rel="noopener noreferrer" className={styles.externalLink}>
                  {link.title}
                  <IconExternal size={12} />
                  <span className="sr-only"> (opens in a new tab)</span>
                </a>
                {link.description ? <span className={styles.message}> — {link.description}</span> : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {!item.missing ? (
        <>
          <section className={styles.section} aria-labelledby="summary-labels">
            <h3 id="summary-labels" className={styles.sectionTitle}>
              Labels
            </h3>
            {liveLoading ? <Skeleton height={16} width="60%" /> : <MapList map={metadataMap(live, 'labels')} label="Labels" />}
          </section>
          <section className={styles.section} aria-labelledby="summary-annotations">
            <h3 id="summary-annotations" className={styles.sectionTitle}>
              Annotations
            </h3>
            {liveLoading ? <Skeleton height={16} width="60%" /> : <MapList map={metadataMap(live, 'annotations')} label="Annotations" />}
          </section>
        </>
      ) : null}
    </div>
  );
}

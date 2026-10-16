import type { UseQueryResult } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { useNotificationServices, useNotificationTemplates, useNotificationTriggers } from '../../api/queries/resources';
import { Badge, Mono, SkeletonText } from '../../design';
import { ErrorBanner } from '../../shell/errors';
import { CATALOG_TRIGGER_DESCRIPTIONS } from './subscriptions';
import styles from './Notifications.module.css';

/** Trigger items may carry more than a name if the server provides it (forward compatible). */
interface TriggerItem {
  name: string;
  description?: unknown;
}

function describeTrigger(item: TriggerItem): { text: string; fromCatalog: boolean } | null {
  if (typeof item.description === 'string' && item.description.trim() !== '') return { text: item.description, fromCatalog: false };
  const catalog = CATALOG_TRIGGER_DESCRIPTIONS[item.name];
  return catalog ? { text: catalog, fromCatalog: true } : null;
}

function Section<T extends { name: string }>({
  id,
  title,
  description,
  query,
  empty,
  render,
}: {
  id: string;
  title: string;
  description: ReactNode;
  query: UseQueryResult<{ items?: T[] }>;
  empty: ReactNode;
  render: (items: T[]) => ReactNode;
}) {
  const items = [...(query.data?.items ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  return (
    <section aria-labelledby={id} className={styles.catalog} aria-busy={query.isPending || undefined}>
      <div className={styles.editorHeader}>
        <h2 id={id} className={styles.heading}>
          {title}
        </h2>
        {query.isSuccess ? <Badge>{`${items.length} configured`}</Badge> : null}
      </div>
      <p className={styles.muted}>{description}</p>
      {query.isPending ? <SkeletonText lines={3} /> : null}
      {query.isError ? <ErrorBanner error={query.error} onRetry={() => void query.refetch()} title={`${title} could not be loaded`} /> : null}
      {query.isSuccess ? items.length === 0 ? <p className={styles.muted}>{empty}</p> : render(items) : null}
    </section>
  );
}

/**
 * Read-only catalog of the notification services, triggers and templates configured in
 * patrol-notifications-cm (GET /api/v1/notifications/*), used when subscribing applications.
 */
export function NotificationsCatalog() {
  const services = useNotificationServices();
  const triggers = useNotificationTriggers();
  const templates = useNotificationTemplates();
  return (
    <div className={styles.sections}>
      <Section
        id="notifications-services"
        title="Services"
        description="Destinations messages are delivered to, from service.<type>[.<name>] keys. Subscriptions name a service."
        query={services}
        empty={
          <>
            No services are configured. Add a <Mono>service.&lt;type&gt;</Mono> key (for example <Mono>service.slack</Mono>) to{' '}
            <Mono>patrol-notifications-cm</Mono>, with its token in <Mono>patrol-notifications-secret</Mono>.
          </>
        }
        render={(items) => (
          <ul className={styles.names} aria-label="Services">
            {items.map((s) => (
              <li key={s.name}>
                <Mono>{s.name}</Mono>
              </li>
            ))}
          </ul>
        )}
      />
      <Section
        id="notifications-triggers"
        title="Triggers"
        description="Conditions evaluated on every application change; when one turns true, the templates it sends are delivered to subscribed services."
        query={triggers}
        empty={
          <>
            No triggers are configured. Define <Mono>trigger.&lt;name&gt;</Mono> keys in <Mono>patrol-notifications-cm</Mono> or enable the
            default catalog.
          </>
        }
        render={(items: TriggerItem[]) => (
          <ul className={styles.triggers} aria-label="Triggers">
            {items.map((t) => {
              const description = describeTrigger(t);
              return (
                <li key={t.name}>
                  <Mono>{t.name}</Mono>
                  {description ? (
                    <span className={styles.muted}>
                      {description.text}
                      {description.fromCatalog ? ' (default catalog description)' : null}
                    </span>
                  ) : (
                    <span className={styles.muted}>Custom trigger</span>
                  )}
                </li>
              );
            })}
          </ul>
        )}
      />
      <Section
        id="notifications-templates"
        title="Templates"
        description="Message bodies (with per-service sections) that triggers send."
        query={templates}
        empty={
          <>
            No templates are configured. Define <Mono>template.&lt;name&gt;</Mono> keys in <Mono>patrol-notifications-cm</Mono>.
          </>
        }
        render={(items) => (
          <ul className={styles.names} aria-label="Templates">
            {items.map((t) => (
              <li key={t.name}>
                <Mono>{t.name}</Mono>
              </li>
            ))}
          </ul>
        )}
      />
    </div>
  );
}

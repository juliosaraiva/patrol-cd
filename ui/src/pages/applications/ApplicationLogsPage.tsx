import { useMemo } from 'react';
import { useParams, useSearchParams } from 'react-router';
import { isApiError } from '../../api/errors';
import { useApplication } from '../../api/queries/applications';
import type { AppScope } from '../../api/types';
import { ButtonLink, EmptyState, Mono, PageHeader, Skeleton } from '../../design';
import { IconWarning } from '../../design/icons';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import { ErrorBanner } from '../../shell/errors';
import styles from './details/Details.module.css';
import { ResourceLogsSlot, type LogsTarget } from './details/ResourceViewsSlot';
import { resourceKey } from './shared/app';

/** "-" stands for "not set" in the route's path segments (the viewer builds it that way). */
function segment(value: string | undefined): string | undefined {
  return value && value !== '-' ? value : undefined;
}

/**
 * Target from the query string the log viewer's "Open full screen" link builds: `podName=` for a
 * Pod, or `kind=&group=&resourceName=` for the aggregated logs of a workload's pods.
 */
export function logsTargetFrom(params: URLSearchParams, namespace: string): LogsTarget | null {
  const podName = params.get('podName');
  if (podName) return { kind: 'Pod', namespace, name: podName };
  const kind = params.get('kind');
  const name = params.get('resourceName');
  const group = params.get('group');
  if (kind && name) return { group: group === null || group === '' ? undefined : group, kind, namespace, name };
  return null;
}

/**
 * /applications/[:appNamespace/]:name/:namespace/:container/logs — full-screen logs (API-096).
 * Mounts the resource views' log viewer (ResourceLogsSlot) in its full-screen mode.
 */
export function Component() {
  const { name = '', appNamespace, namespace: nsParam, container: containerParam } = useParams();
  const [params] = useSearchParams();
  const namespace = segment(nsParam) ?? '';
  const container = segment(containerParam);
  const scope = useMemo<AppScope | undefined>(() => (appNamespace ? { appNamespace } : undefined), [appNamespace]);
  const query = useApplication(name, scope);
  const target = logsTargetFrom(params, namespace);
  useDocumentTitle(target ? `Logs of ${target.name}` : 'Logs', name, 'Applications');

  // Back to the details route this page hangs under (same namespace segment as the URL).
  const back = (search = '') => `/applications/${appNamespace ? `${encodeURIComponent(appNamespace)}/` : ''}${encodeURIComponent(name)}${search ? `?${search}` : ''}`;
  const resourceSearch = target ? new URLSearchParams({ node: resourceKey(target), tab: 'logs' }).toString() : '';

  const header = (
    <PageHeader
      title={target ? `Logs of ${target.kind} ${target.name}` : 'Logs'}
      description={
        <span className={styles.subtitle}>
          <span>
            Application <Mono>{name}</Mono>
          </span>
          {namespace ? (
            <span>
              Namespace <Mono>{namespace}</Mono>
            </span>
          ) : null}
          {container ? (
            <span>
              Container <Mono>{container}</Mono>
            </span>
          ) : null}
        </span>
      }
      actions={
        <ButtonLink to={back(resourceSearch)} variant="secondary">
          Back to {target ? 'resource' : 'application'}
        </ButtonLink>
      }
    />
  );

  if (!target) {
    return (
      <>
        {header}
        <EmptyState
          icon={<IconWarning size={24} />}
          title="No pod or workload selected"
          action={
            <ButtonLink variant="primary" to={back()}>
              Open the application
            </ButtonLink>
          }
        >
          <p>
            This page needs <Mono>?podName=</Mono>, or <Mono>?kind=</Mono> and <Mono>?resourceName=</Mono>. Open a Pod or workload in the
            application&apos;s resource views, go to its Logs tab and choose Open full screen.
          </p>
        </EmptyState>
      </>
    );
  }
  if (query.isPending) {
    return (
      <>
        {header}
        <div aria-busy="true" className={styles.page}>
          <span className="sr-only">Loading application</span>
          <Skeleton height={32} width="60%" />
          <Skeleton height={480} />
        </div>
      </>
    );
  }
  if (query.isError) {
    const error = query.error;
    return (
      <>
        {header}
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
                : 'Your account lacks the applications get permission for its project. Ask an administrator for access.'}
            </p>
          </EmptyState>
        ) : (
          <ErrorBanner error={error} onRetry={() => void query.refetch()} />
        )}
      </>
    );
  }
  return (
    <>
      {header}
      <div className={styles.page}>
        <ResourceLogsSlot key={`${resourceKey(target)}/${container ?? ''}`} app={query.data} target={target} container={container} fullscreen />
      </div>
    </>
  );
}

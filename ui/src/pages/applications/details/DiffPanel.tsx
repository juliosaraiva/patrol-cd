import { useManagedResources } from '../../../api/queries/applications';
import type { Application, ResourceDiff } from '../../../api/types';
import { CodeBlock, Drawer, EmptyState, Mono, SkeletonText } from '../../../design';
import { ErrorBanner } from '../../../shell/errors';
import { appName, appScope } from '../shared/app';
import styles from './Details.module.css';
import { ResourceDiffSlot } from './ResourceViewsSlot';

function pretty(state: string | undefined): string {
  if (!state || state === 'null') return '';
  try {
    return JSON.stringify(JSON.parse(state), null, 2);
  } catch {
    return state;
  }
}

function BasicDiff({ app }: { app: Application }) {
  const managed = useManagedResources(appName(app), appScope(app));
  if (managed.isPending) {
    return (
      <div aria-busy="true">
        <SkeletonText lines={6} />
      </div>
    );
  }
  if (managed.isError) return <ErrorBanner error={managed.error} onRetry={() => void managed.refetch()} title="Differences could not be loaded" />;
  const modified = managed.data.items.filter((d: ResourceDiff) => d.modified);
  if (modified.length === 0) {
    return (
      <EmptyState title="No differences" headingLevel={3}>
        <p>The live state matches the desired state for every managed resource.</p>
      </EmptyState>
    );
  }
  return (
    <div className={styles.stack}>
      {modified.map((d) => {
        const label = `${d.kind ?? ''} ${d.namespace ? `${d.namespace}/` : ''}${d.name ?? ''}`;
        const live = pretty(d.normalizedLiveState ?? d.liveState);
        const desired = pretty(d.predictedLiveState ?? d.targetState);
        return (
          <section key={`${d.group ?? ''}/${label}`} className={styles.diffItem} aria-label={label}>
            <h3>
              <Mono>{label}</Mono>
            </h3>
            <div className={styles.diffPanes}>
              {live ? <CodeBlock code={live} title="Live" label={`Live state of ${label}`} maxHeight={320} /> : <p className={styles.muted}>Missing from the cluster.</p>}
              {desired ? (
                <CodeBlock code={desired} title="Desired" label={`Desired state of ${label}`} maxHeight={320} />
              ) : (
                <p className={styles.muted}>Not in the source (would be pruned).</p>
              )}
            </div>
          </section>
        );
      })}
    </div>
  );
}

/** Diff drawer: the resources module's diff viewer when present, a side-by-side fallback otherwise. */
export function DiffPanel({
  app,
  open,
  onClose,
  onOpenResource,
}: {
  app: Application;
  open: boolean;
  onClose: () => void;
  onOpenResource: (key: string) => void;
}) {
  return (
    <Drawer open={open} onClose={onClose} title={`Differences in ${appName(app)}`} description="Live state compared with the desired state rendered from the source." width="1100px">
      {open ? <ResourceDiffSlot app={app} scope={appScope(app)} onOpenResource={onOpenResource} fallback={<BasicDiff app={app} />} /> : null}
    </Drawer>
  );
}

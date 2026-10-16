import type { ResourceDiff } from '../../../../api/types';
import { EmptyState, Skeleton, Switch } from '../../../../design';
import { ErrorBanner } from '../../../../shell/errors';
import { diffSides } from '../model/diffItems';
import { usePref } from '../model/prefs';
import type { ResourceItem } from '../model/resources';
import { DiffViewer } from '../shared/DiffViewer';
import { IgnoredDifferencesNote } from '../views/AppDiffPanel';
import styles from './Details.module.css';

export interface DiffTabProps {
  item: ResourceItem;
  diff: ResourceDiff | undefined;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
}

/** Normalized live state vs predicted (desired) state of one managed resource. */
export function DiffTab({ item, diff, loading, error, onRetry }: DiffTabProps) {
  const [compact, setCompact] = usePref('compactDiff');
  const [inline, setInline] = usePref('inlineDiff');
  if (loading) {
    return (
      <div aria-busy="true" className={styles.tabBody}>
        <span className="sr-only">Loading diff</span>
        <Skeleton height={240} />
      </div>
    );
  }
  if (error) return <ErrorBanner error={error} onRetry={onRetry} title="Could not load the diff" />;
  if (!diff) {
    return (
      <EmptyState title="No comparison available" headingLevel={3}>
        <p>Only resources rendered from the application&apos;s source are compared. Refresh the application if this resource was just added.</p>
      </EmptyState>
    );
  }
  const { liveYAML, desiredYAML } = diffSides(diff);
  const label = `${item.kind} ${item.name}`;
  return (
    <div className={styles.tabBody}>
      <div className={styles.manifestToolbar}>
        <Switch checked={compact} onChange={setCompact} label="Compact diff" />
        <Switch checked={inline} onChange={setInline} label="Inline diff" />
      </div>
      <IgnoredDifferencesNote />
      {liveYAML === desiredYAML ? (
        <EmptyState title="No differences" headingLevel={3}>
          <p>The live object matches the desired state.</p>
        </EmptyState>
      ) : (
        <DiffViewer caption={`Diff of ${label}: live and desired state`} oldText={liveYAML} newText={desiredYAML} compact={compact} inline={inline} />
      )}
    </div>
  );
}

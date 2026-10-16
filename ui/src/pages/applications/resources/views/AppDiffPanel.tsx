import { useMemo, useState } from 'react';
import { useSearchParams } from 'react-router';
import { useManagedResources } from '../../../../api/queries';
import { Banner, Button, EmptyState, Mono, Skeleton, Switch } from '../../../../design';
import { IconChevronDown, IconChevronRight } from '../../../../design/icons';
import { ErrorBanner } from '../../../../shell/errors';
import { RESOURCE_PARAMS, type AppDiffPanelProps } from '../contract';
import { outOfSyncDiffs, type DiffItem } from '../model/diffItems';
import { usePref } from '../model/prefs';
import { DiffViewer } from '../shared/DiffViewer';
import styles from './AppDiffPanel.module.css';

const PAGE = 20;
const EXPANDED_BY_DEFAULT = 5;

export function IgnoredDifferencesNote() {
  return (
    <p className={styles.note}>
      Fields matched by <code>ignoreDifferences</code> (in the application or in system-level resource customizations) are normalized out
      before comparing, so they never appear here. Secret values are masked.
    </p>
  );
}

function DiffEntry({ entry, single, expanded, onToggle, compact, inline, onOpen }: {
  entry: DiffItem;
  single: boolean;
  expanded: boolean;
  onToggle: () => void;
  compact: boolean;
  inline: boolean;
  onOpen: () => void;
}) {
  const kindName = `${entry.diff.kind ?? ''} ${entry.diff.name ?? ''}`;
  return (
    <section className={styles.entry} aria-label={`Diff of ${kindName}`}>
      {single ? null : (
        <h3 className={styles.entryHeading}>
          <button type="button" className={styles.disclosure} aria-expanded={expanded} onClick={onToggle}>
            {expanded ? <IconChevronDown size={14} /> : <IconChevronRight size={14} />}
            <Mono>{entry.path}</Mono>
          </button>
        </h3>
      )}
      {expanded || single ? (
        <>
          <div className={styles.entryActions}>
            <Button size="sm" onClick={onOpen}>
              Open details of {kindName}
            </Button>
          </div>
          <DiffViewer caption={`Diff of ${kindName}: live and desired state`} oldText={entry.liveYAML} newText={entry.desiredYAML} compact={compact} inline={inline} />
        </>
      ) : null}
    </section>
  );
}

/** Every out-of-sync resource of the application with its live → desired diff (DIFF-042). */
export function AppDiffPanel({ app, onOpenResource }: AppDiffPanelProps) {
  const name = app.metadata.name ?? '';
  const scope = { appNamespace: app.metadata.namespace, project: app.spec.project };
  const query = useManagedResources(name, scope);
  const [compact, setCompact] = usePref('compactDiff');
  const [inline, setInline] = usePref('inlineDiff');
  const [, setSearchParams] = useSearchParams();
  const entries = useMemo(() => outOfSyncDiffs(query.data?.items ?? []), [query.data]);
  const [limit, setLimit] = useState(PAGE);
  const [toggled, setToggled] = useState<ReadonlySet<string>>(() => new Set());
  const isExpanded = (entry: DiffItem, index: number) => (index < EXPANDED_BY_DEFAULT) !== toggled.has(entry.key);

  const open = (key: string) => {
    if (onOpenResource) onOpenResource(key);
    else
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev);
        next.set(RESOURCE_PARAMS.node, key);
        next.set(RESOURCE_PARAMS.tab, 'diff');
        return next;
      });
  };

  if (query.isPending) {
    return (
      <div aria-busy="true" className={styles.root}>
        <span className="sr-only">Loading differences</span>
        <Skeleton height={20} width="40%" />
        <Skeleton height={240} />
      </div>
    );
  }
  if (query.isError) return <ErrorBanner error={query.error} onRetry={() => void query.refetch()} title="Could not load the differences" />;

  return (
    <div className={styles.root}>
      <div className={styles.toolbar}>
        <Switch checked={compact} onChange={setCompact} label="Compact diff" />
        <Switch checked={inline} onChange={setInline} label="Inline diff" />
        {entries.length > 1 ? (
          <>
            <Button size="sm" variant="ghost" onClick={() => setToggled(new Set(entries.slice(EXPANDED_BY_DEFAULT).map((e) => e.key)))}>
              Expand all
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setToggled(new Set(entries.slice(0, EXPANDED_BY_DEFAULT).map((e) => e.key)))}>
              Collapse all
            </Button>
          </>
        ) : null}
      </div>
      <IgnoredDifferencesNote />
      {entries.length === 0 ? (
        <EmptyState title="No differences" headingLevel={3}>
          <p>The live state of every resource matches the desired state from the source. Refresh the application if you expected changes.</p>
        </EmptyState>
      ) : (
        <>
          <p className={styles.count} role="status">
            {entries.length} {entries.length === 1 ? 'resource differs' : 'resources differ'} from the desired state.
          </p>
          {entries.slice(0, limit).map((entry, index) => (
            <DiffEntry
              key={entry.key}
              entry={entry}
              single={entries.length === 1}
              expanded={isExpanded(entry, index)}
              onToggle={() =>
                setToggled((s) => {
                  const next = new Set(s);
                  if (next.has(entry.key)) next.delete(entry.key);
                  else next.add(entry.key);
                  return next;
                })
              }
              compact={compact}
              inline={inline}
              onOpen={() => open(entry.key)}
            />
          ))}
          {entries.length > limit ? (
            <Button onClick={() => setLimit((l) => l + PAGE)}>Show {Math.min(PAGE, entries.length - limit)} more resources</Button>
          ) : null}
        </>
      )}
      {query.data && entries.length === 0 && query.data.items.some((i) => i.hook) ? (
        <Banner tone="info">Sync hooks are not compared and are not listed.</Banner>
      ) : null}
    </div>
  );
}

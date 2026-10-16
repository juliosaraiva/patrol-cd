import { useId, useState } from 'react';
import type { Application } from '../../../api/types';
import { Banner, Button, Timestamp } from '../../../design';
import { activeConditions, conditionSeverity } from '../shared/app';
import styles from './Details.module.css';

/** Recovery guidance per condition type (api/v1alpha1/conditions.go). */
export const CONDITION_GUIDANCE: Record<string, string> = {
  InvalidSpecError:
    'The spec references something missing or not permitted (project, destination, repository). Fix it in Details, or ask a project administrator to allow it.',
  ComparisonError:
    'The desired state could not be rendered or compared. Check repository access, the path or chart, and tool parameters, then use Hard refresh.',
  SyncError: 'The last sync failed. Open the operation to see which resources failed and why, fix the cause and sync again.',
  DeletionError: 'Deleting the application or its resources failed. Check finalizers on the application and the controller logs.',
  UnknownError: 'The controller hit an unexpected error. Retry with Refresh; if it persists, check the controller logs.',
  SharedResourceWarning:
    'Some resources are also managed by another application. Keep each resource in one application; enable FailOnSharedResource to stop such syncs.',
  RepeatedResourceWarning: 'The same resource is defined more than once in the source. Remove the duplicate definitions.',
  ExcludedResourceWarning: 'The source contains kinds excluded by the resource exclusions or project rules; they are not synced.',
  OrphanedResourceWarning:
    'Resources in the destination namespace are not managed by any application. Review them, or adjust orphaned resource monitoring in the project.',
};

const VISIBLE = 3;

/** Error and warning conditions with recovery guidance (parity API-100). */
export function ConditionsPanel({ app }: { app: Application }) {
  const headingId = useId();
  const [expanded, setExpanded] = useState(false);
  const conditions = activeConditions(app).filter((c) => conditionSeverity(c.type) !== 'info');
  if (conditions.length === 0) return null;
  const shown = expanded ? conditions : conditions.slice(0, VISIBLE);
  return (
    <section className={styles.section} aria-labelledby={headingId}>
      <div className={styles.sectionHeader}>
        <h2 id={headingId}>Conditions ({conditions.length})</h2>
        {conditions.length > VISIBLE ? (
          <Button size="sm" variant="ghost" aria-expanded={expanded} onClick={() => setExpanded(!expanded)}>
            {expanded ? 'Show fewer' : `Show all ${conditions.length}`}
          </Button>
        ) : null}
      </div>
      <div className={styles.conditions}>
        {shown.map((c, i) => {
          const severity = conditionSeverity(c.type);
          return (
            <Banner key={`${c.type}-${i}`} tone={severity === 'error' ? 'error' : 'warning'} title={c.type}>
              <p>{c.message}</p>
              {CONDITION_GUIDANCE[c.type] ? <p>{CONDITION_GUIDANCE[c.type]}</p> : null}
              {c.lastTransitionTime ? (
                <p className={styles.conditionMeta}>
                  {c.reason ? `${c.reason}, ` : ''}since <Timestamp value={c.lastTransitionTime} />
                </p>
              ) : null}
            </Banner>
          );
        })}
      </div>
    </section>
  );
}

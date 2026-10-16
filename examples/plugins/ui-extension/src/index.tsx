/**
 * "Backups" tab for ops.example.com/BackupSchedule resources (see
 * examples/plugins/lua for the matching health check and actions).
 *
 * Built into one classic script (dist/extension-backup-schedule.js) that registers itself
 * through window.extensionsAPI when the UI loads extensions.js.
 */
import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import type { KubernetesObject, ResourceTabProps } from './host';

interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
}

interface BackupSchedule extends KubernetesObject {
  spec?: {
    schedule?: string;
    target?: string;
    suspend?: boolean;
    retentionDays?: number;
  };
  status?: {
    observedGeneration?: number;
    lastBackupTime?: string;
    lastBackupResult?: 'Succeeded' | 'Failed';
    conditions?: Condition[];
  };
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <tr>
      <th scope="row" style={{ textAlign: 'left', paddingRight: 16, fontWeight: 600 }}>
        {label}
      </th>
      <td>{children}</td>
    </tr>
  );
}

function BackupsTab({ resource }: ResourceTabProps) {
  const schedule = resource as BackupSchedule;
  const [showConditions, setShowConditions] = useState(false);
  const conditions = useMemo(() => schedule.status?.conditions ?? [], [schedule.status?.conditions]);
  const stale = (schedule.status?.observedGeneration ?? 0) < (schedule.metadata?.generation ?? 0);

  return (
    <section aria-label="Backup schedule" style={{ padding: 16 }}>
      <table>
        <tbody>
          <Row label="Target">{schedule.spec?.target ?? 'not set'}</Row>
          <Row label="Schedule">
            <code>{schedule.spec?.schedule ?? 'not set'}</code>
            {schedule.spec?.suspend === true ? ' (suspended)' : null}
          </Row>
          <Row label="Retention">{schedule.spec?.retentionDays ?? 'controller default'} days</Row>
          <Row label="Last backup">
            {schedule.status?.lastBackupTime ?? 'never'}
            {schedule.status?.lastBackupResult ? `: ${schedule.status.lastBackupResult}` : null}
          </Row>
          {stale ? <Row label="Note">The controller has not observed the latest spec yet.</Row> : null}
        </tbody>
      </table>
      <p>
        <button type="button" onClick={() => setShowConditions((v) => !v)}>
          {showConditions ? 'Hide' : 'Show'} conditions ({conditions.length})
        </button>
      </p>
      {showConditions ? (
        <ul>
          {conditions.map((c) => (
            <li key={c.type}>
              <strong>{c.type}</strong>={c.status}
              {c.reason ? ` (${c.reason})` : null}
              {c.message ? `: ${c.message}` : null}
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}

window.extensionsAPI.registerResourceExtension(BackupsTab, 'ops.example.com', 'BackupSchedule', 'Backups', {
  icon: 'fa-database',
});

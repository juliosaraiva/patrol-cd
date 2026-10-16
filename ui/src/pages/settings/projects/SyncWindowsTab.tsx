import { useMemo, useState } from 'react';
import { useProjectSyncWindows } from '../../../api/hooks/settings';
import type { SyncWindow } from '../../../api/types';
import {
  Badge,
  Banner,
  Button,
  Checkbox,
  EmptyState,
  FormField,
  IconButton,
  IconMore,
  IconPlus,
  Input,
  Menu,
  Mono,
  RadioGroup,
  Select,
  Table,
  formatAbsolute,
} from '../../../design';
import { useNow } from '../../../design/hooks';
import { timeZones } from '../common/catalog';
import { describeCron, isValidTimeZone, parseCron, windowState, type WindowState } from '../common/cron';
import { formatDuration, parseGoDuration } from '../common/duration';
import { nonEmpty, orDefault, StringListEditor } from '../common/editors';
import { ConfirmDialog, useFormErrors, type FieldError } from '../common/feedback';
import styles from '../common/common.module.css';
import { ProjectSection, SectionDrawer, useProjectSave, type ProjectContext } from './shared';

interface WindowForm {
  kind: 'allow' | 'deny';
  schedule: string;
  duration: string;
  timeZone: string;
  applications: string[];
  namespaces: string[];
  clusters: string[];
  manualSync: boolean;
  syncOverrun: boolean;
  andOperator: boolean;
  description: string;
}

function toForm(w: SyncWindow | null): WindowForm {
  return {
    kind: w?.kind ?? 'allow',
    schedule: w?.schedule ?? '',
    duration: w?.duration ?? '',
    timeZone: w?.timeZone ?? 'UTC',
    applications: w?.applications ?? [],
    namespaces: w?.namespaces ?? [],
    clusters: w?.clusters ?? [],
    manualSync: w?.manualSync ?? false,
    syncOverrun: w?.syncOverrun ?? false,
    andOperator: w?.andOperator ?? false,
    description: w?.description ?? '',
  };
}

function fromForm(f: WindowForm): SyncWindow {
  const out: SyncWindow = {
    kind: f.kind,
    schedule: f.schedule.trim().replace(/\s+/g, ' '),
    duration: f.duration.trim(),
    timeZone: f.timeZone || undefined,
  };
  const apps = nonEmpty(f.applications);
  const ns = nonEmpty(f.namespaces);
  const clusters = nonEmpty(f.clusters);
  if (apps.length) out.applications = apps;
  if (ns.length) out.namespaces = ns;
  if (clusters.length) out.clusters = clusters;
  if (f.manualSync) out.manualSync = true;
  if (f.syncOverrun) out.syncOverrun = true;
  if (f.andOperator) out.andOperator = true;
  if (f.description.trim()) out.description = f.description.trim();
  return out;
}

const sameWindow = (a: SyncWindow, b: SyncWindow) =>
  a.kind === b.kind && a.schedule === b.schedule && a.duration === b.duration && (a.timeZone ?? '') === (b.timeZone ?? '') && JSON.stringify(a.applications ?? []) === JSON.stringify(b.applications ?? []) && JSON.stringify(a.namespaces ?? []) === JSON.stringify(b.namespaces ?? []) && JSON.stringify(a.clusters ?? []) === JSON.stringify(b.clusters ?? []);

/** Client-side state of a window; null when its schedule, duration or time zone is invalid. */
function computeState(w: SyncWindow, now: number, upcoming = 3): WindowState | null {
  const cron = parseCron(w.schedule ?? '');
  const duration = parseGoDuration(w.duration ?? '');
  if (!cron.ok || duration === null || duration <= 0 || !isValidTimeZone(w.timeZone ?? '')) return null;
  return windowState(cron.schedule, duration, now, orDefault(w.timeZone, 'UTC'), upcoming);
}

function formatInZone(date: Date, timeZone: string): string {
  try {
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short', timeZone: timeZone || 'UTC' }).format(date);
  } catch {
    return formatAbsolute(date);
  }
}

/** "Oct 19, 2026, 9:00 – 11:00 AM" in the window's time zone. */
function formatRangeInZone(start: Date, end: Date, timeZone: string): string {
  try {
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short', timeZone: timeZone || 'UTC' }).formatRange(start, end);
  } catch {
    return `${formatInZone(start, timeZone)} – ${formatInZone(end, timeZone)}`;
  }
}

const browserZone = (() => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch {
    return 'UTC';
  }
})();

function scopeSummary(w: SyncWindow): string {
  const parts = [
    w.applications?.length ? `applications ${w.applications.join(', ')}` : null,
    w.namespaces?.length ? `namespaces ${w.namespaces.join(', ')}` : null,
    w.clusters?.length ? `clusters ${w.clusters.join(', ')}` : null,
  ].filter(Boolean);
  return parts.join(w.andOperator ? ' and ' : '; ') || 'Nothing (window matches no application)';
}

function WindowPreview({ form }: { form: WindowForm }) {
  const now = useNow(60_000);
  const state = computeState(fromForm(form), now);
  if (!state) return null;
  const tz = form.timeZone || 'UTC';
  return (
    <section aria-labelledby="window-preview" className={styles.stackTight}>
      <h3 id="window-preview">Preview</h3>
      <p className={styles.muted} aria-live="polite">
        {state.active && state.activeUntil
          ? `This window is open now, until ${formatInZone(state.activeUntil, tz)} (${tz}).`
          : 'This window is closed now.'}
      </p>
      {state.upcoming.length > 0 ? (
        <ul className={styles.preview} aria-label="Next windows">
          {state.upcoming.map((w) => (
            <li key={w.start.toISOString()}>
              {formatRangeInZone(w.start, w.end, tz)} <span className={styles.small}>({tz}{tz === browserZone ? '' : `; starts ${formatInZone(w.start, browserZone)} your time`})</span>
            </li>
          ))}
        </ul>
      ) : (
        <p className={styles.muted}>The schedule never fires in the next four years.</p>
      )}
    </section>
  );
}

function WindowDrawer({ ctx, index, onClose }: { ctx: ProjectContext; index: number | null; onClose: () => void }) {
  const windows = ctx.project.spec.syncWindows ?? [];
  const existing = index === null ? null : (windows[index] ?? null);
  const saver = useProjectSave(ctx);
  const [f, setF] = useState<WindowForm>(() => toForm(existing));
  const zones = useMemo(() => {
    const all = timeZones();
    return f.timeZone && !all.includes(f.timeZone) ? [f.timeZone, ...all] : all;
  }, [f.timeZone]);
  const cron = parseCron(f.schedule);
  const cronText = cron.ok ? describeCron(f.schedule) : null;
  const durationMs = parseGoDuration(f.duration);
  const errors: FieldError[] = [];
  if (!cron.ok) errors.push({ id: 'window-schedule', message: cron.error });
  if (f.duration.trim() === '') errors.push({ id: 'window-duration', message: 'Enter a duration such as 1h, 30m or 1h30m.' });
  else if (durationMs === null || durationMs <= 0) errors.push({ id: 'window-duration', message: 'Use a positive duration with h, m or s units, such as 1h30m.' });
  if (!isValidTimeZone(f.timeZone)) errors.push({ id: 'window-tz', message: 'Choose a valid IANA time zone.' });
  if (nonEmpty(f.applications).length + nonEmpty(f.namespaces).length + nonEmpty(f.clusters).length === 0) {
    errors.push({ id: 'window-apps-add', message: 'Add at least one application, namespace or cluster for the window to apply to.' });
  }
  const form = useFormErrors(errors);

  const submit = () => {
    if (!form.check()) return;
    const next = fromForm(f);
    saver.save(
      (s) => {
        const list = [...(s.syncWindows ?? [])];
        if (index === null) list.push(next);
        else list[index] = next;
        return { ...s, syncWindows: list };
      },
      index === null ? 'Sync window added' : 'Sync window updated',
      onClose,
    );
  };

  const scopeError = form.errorFor('window-apps-add');

  return (
    <SectionDrawer
      title={index === null ? 'Add sync window' : 'Edit sync window'}
      description="Allow windows permit automated syncs only while open; deny windows block syncs while open. Deny wins when both match."
      onClose={onClose}
      onSubmit={submit}
      pending={saver.pending}
      error={saver.error}
      onReload={ctx.reload}
      errors={form.errors}
      attempt={form.attempt}
      submitLabel={index === null ? 'Add window' : 'Save window'}
    >
      <RadioGroup
        legend="Kind"
        orientation="horizontal"
        value={f.kind}
        onChange={(kind) => setF({ ...f, kind })}
        options={[
          { value: 'allow', label: 'Allow', description: 'Syncs may run only while the window is open.' },
          { value: 'deny', label: 'Deny', description: 'Syncs are blocked while the window is open.' },
        ]}
      />
      <div className={styles.formGrid}>
        <FormField
          label="Schedule (cron)"
          id="window-schedule"
          required
          error={form.errorFor('window-schedule')}
          help={
            cronText ??
            'Five fields: minute hour day-of-month month day-of-week, e.g. "0 22 * * *" (22:00 daily) or "0 9 * * 1-5" (09:00 on weekdays). @daily and @weekly also work.'
          }
        >
          <Input value={f.schedule} onChange={(e) => setF({ ...f, schedule: e.target.value })} mono placeholder="0 22 * * *" autoComplete="off" spellCheck={false} />
        </FormField>
        <FormField
          label="Duration"
          id="window-duration"
          required
          error={form.errorFor('window-duration')}
          help={durationMs && durationMs > 0 ? `Open for ${formatDuration(durationMs)} from each start.` : 'How long the window stays open, e.g. 1h or 8h30m.'}
        >
          <Input value={f.duration} onChange={(e) => setF({ ...f, duration: e.target.value })} mono placeholder="1h" autoComplete="off" />
        </FormField>
        <FormField label="Time zone" id="window-tz" error={form.errorFor('window-tz')} help="The schedule is evaluated in this time zone, including daylight saving changes.">
          <Select value={f.timeZone} onChange={(e) => setF({ ...f, timeZone: e.target.value })} options={zones.map((z) => ({ value: z, label: z }))} />
        </FormField>
        <FormField label="Description" id="window-description">
          <Input value={f.description} onChange={(e) => setF({ ...f, description: e.target.value })} />
        </FormField>
      </div>
      <WindowPreview form={f} />
      {scopeError ? (
        <Banner tone="error" title="The window applies to nothing">
          {scopeError}
        </Banner>
      ) : null}
      <StringListEditor
        legend="Applications"
        label="Application name or pattern"
        noun="application"
        idPrefix="window-apps"
        values={f.applications}
        onChange={(applications) => setF({ ...f, applications })}
        placeholder="payments-*"
        help="Globs allowed, e.g. * for every application in the project."
        emptyText="No application filter."
      />
      <StringListEditor
        legend="Namespaces"
        label="Destination namespace or pattern"
        noun="namespace"
        idPrefix="window-namespaces"
        values={f.namespaces}
        onChange={(namespaces) => setF({ ...f, namespaces })}
        placeholder="prod-*"
        emptyText="No namespace filter."
      />
      <StringListEditor
        legend="Clusters"
        label="Destination cluster name or pattern"
        noun="cluster"
        idPrefix="window-clusters"
        values={f.clusters}
        onChange={(clusters) => setF({ ...f, clusters })}
        placeholder="prod-eu"
        emptyText="No cluster filter."
      />
      <Checkbox
        label="Match all filters"
        description="An application must match the applications, namespaces and clusters filters together, instead of any one of them."
        checked={f.andOperator}
        onChange={(e) => setF({ ...f, andOperator: e.target.checked })}
      />
      <Checkbox
        label="Allow manual syncs"
        description={f.kind === 'deny' ? 'People can still sync manually while this deny window is open.' : 'Manual syncs are allowed even outside this allow window.'}
        checked={f.manualSync}
        onChange={(e) => setF({ ...f, manualSync: e.target.checked })}
      />
      <Checkbox
        label="Let running syncs finish (sync overrun)"
        description="A sync that started before the window closed (allow) or opened (deny) may keep running."
        checked={f.syncOverrun}
        onChange={(e) => setF({ ...f, syncOverrun: e.target.checked })}
      />
    </SectionDrawer>
  );
}

/** Sync windows tab: list with live active state, add/edit/delete (API-105, TEN-064). */
export function SyncWindowsTab({ ctx }: { ctx: ProjectContext }) {
  const name = ctx.project.metadata.name ?? '';
  const windows = ctx.project.spec.syncWindows ?? [];
  const server = useProjectSyncWindows(name);
  const saver = useProjectSave(ctx);
  const now = useNow(60_000);
  const [editing, setEditing] = useState<number | null | undefined>(undefined);
  const [deleting, setDeleting] = useState<number | null>(null);

  const rows = windows.map((w, index) => {
    const client = computeState(w, now, 1);
    const serverActive = server.data ? (server.data.windows ?? []).some((a) => sameWindow(a, w)) : undefined;
    return { w, index, client, active: serverActive ?? client?.active ?? false };
  });
  type RowT = (typeof rows)[number];

  return (
    <div className={styles.stack}>
      <ProjectSection
        id="project-windows"
        title="Sync windows"
        description="Time ranges in which syncs of this project's applications are allowed or denied."
      >
        <div>
          <Button variant="primary" icon={<IconPlus />} onClick={() => setEditing(null)}>
            Add sync window
          </Button>
        </div>
        {windows.length === 0 ? (
          <EmptyState headingLevel={3} title="No sync windows">
            Without windows, syncs may run at any time. Add a deny window for change freezes, or allow windows to limit automated syncs to
            maintenance hours.
          </EmptyState>
        ) : (
          <Table<RowT>
            caption="Sync windows"
            className={styles.wideTable}
            rows={rows}
            getRowKey={(r) => String(r.index)}
            onRowActivate={(r) => setEditing(r.index)}
            columns={[
              { id: 'kind', header: 'Kind', width: '80px', cell: (r) => <Badge tone={r.w.kind === 'deny' ? 'warning' : 'info'}>{r.w.kind === 'deny' ? 'Deny' : 'Allow'}</Badge> },
              {
                id: 'state',
                header: 'Now',
                width: '110px',
                cell: (r) =>
                  r.client === null ? <Badge tone="danger">Invalid</Badge> : r.active ? <Badge tone={r.w.kind === 'deny' ? 'danger' : 'success'}>Open</Badge> : <span className={styles.small}>Closed</span>,
              },
              { id: 'schedule', header: 'Schedule', width: '150px', cell: (r) => <Mono>{r.w.schedule}</Mono> },
              { id: 'duration', header: 'Duration', width: '90px', cell: (r) => <Mono>{r.w.duration}</Mono> },
              { id: 'tz', header: 'Time zone', width: '140px', cell: (r) => r.w.timeZone ?? 'UTC' },
              { id: 'scope', header: 'Applies to', cell: (r) => <span title={scopeSummary(r.w)}>{scopeSummary(r.w)}</span> },
              { id: 'manual', header: 'Manual sync', width: '100px', cell: (r) => (r.w.manualSync ? 'Allowed' : 'Blocked') },
              {
                id: 'actions',
                header: 'Actions',
                hideHeader: true,
                width: '56px',
                align: 'end',
                cell: (r) => (
                  <Menu
                    align="end"
                    label={`Actions for window ${r.index + 1}`}
                    trigger={(props) => <IconButton {...props} aria-label={`Actions for sync window ${r.index + 1} (${r.w.schedule ?? ''})`} icon={<IconMore />} size="sm" />}
                    items={[
                      { id: 'edit', label: 'Edit', onSelect: () => setEditing(r.index) },
                      { id: 'delete', label: 'Delete', danger: true, onSelect: () => setDeleting(r.index) },
                    ]}
                  />
                ),
              },
            ]}
          />
        )}
        {server.isError ? <p className={styles.small}>Open/closed state is computed in the browser: the server state could not be loaded.</p> : null}
      </ProjectSection>
      {editing !== undefined ? <WindowDrawer ctx={ctx} index={editing} onClose={() => setEditing(undefined)} /> : null}
      <ConfirmDialog
        open={deleting !== null}
        onClose={() => {
          setDeleting(null);
          saver.reset();
        }}
        title="Delete sync window?"
        confirmLabel="Delete window"
        action="delete the sync window"
        busy={saver.pending}
        error={saver.error}
        onConfirm={() => {
          if (deleting === null) return;
          saver.save((s) => ({ ...s, syncWindows: (s.syncWindows ?? []).filter((_, i) => i !== deleting) }), 'Sync window deleted', () => setDeleting(null));
        }}
      >
        <p>
          The {windows[deleting ?? -1]?.kind ?? ''} window <Mono>{windows[deleting ?? -1]?.schedule}</Mono> stops applying immediately.
        </p>
      </ConfirmDialog>
    </div>
  );
}

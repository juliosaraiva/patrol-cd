import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { usePatchApplicationAnnotations } from '../../api/hooks/notifications';
import { useNotificationServices, useNotificationTriggers } from '../../api/queries/resources';
import { useCanI, useSettings } from '../../api/queries/resources';
import type { Application } from '../../api/types';
import { Banner, Button, CodeBlock, FormField, IconButton, Input, Mono, Table, useToast, type Column } from '../../design';
import { IconClose, IconPlus } from '../../design/icons';
import { ErrorBanner } from '../../shell/errors';
import {
  formatRecipients,
  newRowId,
  parseSubscriptions,
  SUBSCRIBE_PREFIX,
  SUBSCRIPTIONS_ANNOTATION,
  subscriptionPatch,
  validateRows,
  type SubscriptionRow,
} from './subscriptions';
import styles from './Notifications.module.css';

/** RBAC object of an application: <project>/<name>, or <project>/<namespace>/<name> outside the control plane. */
export function applicationRBACObject(app: Application, controlPlaneNamespace: string | undefined): string {
  const ns = app.metadata.namespace;
  const name = app.metadata.name ?? '';
  return ns && ns !== controlPlaneNamespace ? `${app.spec.project}/${ns}/${name}` : `${app.spec.project}/${name}`;
}

export interface SubscriptionEditorProps {
  application: Application;
  /** Heading level of the section title (default 3, inside an application summary). */
  headingLevel?: 2 | 3;
}

function Heading({ level, id, children }: { level: 2 | 3; id: string; children: string }) {
  return level === 2 ? (
    <h2 id={id} className={styles.heading}>
      {children}
    </h2>
  ) : (
    <h3 id={id} className={styles.heading}>
      {children}
    </h3>
  );
}

const COLUMNS: Column<SubscriptionRow>[] = [
  { id: 'trigger', header: 'Trigger', cell: (r) => <Mono>{r.trigger}</Mono>, sortValue: (r) => r.trigger },
  { id: 'service', header: 'Service', cell: (r) => <Mono>{r.service}</Mono>, sortValue: (r) => r.service },
  {
    id: 'recipients',
    header: 'Recipients',
    cell: (r) => (formatRecipients(r.recipients) === '' ? <span className={styles.muted}>No recipient</span> : formatRecipients(r.recipients).split(';').join(', ')),
  },
];

/**
 * Per-application notification subscriptions (NOTIF-109): one row per
 * `notifications.cd.patrol.example/subscribe.<trigger>.<service>` annotation, with trigger and
 * service suggestions from the notifications API and recipient validation. Saving sends a merge
 * patch of only the changed subscription keys.
 */
export function SubscriptionEditor({ application, headingLevel = 3 }: SubscriptionEditorProps) {
  const headingId = useId();
  const listIds = { triggers: `${headingId}-triggers`, services: `${headingId}-services` };
  const settings = useSettings();
  const triggers = useNotificationTriggers();
  const services = useNotificationServices();
  const canUpdate = useCanI('applications', 'update', applicationRBACObject(application, settings.data?.controllerNamespace), !settings.isPending);
  const patch = usePatchApplicationAnnotations();
  const { toast } = useToast();

  const parsed = useMemo(() => parseSubscriptions(application.metadata.annotations), [application.metadata.annotations]);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<SubscriptionRow[]>([]);
  const [submitted, setSubmitted] = useState(false);
  const editButtonRef = useRef<HTMLButtonElement>(null);
  const addButtonRef = useRef<HTMLButtonElement>(null);
  const focusRow = useRef<string | null>(null);
  const restoreEditFocus = useRef(false);

  const triggerNames = triggers.data?.items?.map((t) => t.name) ?? [];
  const serviceNames = services.data?.items?.map((s) => s.name) ?? [];
  const validation = validateRows(draft, { triggers: triggers.isSuccess ? triggerNames : undefined, services: services.isSuccess ? serviceNames : undefined });
  const changes = editing ? subscriptionPatch(parsed.rows, draft) : {};
  const dirty = Object.keys(changes).length > 0;

  useEffect(() => {
    if (focusRow.current) {
      document.getElementById(`${focusRow.current}-trigger`)?.focus();
      focusRow.current = null;
    }
    if (!editing && restoreEditFocus.current) {
      editButtonRef.current?.focus();
      restoreEditFocus.current = false;
    }
  });

  const startEditing = () => {
    const rows = parsed.rows.map((r) => ({ ...r }));
    const initial = rows.length > 0 ? rows : [{ id: newRowId(), trigger: '', service: '', recipients: '' }];
    setDraft(initial);
    setSubmitted(false);
    patch.reset();
    setEditing(true);
    focusRow.current = initial[0]?.id ?? null;
  };

  const stopEditing = () => {
    setEditing(false);
    restoreEditFocus.current = true;
  };

  const update = (id: string, field: 'trigger' | 'service' | 'recipients', value: string) =>
    setDraft((rows) => rows.map((r) => (r.id === id ? { ...r, [field]: value } : r)));

  const addRow = () => {
    const id = newRowId();
    setDraft((rows) => [...rows, { id, trigger: '', service: '', recipients: '' }]);
    focusRow.current = id;
  };

  const removeRow = (index: number) => {
    const next = draft.filter((_, i) => i !== index);
    setDraft(next);
    const neighbour = next[Math.min(index, next.length - 1)];
    if (neighbour) focusRow.current = neighbour.id;
    else addButtonRef.current?.focus();
  };

  const save = () => {
    setSubmitted(true);
    if (!validation.valid || !dirty) return;
    patch.mutate(
      { name: application.metadata.name ?? '', namespace: application.metadata.namespace, project: application.spec.project, annotations: changes },
      {
        onSuccess: () => {
          toast({ tone: 'success', title: 'Notification subscriptions saved' });
          stopEditing();
        },
      },
    );
  };

  const readOnlyReason = canUpdate.data === false ? 'You can view subscriptions but not change them: your role lacks applications, update on this application.' : null;

  return (
    <section aria-labelledby={headingId} className={styles.editor}>
      <div className={styles.editorHeader}>
        <Heading level={headingLevel} id={headingId}>
          Notification subscriptions
        </Heading>
        {!editing && canUpdate.data !== false ? (
          <Button ref={editButtonRef} size="sm" onClick={startEditing} disabled={canUpdate.isPending}>
            Edit subscriptions
          </Button>
        ) : null}
      </div>
      {readOnlyReason ? <p className={styles.muted}>{readOnlyReason}</p> : null}

      {!editing ? (
        parsed.rows.length === 0 ? (
          <p className={styles.muted}>
            Not subscribed to any trigger. Subscribe to send messages to a service (Slack, email, webhook...) when a trigger such as{' '}
            <Mono>on-sync-failed</Mono> fires.
          </p>
        ) : (
          <Table caption="Notification subscriptions" columns={COLUMNS} rows={parsed.rows} getRowKey={(r) => r.id} getRowLabel={(r) => `${r.trigger} via ${r.service}`} />
        )
      ) : (
        <form
          className={styles.form}
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
        >
          {triggers.isError || services.isError ? (
            <Banner tone="warning" title="Suggestions unavailable">
              The configured triggers and services could not be loaded, so names are not suggested or checked. You can still type them.
            </Banner>
          ) : null}
          <datalist id={listIds.triggers}>
            {triggerNames.map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          <datalist id={listIds.services}>
            {serviceNames.map((n) => (
              <option key={n} value={n} />
            ))}
          </datalist>
          {draft.length === 0 ? <p className={styles.muted}>All subscriptions removed. Save to unsubscribe from every trigger.</p> : null}
          <ol className={styles.rows}>
            {draft.map((row, index) => {
              const errors = submitted || row.trigger || row.service ? validation.errors.get(row.id) : undefined;
              const warnings = validation.warnings.get(row.id);
              const label = `Subscription ${index + 1}`;
              return (
                <li key={row.id}>
                  <fieldset className={styles.row}>
                    <legend className="sr-only">{label}</legend>
                    <FormField label="Trigger" required error={errors?.trigger} help={warnings?.trigger} id={`${row.id}-trigger`}>
                      <Input
                        mono
                        list={listIds.triggers}
                        autoComplete="off"
                        placeholder="on-sync-succeeded"
                        value={row.trigger}
                        onChange={(e) => update(row.id, 'trigger', e.target.value)}
                      />
                    </FormField>
                    <FormField label="Service" required error={errors?.service} help={warnings?.service} id={`${row.id}-service`}>
                      <Input
                        mono
                        list={listIds.services}
                        autoComplete="off"
                        placeholder="slack"
                        value={row.service}
                        onChange={(e) => update(row.id, 'service', e.target.value)}
                      />
                    </FormField>
                    <FormField
                      label="Recipients"
                      error={errors?.recipients}
                      help={warnings?.recipients ?? 'Separate several recipients with ";".'}
                      id={`${row.id}-recipients`}
                      className={styles.recipients}
                    >
                      <Input
                        placeholder="my-channel1; my-channel2"
                        value={row.recipients}
                        onChange={(e) => update(row.id, 'recipients', e.target.value)}
                      />
                    </FormField>
                    <IconButton className={styles.remove} aria-label={`Remove ${label}`} icon={<IconClose />} onClick={() => removeRow(index)} />
                  </fieldset>
                </li>
              );
            })}
          </ol>
          <div>
            <Button ref={addButtonRef} size="sm" icon={<IconPlus />} onClick={addRow}>
              Add subscription
            </Button>
          </div>
          {submitted && !validation.valid ? (
            <Banner tone="error" live title="Fix the highlighted fields">
              Each subscription needs a trigger, a service and valid recipients.
            </Banner>
          ) : null}
          {patch.isError ? <ErrorBanner error={patch.error} title="Subscriptions were not saved" /> : null}
          <div className={styles.actions}>
            <Button type="submit" variant="primary" loading={patch.isPending} disabled={!dirty}>
              Save subscriptions
            </Button>
            <Button onClick={stopEditing}>Cancel</Button>
            {!dirty ? <span className={styles.muted}>No changes</span> : null}
          </div>
        </form>
      )}

      {parsed.defaultTriggerSubscriptions.length > 0 || parsed.yaml !== null ? (
        <div className={styles.other}>
          {headingLevel === 2 ? <h3 className={styles.subheading}>Other subscriptions</h3> : <h4 className={styles.subheading}>Other subscriptions</h4>}
          <p className={styles.muted}>These annotations are read-only here; edit them in the application annotations.</p>
          {parsed.defaultTriggerSubscriptions.length > 0 ? (
            <ul className={styles.plainList}>
              {parsed.defaultTriggerSubscriptions.map((s) => (
                <li key={s.key}>
                  Default triggers of <Mono>{s.service}</Mono> to {formatRecipients(s.recipients) || 'no recipient'} (
                  <Mono>{`${SUBSCRIBE_PREFIX}${s.service}`}</Mono>)
                </li>
              ))}
            </ul>
          ) : null}
          {parsed.yaml !== null ? <CodeBlock title={SUBSCRIPTIONS_ANNOTATION} label="Subscriptions annotation" code={parsed.yaml} wrap maxHeight={200} /> : null}
        </div>
      ) : null}
    </section>
  );
}

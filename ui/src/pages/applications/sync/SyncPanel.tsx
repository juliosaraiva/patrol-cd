import { useMemo, useState } from 'react';
import { useSettings } from '../../../api/queries/resources';
import { useSyncApplication } from '../../../api/queries/applications';
import type { Application, Operation } from '../../../api/types';
import { Banner, Button, ButtonLink, Checkbox, Drawer, FormField, HealthStatus, Input, Mono, SearchInput, SyncStatus, useToast } from '../../../design';
import {
  appDetailsPath,
  appName,
  appScope,
  appSources,
  isMultiSource,
  resourceKey,
  sourceKind,
  sourceLocation,
  type ManagedResourceStatus,
} from '../shared/app';
import { ActionError } from '../shared/errors';
import { ManualFlagsFields, RetryFields, SyncOptionsFields } from './SyncOptionsFields';
import styles from './Sync.module.css';
import { buildSyncRequest, initialSyncForm, optionValue, validateRetry, type SyncForm } from './syncOptions';

const PAGE = 200;

export interface SyncPanelProps {
  app: Application;
  open: boolean;
  onClose: () => void;
  /** Resource keys (group/kind/namespace/name) preselected for a selective sync. */
  preselected?: readonly string[] | undefined;
  /** Called with the Operation the server created. */
  onStarted?: (operation: Operation) => void;
}

/** Manual sync drawer (parity API-098). Mount with `key` per opening to reset its form. */
export function SyncPanel({ app, open, onClose, preselected, onStarted }: SyncPanelProps) {
  return (
    <Drawer open={open} onClose={onClose} title={`Sync ${appName(app)}`} description="Apply the desired state from the source to the cluster." width="720px">
      {open ? <SyncForm app={app} onClose={onClose} preselected={preselected} onStarted={onStarted} /> : null}
    </Drawer>
  );
}

function syncable(app: Application): ManagedResourceStatus[] {
  return (app.status?.resources ?? []).filter((r) => !r.hook);
}

function SyncForm({ app, onClose, preselected, onStarted }: Omit<SyncPanelProps, 'open'>) {
  const settings = useSettings();
  const sync = useSyncApplication();
  const { toast } = useToast();
  const resources = useMemo(() => syncable(app), [app]);
  const [form, setForm] = useState<SyncForm>(() => initialSyncForm(app));
  const [selected, setSelected] = useState<ReadonlySet<string>>(() => {
    const all = resources.map((r) => resourceKey(r));
    if (preselected && preselected.length > 0) return new Set(all.filter((k) => preselected.includes(k)));
    return new Set(all);
  });
  const [query, setQuery] = useState('');
  const [visible, setVisible] = useState(PAGE);
  const [confirmText, setConfirmText] = useState('');
  const [replaceText, setReplaceText] = useState('');
  const [forceConfirmed, setForceConfirmed] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  const sources = appSources(app);
  const replaceAllowed = settings.data?.syncWithReplaceAllowed ?? false;
  const retryErrors = validateRetry(form.retry);
  const selectedResources = resources.filter((r) => selected.has(resourceKey(r)));
  const allSelected = selectedResources.length === resources.length;
  const pruneTargets = form.prune ? selectedResources.filter((r) => r.requiresPruning) : [];
  const pruneAll = pruneTargets.length > 0 && pruneTargets.length === selectedResources.length;
  const replaceOn = replaceAllowed && optionValue(form.syncOptions, 'Replace') === 'true';
  const confirmOptions = ['Prune', 'Delete'].filter((k) => optionValue(form.syncOptions, k) === 'confirm');
  const awaitingApproval = resources.filter((r) => r.requiresDeletionConfirmation);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return resources;
    return resources.filter((r) => `${r.kind ?? ''} ${r.namespace ?? ''} ${r.name ?? ''} ${r.group ?? ''}`.toLowerCase().includes(q));
  }, [resources, query]);

  const problems: string[] = [];
  if (resources.length > 0 && selectedResources.length === 0) problems.push('Select at least one resource to sync.');
  if (Object.keys(retryErrors).length > 0) problems.push('Fix the retry settings.');
  if (pruneTargets.length > 0 && confirmText !== 'prune') problems.push("Type 'prune' to confirm deleting resources.");
  if (replaceOn && replaceText !== 'replace') problems.push("Type 'replace' to confirm the Replace option.");
  if (form.force && !forceConfirmed) problems.push('Confirm that you understand the Force option.');

  const submit = async () => {
    setSubmitted(true);
    if (problems.length > 0) return;
    const request = buildSyncRequest(app, {
      ...form,
      resources: allSelected
        ? null
        : selectedResources.map((r) => ({ group: r.group, kind: r.kind ?? '', namespace: r.namespace, name: r.name ?? '' })),
    });
    try {
      const { operation } = await sync.mutateAsync({ name: appName(app), scope: appScope(app), request });
      const opName = operation?.metadata.name ?? '';
      toast({
        tone: 'success',
        title: form.dryRun ? `Dry run started for ${appName(app)}` : `Sync started for ${appName(app)}`,
        description: opName ? `Operation ${opName}` : undefined,
        action: (
          <ButtonLink size="sm" to={appDetailsPath(app, `panel=operation${opName ? `&op=${encodeURIComponent(opName)}` : ''}`)}>
            View operation
          </ButtonLink>
        ),
      });
      if (operation) onStarted?.(operation);
      onClose();
    } catch {
      // Rendered below from sync.error.
    }
  };

  const toggle = (key: string, on: boolean) => {
    const next = new Set(selected);
    if (on) next.add(key);
    else next.delete(key);
    setSelected(next);
  };

  return (
    <form
      className={styles.form}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
      noValidate
    >
      <fieldset className={styles.fieldset}>
        <legend>{isMultiSource(app) ? 'Revisions' : 'Revision'}</legend>
        <div className={styles.stack}>
          {sources.map((source, i) => (
            <FormField
              key={i}
              label={isMultiSource(app) ? `Revision of source ${i + 1}` : 'Revision'}
              help={
                <>
                  {sourceKind(source)} <Mono>{source.repoURL}</Mono>
                  {sourceLocation(source) ? (
                    <>
                      {' '}
                      <Mono>{sourceLocation(source)}</Mono>
                    </>
                  ) : null}
                  . Leave the target revision to sync what the application tracks; another branch, tag or commit is a one-off override.
                </>
              }
            >
              <Input
                mono
                value={form.revisions[i] ?? ''}
                onChange={(e) => {
                  const revisions = [...form.revisions];
                  revisions[i] = e.target.value;
                  setForm({ ...form, revisions });
                }}
              />
            </FormField>
          ))}
          {sources.length === 0 ? <p className={styles.muted}>This application has no source; there is nothing to sync.</p> : null}
        </div>
      </fieldset>

      <ManualFlagsFields
        value={form}
        onChange={(flags) => setForm({ ...form, ...flags })}
      />
      {form.force ? (
        <Banner tone="warning" title="Force is destructive">
          <p>
            Resources that cannot be patched are deleted and recreated immediately, bypassing graceful deletion. This can cause downtime or
            data loss.
          </p>
          <Checkbox label="I understand that Force deletes and recreates resources" checked={forceConfirmed} onChange={(e) => setForceConfirmed(e.target.checked)} />
        </Banner>
      ) : null}

      <SyncOptionsFields options={form.syncOptions} onChange={(syncOptions) => setForm({ ...form, syncOptions })} replaceAllowed={replaceAllowed} />
      {replaceOn ? (
        <Banner tone="warning" title="Replace recreates resources">
          <p>Resources are replaced (and deleted and recreated when needed) instead of applied. This can cause downtime.</p>
          <FormField label="Type 'replace' to confirm">
            <Input value={replaceText} onChange={(e) => setReplaceText(e.target.value)} autoComplete="off" />
          </FormField>
        </Banner>
      ) : null}
      {confirmOptions.length > 0 || awaitingApproval.length > 0 ? (
        <Banner tone="info" title="Deletion approval">
          <p>
            {confirmOptions.length > 0 ? `${confirmOptions.map((k) => `${k}=confirm`).join(' and ')} holds deletions until approved. ` : ''}
            {awaitingApproval.length > 0 ? `${awaitingApproval.length} resource(s) are waiting for approval. ` : ''}
            After the sync starts, approve deletions with Confirm pruning in the operation panel.
          </p>
        </Banner>
      ) : null}

      <RetryFields value={form.retry} onChange={(retry) => setForm({ ...form, retry })} errors={submitted ? retryErrors : {}} />

      <fieldset className={styles.fieldset}>
        <legend>Resources</legend>
        {resources.length === 0 ? (
          <p className={styles.muted}>No resources have been compared yet; the whole application is synced.</p>
        ) : (
          <>
            <p className={styles.muted}>
              {allSelected
                ? `All ${resources.length} resources are synced.`
                : `Selective sync of ${selectedResources.length} of ${resources.length} resources.`}{' '}
              A selective sync applies only the chosen resources and skips sync hooks; hooks are not listed here.
            </p>
            <div className={styles.resourceTools}>
              <SearchInput size="sm" aria-label="Filter resources" value={query} onValueChange={setQuery} />
              <Button size="sm" onClick={() => setSelected(new Set(resources.map((r) => resourceKey(r))))}>
                Select all
              </Button>
              <Button size="sm" onClick={() => setSelected(new Set(resources.filter((r) => r.status === 'OutOfSync').map((r) => resourceKey(r))))}>
                Select out of sync
              </Button>
              <Button size="sm" onClick={() => setSelected(new Set())}>
                Select none
              </Button>
            </div>
            <ul className={styles.resourceList} aria-label="Resources to sync">
              {filtered.slice(0, visible).map((r) => {
                const key = resourceKey(r);
                const label = `${r.kind ?? ''} ${r.namespace ? `${r.namespace}/` : ''}${r.name ?? ''}`;
                return (
                  <li key={key} className={styles.resourceItem}>
                    <Checkbox label={<Mono>{label}</Mono>} aria-label={`Sync ${label}`} checked={selected.has(key)} onChange={(e) => toggle(key, e.target.checked)} />
                    <span className={styles.resourceStatus}>
                      <SyncStatus status={r.status} />
                      {r.health?.status ? <HealthStatus status={r.health.status} /> : null}
                      {r.requiresPruning ? <span>Requires pruning</span> : null}
                    </span>
                  </li>
                );
              })}
            </ul>
            {filtered.length > visible ? (
              <p className={styles.muted}>
                Showing {visible} of {filtered.length}.{' '}
                <Button size="sm" variant="ghost" onClick={() => setVisible(visible + PAGE)}>
                  Show more
                </Button>
              </p>
            ) : null}
          </>
        )}
      </fieldset>

      {pruneTargets.length > 0 ? (
        <Banner tone="warning" title={pruneAll ? 'Every selected resource will be deleted' : `${pruneTargets.length} resource(s) will be deleted`}>
          <p>
            {pruneAll
              ? 'All resources in this sync require pruning: the sync deletes them from the cluster and creates nothing.'
              : 'Prune deletes these live resources because they are no longer in the source:'}
          </p>
          <ul className={styles.pruneList}>
            {pruneTargets.slice(0, 50).map((r) => (
              <li key={resourceKey(r)}>
                <Mono>
                  {r.kind} {r.namespace ? `${r.namespace}/` : ''}
                  {r.name}
                </Mono>
              </li>
            ))}
            {pruneTargets.length > 50 ? <li>and {pruneTargets.length - 50} more</li> : null}
          </ul>
          <FormField label="Type 'prune' to confirm">
            <Input value={confirmText} onChange={(e) => setConfirmText(e.target.value)} autoComplete="off" />
          </FormField>
        </Banner>
      ) : null}

      {submitted && problems.length > 0 ? (
        <Banner tone="error" live title="The sync cannot start yet">
          <ul>
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </Banner>
      ) : null}
      {sync.isError ? <ActionError error={sync.error} action="sync" onRetry={() => void submit()} /> : null}

      <div className={styles.footer}>
        <Button onClick={onClose}>Cancel</Button>
        <Button type="submit" variant="primary" loading={sync.isPending} disabled={sources.length === 0}>
          {form.dryRun ? 'Start dry run' : 'Synchronize'}
        </Button>
      </div>
    </form>
  );
}

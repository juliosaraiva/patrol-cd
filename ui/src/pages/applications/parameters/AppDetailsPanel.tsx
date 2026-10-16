import { useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import { useApplicationLinks, useUpdateApplication } from '../../../api/queries/applications';
import { useProjects, useSettings } from '../../../api/queries/resources';
import type { Application, ApplicationTree } from '../../../api/types';
import {
  Badge,
  Banner,
  Button,
  Drawer,
  FormField,
  Input,
  KeyValueList,
  Mono,
  RadioGroup,
  Select,
  Switch,
  Tabs,
  TextArea,
  Timestamp,
  useToast,
} from '../../../design';
import { IconExternal, IconPlus } from '../../../design/icons';
import { useResourceExtensionTabs } from '../../../extensions';
import { isHttpURL } from '../../../lib/url';
import { SubscriptionEditor } from '../../notifications';
import { appName, appSources, isAutoSyncEnabled, isMultiSource } from '../shared/app';
import { ActionError } from '../shared/errors';
import { applicationExternalLinks, managedByURL } from '../shared/links';
import { RetryFields, SyncOptionsFields } from '../sync/SyncOptionsFields';
import { validateRetry } from '../sync/syncOptions';
import { StatusBadge } from '../details/StatusBadge';
import { KeyValueEditor } from './Fields';
import styles from './Parameters.module.css';
import { editableChanges, rebaseApplication, rebaseManifest, rebaseSources, rebaseSummary, type RebaseReport } from './rebase';
import { SourceEditor } from './SourceEditor';
import {
  applySources,
  applySummary,
  hasErrors,
  manifestYAML,
  newSourceDraft,
  parseManifest,
  sourceDrafts,
  summaryFormFrom,
  validateSources,
  validateSummary,
  type SourceDraft,
  type SummaryForm,
} from './specEdit';

/** 'summary' | 'parameters' | 'manifest', or the id of an Application resource-tab extension. */
export type DetailsTab = string;

export function parseDetailsTab(value: string | null): DetailsTab {
  return value === null || value === '' ? 'summary' : value;
}

export interface AppDetailsPanelProps {
  app: Application;
  open: boolean;
  onClose: () => void;
  tab: DetailsTab;
  onTabChange: (tab: DetailsTab) => void;
  /** Refetches the application and resolves with the latest object. */
  reloadApp: () => Promise<Application | undefined>;
  /** Resource tree when cached, passed to extension tabs. */
  tree?: ApplicationTree | undefined;
}

interface Edit<T> {
  /** Version the draft is based on: where the edit started, or the version it was rebased onto. */
  base: Application;
  draft: T;
  /** What the last rebase took from the server, shown until the edit is saved or cancelled. */
  report?: RebaseReport | undefined;
}

type SetEdit<T> = Dispatch<SetStateAction<Edit<T> | null>>;

/**
 * Editing state shared by the three tabs: the draft survives tab switches. Saves apply the
 * user's changes (draft relative to `base`) onto the latest known version, so status updates
 * from the controller (which bump resourceVersion) do not cause conflicts; when an editable
 * field (labels, annotations, finalizers, spec) changed on the server, saving is held until the
 * user rebases and reviews (StaleNotice, RebaseNotice).
 */
function useSave() {
  const update = useUpdateApplication();
  const { toast } = useToast();
  const save = async (next: Application, what: string): Promise<boolean> => {
    try {
      await update.mutateAsync({ app: next, validate: true });
      toast({ tone: 'success', title: `${what} saved`, description: `${appName(next)} was updated.` });
      update.reset();
      return true;
    } catch {
      return false;
    }
  };
  /** 'blocked' when the server changed editable fields since `base`; saved otherwise. */
  const saveOnto = (base: Application, mine: Application, current: Application, what: string): 'blocked' | Promise<boolean> => {
    if (editableChanges(base, current).length > 0) return 'blocked';
    return save(rebaseApplication(base, mine, current).merged, what);
  };
  return { update, save, saveOnto };
}

const MAX_PATHS = 8;

function PathList({ paths }: { paths: readonly string[] }) {
  const shown = paths.slice(0, MAX_PATHS);
  return (
    <ul className={styles.pathList}>
      {shown.map((p) => (
        <li key={p}>
          <Mono>{p}</Mono>
        </li>
      ))}
      {paths.length > shown.length ? <li>and {paths.length - shown.length} more</li> : null}
    </ul>
  );
}

/**
 * Shown while labels, annotations, finalizers or the spec differ between the version the draft is
 * based on and the live application (status-only updates do not count).
 */
function StaleNotice({ base, app, blocked, onReload }: { base: Application; app: Application; blocked: boolean; onReload: () => void }) {
  const changes = editableChanges(base, app);
  if (changes.length === 0) return null;
  return (
    <Banner
      tone={blocked ? 'error' : 'warning'}
      live={blocked}
      title={blocked ? 'Review the changes made on the server before saving' : 'This application changed on the server since you started editing'}
      actions={
        <Button size="sm" onClick={onReload}>
          Use latest version
        </Button>
      }
    >
      <p>Changed on the server:</p>
      <PathList paths={changes} />
      <p>
        Use the latest version to merge your edits onto it: fields you did not edit take the server&apos;s values, and you review the
        result before saving.
      </p>
    </Banner>
  );
}

/** What the last rebase merged, so the user reviews it before saving. */
function RebaseNotice({ report }: { report: RebaseReport | undefined }) {
  if (!report) return null;
  const { serverChanges, conflicts } = report;
  return (
    <Banner tone={conflicts.length > 0 ? 'warning' : 'info'} live title="Your edits were merged onto the latest version">
      {serverChanges.length > 0 ? (
        <>
          <p>Taken from the server (you did not edit these):</p>
          <PathList paths={serverChanges} />
        </>
      ) : null}
      {conflicts.length > 0 ? (
        <>
          <p>Changed by you and on the server; your values will be saved:</p>
          <PathList paths={conflicts} />
        </>
      ) : null}
      {serverChanges.length === 0 && conflicts.length === 0 ? <p>None of the fields you can edit changed on the server.</p> : null}
      <p>Review the values, then save.</p>
    </Banner>
  );
}

/** Application details drawer: summary, parameters/sources and manifest (parity API-094). */
export function AppDetailsPanel({ app, open, onClose, tab, onTabChange, reloadApp, tree }: AppDetailsPanelProps) {
  const [summary, setSummary] = useState<Edit<SummaryForm> | null>(null);
  const [sources, setSources] = useState<Edit<{ drafts: SourceDraft[]; multi: boolean }> | null>(null);
  const [manifest, setManifest] = useState<Edit<string> | null>(null);
  const saving = useSave();
  const dirty = summary !== null || sources !== null || manifest !== null;
  // Resource-tab extensions registered for Applications (argoproj.io or cd.patrol.example).
  const extensionTabs = useResourceExtensionTabs({ group: 'cd.patrol.example', kind: 'Application', application: app, resource: app, tree });

  /** Rebases the draft onto the latest version (3-way merge, see ./rebase.ts). */
  const rebase = async <T,>(set: SetEdit<T>, merge: (edit: Edit<T>, latest: Application) => { draft: T; report: RebaseReport } | null) => {
    const latest = (await reloadApp()) ?? app;
    set((e) => {
      if (!e) return e;
      const result = merge(e, latest);
      return result ? { base: latest, draft: result.draft, report: result.report } : e;
    });
    saving.update.reset();
  };

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={`${appName(app)} details`}
      description={dirty ? 'You have unsaved changes.' : 'Settings, sources and parameters of the application.'}
      width="960px"
    >
      <Tabs
        label="Application details"
        value={tab}
        onValueChange={onTabChange}
        items={[
          {
            id: 'summary',
            label: summary ? 'Summary (editing)' : 'Summary',
            content: summary ? (
              <SummaryEditor
                key={summary.base.metadata.resourceVersion}
                app={app}
                edit={summary}
                onChange={(draft) => setSummary({ ...summary, draft })}
                onCancel={() => {
                  setSummary(null);
                  saving.update.reset();
                }}
                saving={saving}
                onRebase={() => void rebase(setSummary, (e, latest) => rebaseSummary(e.base, e.draft, latest))}
                onSaved={() => setSummary(null)}
              />
            ) : (
              <SummaryView app={app} onEdit={() => setSummary({ base: app, draft: summaryFormFrom(app) })} />
            ),
          },
          {
            id: 'parameters',
            label: `${isMultiSource(app) ? 'Sources' : 'Parameters'}${sources ? ' (editing)' : ''}`,
            content: (
              <SourcesEditor
                key={sources?.base.metadata.resourceVersion ?? 'view'}
                app={app}
                edit={sources}
                onStart={() => setSources({ base: app, draft: { drafts: sourceDrafts(app), multi: isMultiSource(app) } })}
                onChange={(draft) => setSources((e) => (e ? { ...e, draft } : e))}
                onCancel={() => {
                  setSources(null);
                  saving.update.reset();
                }}
                saving={saving}
                onRebase={() => void rebase(setSources, (e, latest) => rebaseSources(e.base, e.draft, latest))}
                onSaved={() => setSources(null)}
              />
            ),
          },
          {
            id: 'manifest',
            label: manifest ? 'Manifest (editing)' : 'Manifest',
            content: (
              <ManifestEditor
                key={manifest?.base.metadata.resourceVersion ?? 'view'}
                app={app}
                edit={manifest}
                onStart={() => setManifest({ base: app, draft: manifestYAML(app) })}
                onChange={(draft) => setManifest((e) => (e ? { ...e, draft } : e))}
                onCancel={() => {
                  setManifest(null);
                  saving.update.reset();
                }}
                saving={saving}
                onRebase={() => void rebase(setManifest, (e, latest) => rebaseManifest(e.base, e.draft, latest))}
                onSaved={() => setManifest(null)}
              />
            ),
          },
          ...extensionTabs,
        ]}
      />
    </Drawer>
  );
}

type Saving = ReturnType<typeof useSave>;

function labelsView(record: Record<string, string> | undefined): ReactNode {
  const entries = Object.entries(record ?? {});
  if (entries.length === 0) return '';
  return (
    <span style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
      {entries.map(([k, v]) => (
        <Badge key={k}>
          <Mono>{v ? `${k}=${v}` : k}</Mono>
        </Badge>
      ))}
    </span>
  );
}

function SummaryView({ app, onEdit }: { app: Application; onEdit: () => void }) {
  const settings = useSettings();
  const links = useApplicationLinks(appName(app), { appNamespace: app.metadata.namespace, project: app.spec.project });
  const external = applicationExternalLinks(app);
  const managedBy = managedByURL(app);
  const policy = app.spec.syncPolicy;
  const automated = policy?.automated;
  const d = app.spec.destination;
  const retry = policy?.retry;
  const deepLinks = (links.data?.items ?? []).filter((l) => isHttpURL(l.url));
  return (
    <div className={styles.stack}>
      <div className={styles.toolbar}>
        <Button variant="primary" onClick={onEdit}>
          Edit summary
        </Button>
      </div>
      <KeyValueList
        items={[
          { key: 'project', label: 'Project', value: app.spec.project },
          { key: 'namespace', label: 'Application namespace', value: app.metadata.namespace ? <Mono>{app.metadata.namespace}</Mono> : '' },
          { key: 'created', label: 'Created', value: <Timestamp value={app.metadata.creationTimestamp} /> },
          { key: 'labels', label: 'Labels', value: labelsView(app.metadata.labels) },
          { key: 'annotations', label: 'Annotations', value: labelsView(app.metadata.annotations) },
          { key: 'cluster', label: 'Cluster', value: <Mono>{d.name ?? d.server}</Mono> },
          { key: 'destns', label: 'Destination namespace', value: d.namespace ? <Mono>{d.namespace}</Mono> : '' },
          {
            key: 'policy',
            label: 'Sync policy',
            value: isAutoSyncEnabled(app)
              ? `Automated${automated?.prune ? ', prune' : ''}${automated?.selfHeal ? ', self-heal' : ''}${automated?.allowEmpty ? ', allow empty' : ''}`
              : 'Manual',
          },
          { key: 'options', label: 'Sync options', value: policy?.syncOptions?.length ? <Mono>{policy.syncOptions.join(', ')}</Mono> : '' },
          {
            key: 'retry',
            label: 'Retry',
            value: retry ? `Limit ${retry.limit ?? 'default'}, backoff ${retry.backoff?.duration ?? '5s'} ×${retry.backoff?.factor ?? 2} up to ${retry.backoff?.maxDuration ?? '3m'}` : '',
          },
          { key: 'history', label: 'Revision history limit', value: String(app.spec.revisionHistoryLimit ?? 10) },
          ...(app.spec.info ?? []).map((i, n) => ({ key: `info-${n}`, label: i.name, value: isHttpURL(i.value) ? <a href={i.value} target="_blank" rel="noopener noreferrer">{i.value}</a> : i.value })),
          { key: 'images', label: 'Images', value: app.status?.summary?.images?.length ? <Mono>{app.status.summary.images.join(', ')}</Mono> : '' },
        ]}
      />
      {managedBy.kind === 'valid' ? (
        <Banner tone="info" title="Managed by another Patrol instance">
          <a href={managedBy.url} target="_blank" rel="noopener noreferrer">
            Open it in the managing instance <IconExternal size={12} />
          </a>
        </Banner>
      ) : managedBy.kind === 'invalid' ? (
        <Banner tone="warning" title="managed-by-url: invalid url provided">
          The managed-by URL annotation must be a valid http(s) URL; it is ignored.
        </Banner>
      ) : null}
      {deepLinks.length + external.length > 0 ? (
        <section>
          <h3>Links</h3>
          <ul style={{ margin: 0, paddingInlineStart: 'var(--space-7)' }}>
            {deepLinks.map((l) => (
              <li key={`d:${l.url}`}>
                <a href={l.url} target="_blank" rel="noopener noreferrer">
                  {l.title}
                </a>
                {l.description ? ` — ${l.description}` : ''}
              </li>
            ))}
            {external.map((l) => (
              <li key={`e:${l.url}`}>
                <a href={l.url} target="_blank" rel="noopener noreferrer">
                  {l.title ?? l.url}
                </a>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
      <SubscriptionEditor application={app} headingLevel={3} />
      {settings.data?.statusBadgeEnabled ? (
        <section>
          <h3>Status badge</h3>
          <StatusBadge app={app} settings={settings.data} />
        </section>
      ) : null}
    </div>
  );
}

function SaveFooter({ onCancel, saving, label }: { onCancel: () => void; saving: Saving; label: string }) {
  return (
    <div className={styles.footer}>
      <Button onClick={onCancel}>Cancel</Button>
      <Button type="submit" variant="primary" loading={saving.update.isPending}>
        {label}
      </Button>
    </div>
  );
}

interface EditorProps<T> {
  app: Application;
  saving: Saving;
  onCancel: () => void;
  onRebase: () => void;
  onSaved: () => void;
  onChange: (draft: T) => void;
}

function SummaryEditor({ app, edit, onChange, onCancel, saving, onRebase, onSaved }: EditorProps<SummaryForm> & { edit: Edit<SummaryForm> }) {
  const projects = useProjects();
  const settings = useSettings();
  const [attempted, setAttempted] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const form = edit.draft;
  const errors = validateSummary(form);
  const shown = attempted ? errors : {};
  // Rebasing applies the draft first, so it needs a valid draft.
  const tryRebase = () => {
    setAttempted(true);
    if (!hasErrors(errors)) onRebase();
  };
  const set = (patch: Partial<SummaryForm>) => onChange({ ...form, ...patch });
  const projectNames = (projects.data?.items ?? []).map((p) => p.metadata.name ?? '').filter(Boolean);
  if (form.project && !projectNames.includes(form.project)) projectNames.unshift(form.project);

  return (
    <form
      className={styles.stack}
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        setAttempted(true);
        if (hasErrors(errors)) return;
        const result = saving.saveOnto(edit.base, applySummary(edit.base, form), app, 'Summary');
        if (result === 'blocked') setBlocked(true);
        else void result.then((ok) => ok && onSaved());
      }}
    >
      <StaleNotice base={edit.base} app={app} blocked={blocked} onReload={tryRebase} />
      <RebaseNotice report={edit.report} />
      <div className={styles.grid2}>
        <FormField label="Project" required error={shown.project} help={projects.isError ? 'Projects could not be listed; type the project name.' : undefined}>
          {projects.isError ? (
            <Input value={form.project} onChange={(e) => set({ project: e.target.value })} />
          ) : (
            <Select value={form.project} onChange={(e) => set({ project: e.target.value })} options={projectNames.map((p) => ({ value: p, label: p }))} />
          )}
        </FormField>
        <FormField label="Revision history limit" help="Deployments kept in the history (default 10)." error={shown.revisionHistoryLimit}>
          <Input inputMode="numeric" value={form.revisionHistoryLimit} onChange={(e) => set({ revisionHistoryLimit: e.target.value })} />
        </FormField>
      </div>
      <KeyValueEditor legend="Labels" itemLabel="label" rows={form.labels} onChange={(labels) => set({ labels })} errors={shown.labels} />
      <KeyValueEditor legend="Annotations" itemLabel="annotation" rows={form.annotations} onChange={(annotations) => set({ annotations })} errors={shown.annotations} />
      <fieldset className={styles.fieldset}>
        <legend>Destination</legend>
        <div className={styles.stack}>
          <RadioGroup
            legend="Identify the cluster by"
            orientation="horizontal"
            value={form.destinationMode}
            onChange={(destinationMode) => set({ destinationMode })}
            options={[
              { value: 'server', label: 'API server URL' },
              { value: 'name', label: 'Cluster name' },
            ]}
          />
          <div className={styles.grid2}>
            {form.destinationMode === 'server' ? (
              <FormField label="Cluster URL" required error={shown.destination}>
                <Input mono value={form.destinationServer} onChange={(e) => set({ destinationServer: e.target.value })} />
              </FormField>
            ) : (
              <FormField label="Cluster name" required error={shown.destination}>
                <Input mono value={form.destinationName} onChange={(e) => set({ destinationName: e.target.value })} />
              </FormField>
            )}
            <FormField label="Namespace">
              <Input mono value={form.destinationNamespace} onChange={(e) => set({ destinationNamespace: e.target.value })} />
            </FormField>
          </div>
        </div>
      </fieldset>
      <fieldset className={styles.fieldset}>
        <legend>Sync policy</legend>
        <div className={styles.stack}>
          <Switch label="Automated sync" checked={form.automated} onChange={(automated) => set({ automated })} />
          <p className={styles.muted}>Automated sync applies new revisions as soon as the application is out of sync.</p>
          {form.automated ? (
            <>
              <Switch label="Prune resources" checked={form.prune} onChange={(prune) => set({ prune })} />
              <Switch label="Self heal" checked={form.selfHeal} onChange={(selfHeal) => set({ selfHeal })} />
              <Switch label="Allow empty" checked={form.allowEmpty} onChange={(allowEmpty) => set({ allowEmpty })} />
              <p className={styles.muted}>Self heal reverts manual changes in the cluster; allow empty lets an automated sync prune every resource.</p>
            </>
          ) : null}
        </div>
      </fieldset>
      <SyncOptionsFields options={form.syncOptions} onChange={(syncOptions) => set({ syncOptions })} replaceAllowed={settings.data?.syncWithReplaceAllowed ?? false} />
      <RetryFields value={form.retry} onChange={(retry) => set({ retry })} errors={attempted ? validateRetry(form.retry) : {}} legend="Retry (automated syncs)" />
      <fieldset className={styles.fieldset}>
        <legend>Info items</legend>
        {form.info.length === 0 ? <p className={styles.muted}>None.</p> : null}
        <ul className={styles.rows}>
          {form.info.map((item, i) => (
            <li key={i} className={styles.row}>
              <Input size="sm" aria-label={`Name of info item ${i + 1}`} value={item.name} onChange={(e) => set({ info: form.info.map((x, j) => (j === i ? { ...x, name: e.target.value } : x)) })} />
              <Input size="sm" aria-label={`Value of info item ${i + 1}`} value={item.value} onChange={(e) => set({ info: form.info.map((x, j) => (j === i ? { ...x, value: e.target.value } : x)) })} />
              <Button size="sm" variant="ghost" onClick={() => set({ info: form.info.filter((_x, j) => j !== i) })}>
                Remove<span className="sr-only"> info item {i + 1}</span>
              </Button>
            </li>
          ))}
        </ul>
        {shown.info ? <p className={styles.rowError}>{shown.info}</p> : null}
        <Button size="sm" icon={<IconPlus />} onClick={() => set({ info: [...form.info, { name: '', value: '' }] })}>
          Add info item
        </Button>
      </fieldset>
      {attempted && hasErrors(errors) ? (
        <Banner tone="error" live title="Some fields need attention">
          Fix the highlighted fields, then save again.
        </Banner>
      ) : null}
      {saving.update.isError ? <ActionError error={saving.update.error} action="save the application" onReload={tryRebase} /> : null}
      <SaveFooter onCancel={onCancel} saving={saving} label="Save summary" />
    </form>
  );
}

function SourcesEditor({
  app,
  edit,
  onStart,
  onChange,
  onCancel,
  saving,
  onRebase,
  onSaved,
}: EditorProps<{ drafts: SourceDraft[]; multi: boolean }> & { edit: Edit<{ drafts: SourceDraft[]; multi: boolean }> | null; onStart: () => void }) {
  const [attempted, setAttempted] = useState(false);
  const [blocked, setBlocked] = useState(false);
  if (!edit) {
    const sources = appSources(app);
    return (
      <div className={styles.stack}>
        <p className={styles.muted}>
          {sources.length === 0
            ? 'This application has no source.'
            : isMultiSource(app)
              ? `${sources.length} sources are combined into one set of manifests.`
              : 'Repository, revision and tool parameters used to render the manifests.'}
        </p>
        <div className={styles.toolbar}>
          <Button variant="primary" onClick={onStart}>
            Edit {isMultiSource(app) ? 'sources' : 'parameters'}
          </Button>
        </div>
        {sources.map((s, i) => (
          <KeyValueList
            key={i}
            items={[
              ...(sources.length > 1 ? [{ key: 'pos', label: 'Source', value: String(i + 1) }] : []),
              { key: 'repo', label: 'Repository', value: <Mono>{s.repoURL}</Mono> },
              { key: 'rev', label: 'Target revision', value: <Mono>{s.targetRevision ?? 'HEAD'}</Mono> },
              { key: 'path', label: s.chart ? 'Chart' : 'Path', value: <Mono>{s.chart ?? s.path ?? ''}</Mono> },
              { key: 'ref', label: 'Ref', value: s.ref ? <Mono>{s.ref}</Mono> : '' },
              { key: 'tool', label: 'Tool', value: s.helm || s.chart ? 'Helm' : s.kustomize ? 'Kustomize' : s.plugin ? 'Plugin' : s.directory ? 'Directory' : app.status?.sourceTypes?.[i] ?? app.status?.sourceType ?? 'Detected automatically' },
              { key: 'params', label: 'Helm parameters', value: s.helm?.parameters?.length ? s.helm.parameters.map((p) => `${p.name ?? ''}=${p.value ?? ''}`).join(', ') : '' },
            ]}
          />
        ))}
      </div>
    );
  }
  const { drafts, multi } = edit.draft;
  const errors = validateSources(drafts, multi);
  const anyError = errors.some((e) => Object.keys(e).length > 0);
  const savedSources = appSources(edit.base);
  const setDraft = (i: number, d: SourceDraft) => onChange({ drafts: drafts.map((x, j) => (j === i ? d : x)), multi });
  const tryRebase = () => {
    setAttempted(true);
    if (!anyError) onRebase();
  };
  return (
    <form
      className={styles.stack}
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        setAttempted(true);
        if (anyError) return;
        const result = saving.saveOnto(edit.base, applySources(edit.base, drafts, multi), app, multi ? 'Sources' : 'Parameters');
        if (result === 'blocked') setBlocked(true);
        else void result.then((ok) => ok && onSaved());
      }}
    >
      <StaleNotice base={edit.base} app={app} blocked={blocked} onReload={tryRebase} />
      <RebaseNotice report={edit.report} />
      {drafts.map((d, i) => (
        <SourceEditor
          key={i}
          index={i}
          multi={multi}
          draft={d}
          saved={savedSources[i]}
          appName={appName(app)}
          appProject={app.spec.project}
          errors={attempted ? (errors[i] ?? {}) : {}}
          onChange={(next) => setDraft(i, next)}
          onRemove={multi && drafts.length > 1 ? () => onChange({ drafts: drafts.filter((_x, j) => j !== i), multi }) : undefined}
        />
      ))}
      <div className={styles.toolbar}>
        <Button
          icon={<IconPlus />}
          onClick={() => onChange({ drafts: [...drafts, newSourceDraft()], multi: true })}
        >
          Add source
        </Button>
        {!multi ? <span className={styles.muted}>Adding a source turns this into a multi-source application.</span> : null}
      </div>
      {attempted && anyError ? (
        <Banner tone="error" live title="Some fields need attention">
          Fix the highlighted fields, then save again.
        </Banner>
      ) : null}
      {saving.update.isError ? <ActionError error={saving.update.error} action="save the application" onReload={tryRebase} /> : null}
      <SaveFooter onCancel={onCancel} saving={saving} label={multi ? 'Save sources' : 'Save parameters'} />
    </form>
  );
}

function ManifestEditor({ app, edit, onStart, onChange, onCancel, saving, onRebase, onSaved }: EditorProps<string> & { edit: Edit<string> | null; onStart: () => void }) {
  const [attempted, setAttempted] = useState(false);
  const [blocked, setBlocked] = useState(false);
  if (!edit) {
    return (
      <div className={styles.stack}>
        <div className={styles.toolbar}>
          <Button variant="primary" onClick={onStart}>
            Edit manifest
          </Button>
        </div>
        <TextArea mono readOnly rows={24} aria-label="Application manifest (read-only)" className={styles.editor} value={manifestYAML(app)} />
      </div>
    );
  }
  const result = parseManifest(edit.draft, edit.base);
  const tryRebase = () => {
    setAttempted(true);
    if (result.ok) onRebase();
  };
  return (
    <form
      className={styles.stack}
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        setAttempted(true);
        if (!result.ok) return;
        const saved = saving.saveOnto(edit.base, result.app, app, 'Manifest');
        if (saved === 'blocked') setBlocked(true);
        else void saved.then((ok) => ok && onSaved());
      }}
    >
      <StaleNotice base={edit.base} app={app} blocked={blocked} onReload={tryRebase} />
      <RebaseNotice report={edit.report} />
      <p className={styles.muted}>
        Edit labels, annotations, finalizers and the spec. Status and server-managed metadata are not shown; the name and namespace cannot change.
      </p>
      <FormField label="Application manifest (YAML)" error={!result.ok && attempted ? 'The manifest is not valid.' : undefined}>
        <TextArea mono rows={24} spellCheck={false} className={styles.editor} value={edit.draft} onChange={(e) => onChange(e.target.value)} />
      </FormField>
      {!result.ok ? (
        <Banner tone={attempted ? 'error' : 'warning'} live={attempted} title="Validation">
          <ul>
            {result.errors.map((err) => (
              <li key={err}>{err}</li>
            ))}
          </ul>
        </Banner>
      ) : (
        <p className={styles.muted} role="status">
          The manifest is valid.
        </p>
      )}
      {saving.update.isError ? <ActionError error={saving.update.error} action="save the application" onReload={tryRebase} /> : null}
      <SaveFooter onCancel={onCancel} saving={saving} label="Save manifest" />
    </form>
  );
}

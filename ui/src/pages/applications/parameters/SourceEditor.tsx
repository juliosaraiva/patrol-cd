import { useId, useMemo, useState } from 'react';
import { useSourceAppDetails } from '../../../api/hooks/apps-core';
import { usePluginSettings } from '../../../api/queries/resources';
import type { RepositoryAppDetails } from '../../../api/types';
import { Badge, Button, Checkbox, FormField, Input, Mono, SearchInput, Select, TextArea } from '../../../design';
import { IconClose, IconPlus } from '../../../design/icons';
import type { SpecSource } from '../shared/app';
import { KeyValueEditor, LinesField } from './Fields';
import styles from './Parameters.module.css';
import {
  helmParamRows,
  parseAnnouncement,
  setHelmParam,
  setPluginParam,
  setTool,
  toKV,
  toolOf,
  type KV,
  type SourceDraft,
  type SourceErrors,
  type ToolKind,
} from './specEdit';

const TOOLS: { value: ToolKind; label: string }[] = [
  { value: 'auto', label: 'Detect automatically' },
  { value: 'helm', label: 'Helm' },
  { value: 'kustomize', label: 'Kustomize' },
  { value: 'directory', label: 'Directory (plain YAML or Jsonnet)' },
  { value: 'plugin', label: 'Config management plugin' },
];

export interface SourceEditorProps {
  index: number;
  multi: boolean;
  draft: SourceDraft;
  /** Source as currently saved (used to load tool details from the repo server). */
  saved: SpecSource | undefined;
  appName: string;
  appProject: string;
  errors: SourceErrors;
  onChange: (draft: SourceDraft) => void;
  onRemove?: (() => void) | undefined;
}

/** General fields and tool parameters of one source (Helm, Kustomize, Directory, Plugin). */
export function SourceEditor({ index, multi, draft, saved, appName, appProject, errors, onChange, onRemove }: SourceEditorProps) {
  const source = draft.source;
  const tool = toolOf(source);
  const details = useSourceAppDetails(saved, { appName, appProject, ...(multi ? { sourceIndex: index } : {}) }, saved !== undefined && (tool === 'helm' || tool === 'plugin' || tool === 'kustomize'));
  const set = (patch: Partial<SpecSource>) => onChange({ ...draft, source: { ...source, ...patch } });
  const title = multi ? `Source ${index + 1}` : 'Source';
  return (
    <fieldset className={styles.sourceBox}>
      <legend>{title}</legend>
      <div className={styles.stack}>
        <div className={styles.grid2}>
          <FormField label="Repository URL" required error={errors.repoURL}>
            <Input mono value={source.repoURL} onChange={(e) => set({ repoURL: e.target.value })} />
          </FormField>
          <FormField label="Target revision" help="Branch, tag, commit, or chart version (Helm repositories).">
            <Input mono value={source.targetRevision ?? ''} onChange={(e) => set({ targetRevision: e.target.value })} />
          </FormField>
          <FormField label="Path" help="Directory in the repository." error={errors.location}>
            <Input mono value={source.path ?? ''} onChange={(e) => set({ path: e.target.value })} />
          </FormField>
          <FormField label="Chart" help="Chart name, for Helm and OCI repositories.">
            <Input mono value={source.chart ?? ''} onChange={(e) => set({ chart: e.target.value })} />
          </FormField>
          {multi ? (
            <FormField label="Ref" help="Name other sources use as $ref/path in Helm value files." error={errors.ref}>
              <Input mono value={source.ref ?? ''} onChange={(e) => set({ ref: e.target.value })} />
            </FormField>
          ) : null}
          <FormField label="Tool" help="Rendering tool for this source; detection uses the repository content.">
            <Select value={tool} onChange={(e) => onChange({ ...draft, source: setTool(source, e.target.value as ToolKind) })} options={TOOLS} />
          </FormField>
        </div>
        {tool === 'helm' ? <HelmEditor draft={draft} onChange={onChange} errors={errors} details={details.data} detailsFailed={details.isError} /> : null}
        {tool === 'kustomize' ? <KustomizeEditor draft={draft} onChange={onChange} errors={errors} detectedImages={details.data?.kustomize?.images} /> : null}
        {tool === 'directory' ? <DirectoryEditor draft={draft} onChange={onChange} /> : null}
        {tool === 'plugin' ? <PluginEditor draft={draft} onChange={onChange} errors={errors} details={details.data} detailsFailed={details.isError} /> : null}
        {onRemove ? (
          <div>
            <Button size="sm" variant="danger" icon={<IconClose />} onClick={onRemove}>
              Remove {title.toLowerCase()}
            </Button>
          </div>
        ) : null}
      </div>
    </fieldset>
  );
}

const PARAM_PAGE = 50;

function HelmEditor({
  draft,
  onChange,
  errors,
  details,
  detailsFailed,
}: {
  draft: SourceDraft;
  onChange: (d: SourceDraft) => void;
  errors: SourceErrors;
  details: RepositoryAppDetails | undefined;
  detailsFailed: boolean;
}) {
  const helm = draft.source.helm ?? {};
  const setHelm = (patch: Partial<typeof helm>) => onChange({ ...draft, source: { ...draft.source, helm: { ...helm, ...patch } } });
  const [query, setQuery] = useState('');
  const [limit, setLimit] = useState(PARAM_PAGE);
  const [newName, setNewName] = useState('');
  const [newValue, setNewValue] = useState('');
  const rows = useMemo(() => helmParamRows(helm.parameters, details?.helm?.parameters), [helm.parameters, details]);
  const filtered = query ? rows.filter((r) => r.name.toLowerCase().includes(query.toLowerCase())) : rows;
  const valuesId = useId();
  return (
    <div className={styles.stack}>
      <div className={styles.grid2}>
        <FormField label="Release name" help="Defaults to the application name.">
          <Input mono value={helm.releaseName ?? ''} onChange={(e) => setHelm({ releaseName: e.target.value || undefined })} />
        </FormField>
        <FormField label="Value files" help="One path per line, relative to the chart; $ref/path reads from another source.">
          <LinesField value={helm.valueFiles} onChange={(valueFiles) => setHelm({ valueFiles })} />
        </FormField>
      </div>
      {details?.helm?.valueFiles && details.helm.valueFiles.length > 0 ? (
        <p className={styles.muted}>
          Value files in the chart: <Mono>{details.helm.valueFiles.join(', ')}</Mono>
        </p>
      ) : null}
      <FormField label="Values" help="YAML merged over the chart values (string form)." error={errors.values} id={`${valuesId}-values`}>
        <TextArea mono rows={6} value={helm.values ?? ''} onChange={(e) => setHelm({ values: e.target.value || undefined })} />
      </FormField>
      <FormField label="Values object" help="YAML mapping stored as structured values; takes precedence over Values." error={errors.valuesObject} id={`${valuesId}-object`}>
        <TextArea mono rows={6} value={draft.valuesObjectText} onChange={(e) => onChange({ ...draft, valuesObjectText: e.target.value })} />
      </FormField>
      <div className={styles.grid2}>
        <Checkbox label="Skip CRDs" checked={helm.skipCrds ?? false} onChange={(e) => setHelm({ skipCrds: e.target.checked || undefined })} />
        <Checkbox label="Skip tests" checked={helm.skipTests ?? false} onChange={(e) => setHelm({ skipTests: e.target.checked || undefined })} />
        <Checkbox label="Pass credentials to all domains" checked={helm.passCredentials ?? false} onChange={(e) => setHelm({ passCredentials: e.target.checked || undefined })} />
        <Checkbox
          label="Ignore missing value files"
          checked={helm.ignoreMissingValueFiles ?? false}
          onChange={(e) => setHelm({ ignoreMissingValueFiles: e.target.checked || undefined })}
        />
      </div>
      <fieldset className={styles.fieldset}>
        <legend>Parameters</legend>
        {detailsFailed ? <p className={styles.muted}>Chart defaults could not be loaded; only overridden parameters are listed.</p> : null}
        <div className={styles.toolbar}>
          <SearchInput size="sm" aria-label="Filter parameters" value={query} onValueChange={setQuery} />
          <span className={styles.muted}>
            {rows.filter((r) => r.overridden).length} overridden of {rows.length}
          </span>
        </div>
        <div className={styles.paramScroll}>
          <table className={styles.paramTable}>
            <caption className="sr-only">Helm parameters</caption>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Value</th>
                <th scope="col">Source</th>
                <th scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {filtered.slice(0, limit).map((row) => (
                <tr key={row.name}>
                  <th scope="row">
                    <Mono>{row.name}</Mono>
                  </th>
                  <td>
                    <Input
                      size="sm"
                      mono
                      aria-label={`Value of ${row.name}`}
                      value={row.value}
                      onChange={(e) => setHelm({ parameters: setHelmParam(helm.parameters, row.name, e.target.value, row.forceString) })}
                    />
                  </td>
                  <td>{row.overridden ? <Badge tone="accent">Overridden</Badge> : <Badge>Chart default</Badge>}</td>
                  <td>
                    {row.overridden ? (
                      <Button size="sm" variant="ghost" onClick={() => setHelm({ parameters: setHelmParam(helm.parameters, row.name, undefined) })}>
                        {row.defaultValue !== undefined ? 'Reset' : 'Remove'}
                        <span className="sr-only"> {row.name}</span>
                      </Button>
                    ) : null}
                  </td>
                </tr>
              ))}
              {filtered.length === 0 ? (
                <tr>
                  <td colSpan={4} className={styles.muted}>
                    {rows.length === 0 ? 'No parameters. Add an override below.' : 'No parameters match the filter.'}
                  </td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
        {filtered.length > limit ? (
          <Button size="sm" variant="ghost" onClick={() => setLimit(limit + PARAM_PAGE)}>
            Show more parameters ({filtered.length - limit} hidden)
          </Button>
        ) : null}
        <div className={styles.row} style={{ marginTop: 'var(--space-4)' }}>
          <Input size="sm" mono aria-label="New parameter name" placeholder="name (e.g. image.tag)" value={newName} onChange={(e) => setNewName(e.target.value)} />
          <Input size="sm" mono aria-label="New parameter value" placeholder="value" value={newValue} onChange={(e) => setNewValue(e.target.value)} />
          <Button
            size="sm"
            icon={<IconPlus />}
            disabled={!newName.trim()}
            onClick={() => {
              setHelm({ parameters: setHelmParam(helm.parameters, newName.trim(), newValue) });
              setNewName('');
              setNewValue('');
            }}
          >
            Add parameter
          </Button>
        </div>
      </fieldset>
    </div>
  );
}

function KustomizeEditor({
  draft,
  onChange,
  errors,
  detectedImages,
}: {
  draft: SourceDraft;
  onChange: (d: SourceDraft) => void;
  errors: SourceErrors;
  detectedImages: string[] | undefined;
}) {
  const k = draft.source.kustomize ?? {};
  const setK = (patch: Partial<typeof k>) => onChange({ ...draft, source: { ...draft.source, kustomize: { ...k, ...patch } } });
  const replicas = (k.replicas ?? []).map((r) => ({ key: r.name, value: String(r.count) }));
  return (
    <div className={styles.stack}>
      <div className={styles.grid2}>
        <FormField label="Name prefix">
          <Input mono value={k.namePrefix ?? ''} onChange={(e) => setK({ namePrefix: e.target.value || undefined })} />
        </FormField>
        <FormField label="Name suffix">
          <Input mono value={k.nameSuffix ?? ''} onChange={(e) => setK({ nameSuffix: e.target.value || undefined })} />
        </FormField>
        <FormField label="Namespace" help="Overrides the namespace of every resource.">
          <Input mono value={k.namespace ?? ''} onChange={(e) => setK({ namespace: e.target.value || undefined })} />
        </FormField>
        <FormField label="Kustomize version" help="Named version configured on the server; empty uses the default.">
          <Input mono value={k.version ?? ''} onChange={(e) => setK({ version: e.target.value || undefined })} />
        </FormField>
      </div>
      <FormField label="Images" help="One per line: name=new-name:tag, name:tag or name@digest.">
        <LinesField value={k.images} onChange={(images) => setK({ images })} rows={4} />
      </FormField>
      {detectedImages && detectedImages.length > 0 ? (
        <p className={styles.muted}>
          Images found in the kustomization: <Mono>{detectedImages.join(', ')}</Mono>
        </p>
      ) : null}
      <KeyValueEditor
        legend="Common labels"
        itemLabel="common label"
        rows={draft.commonLabels}
        onChange={(commonLabels) => onChange({ ...draft, commonLabels })}
        errors={errors.commonLabels}
      />
      <Checkbox label="Force common labels" description="Overwrite existing label values." checked={k.forceCommonLabels ?? false} onChange={(e) => setK({ forceCommonLabels: e.target.checked || undefined })} />
      <KeyValueEditor
        legend="Common annotations"
        itemLabel="common annotation"
        rows={draft.commonAnnotations}
        onChange={(commonAnnotations) => onChange({ ...draft, commonAnnotations })}
        errors={errors.commonAnnotations}
      />
      <Checkbox
        label="Force common annotations"
        description="Overwrite existing annotation values."
        checked={k.forceCommonAnnotations ?? false}
        onChange={(e) => setK({ forceCommonAnnotations: e.target.checked || undefined })}
      />
      <KeyValueEditor
        legend="Replicas"
        itemLabel="replica override"
        keyLabel="Resource name"
        valueLabel="Count"
        rows={replicas}
        onChange={(rows) =>
          setK({
            replicas: rows.length > 0 ? rows.map((r) => ({ name: r.key, count: /^\d+$/.test(r.value) ? Number(r.value) : r.value })) : undefined,
          })
        }
      />
      <FormField label="Components" help="One component path per line.">
        <LinesField value={k.components} onChange={(components) => setK({ components })} />
      </FormField>
      <FormField label="Patches" help="YAML list of patches, each with patch or path and an optional target." error={errors.patches}>
        <TextArea mono rows={8} value={draft.patchesText} onChange={(e) => onChange({ ...draft, patchesText: e.target.value })} />
      </FormField>
    </div>
  );
}

type JsonnetVar = { name: string; value: string; code?: boolean };

function VarRows({ legend, rows, onChange }: { legend: string; rows: readonly JsonnetVar[]; onChange: (rows: JsonnetVar[] | undefined) => void }) {
  const update = (i: number, patch: Partial<JsonnetVar>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const singular = legend.replace(/s$/, '').toLowerCase();
  return (
    <fieldset className={styles.fieldset}>
      <legend>{legend}</legend>
      {rows.length === 0 ? <p className={styles.muted}>None.</p> : null}
      <ul className={styles.rows}>
        {rows.map((row, i) => (
          <li key={i} className={styles.row3}>
            <Input size="sm" mono aria-label={`Name of ${singular} ${i + 1}`} value={row.name} onChange={(e) => update(i, { name: e.target.value })} />
            <Input size="sm" mono aria-label={`Value of ${singular} ${i + 1}`} value={row.value} onChange={(e) => update(i, { value: e.target.value })} />
            <Checkbox label="Code" checked={row.code ?? false} onChange={(e) => update(i, { code: e.target.checked || undefined })} />
            <Button
              size="sm"
              variant="ghost"
              onClick={() => {
                const next = rows.filter((_r, j) => j !== i);
                onChange(next.length > 0 ? next : undefined);
              }}
            >
              Remove<span className="sr-only"> {singular} {i + 1}</span>
            </Button>
          </li>
        ))}
      </ul>
      <Button size="sm" icon={<IconPlus />} onClick={() => onChange([...rows, { name: '', value: '' }])}>
        Add {singular}
      </Button>
    </fieldset>
  );
}

function DirectoryEditor({ draft, onChange }: { draft: SourceDraft; onChange: (d: SourceDraft) => void }) {
  const dir = draft.source.directory ?? {};
  const jsonnet = dir.jsonnet ?? {};
  const setDir = (patch: Partial<typeof dir>) => onChange({ ...draft, source: { ...draft.source, directory: { ...dir, ...patch } } });
  const setJsonnet = (patch: Partial<typeof jsonnet>) => setDir({ jsonnet: { ...jsonnet, ...patch } });
  return (
    <div className={styles.stack}>
      <Checkbox label="Recurse into subdirectories" checked={dir.recurse ?? false} onChange={(e) => setDir({ recurse: e.target.checked || undefined })} />
      <div className={styles.grid2}>
        <FormField label="Include" help="Glob of files to include, e.g. {*.yaml,*.jsonnet}.">
          <Input mono value={dir.include ?? ''} onChange={(e) => setDir({ include: e.target.value || undefined })} />
        </FormField>
        <FormField label="Exclude" help="Glob of files to skip, e.g. config/*.">
          <Input mono value={dir.exclude ?? ''} onChange={(e) => setDir({ exclude: e.target.value || undefined })} />
        </FormField>
      </div>
      <VarRows legend="External variables" rows={jsonnet.extVars ?? []} onChange={(extVars) => setJsonnet({ extVars })} />
      <VarRows legend="Top-level arguments" rows={jsonnet.tlas ?? []} onChange={(tlas) => setJsonnet({ tlas })} />
      <FormField label="Jsonnet library paths" help="One path per line.">
        <LinesField value={jsonnet.libs} onChange={(libs) => setJsonnet({ libs })} />
      </FormField>
    </div>
  );
}

function PluginEditor({
  draft,
  onChange,
  errors,
  details,
  detailsFailed,
}: {
  draft: SourceDraft;
  onChange: (d: SourceDraft) => void;
  errors: SourceErrors;
  details: RepositoryAppDetails | undefined;
  detailsFailed: boolean;
}) {
  const plugins = usePluginSettings();
  const plugin = draft.source.plugin ?? {};
  const setPlugin = (patch: Partial<typeof plugin>) => onChange({ ...draft, source: { ...draft.source, plugin: { ...plugin, ...patch } } });
  // Map parameters are edited as rows in the draft (see SourceDraft.pluginMaps); untouched
  // announced maps show the announced default and are not written.
  const mapRows = (name: string, fallback: Record<string, string> | undefined): KV[] => draft.pluginMaps[name] ?? toKV(fallback);
  const setMapRows = (name: string, rows: KV[]) => onChange({ ...draft, pluginMaps: { ...draft.pluginMaps, [name]: rows } });
  const announced = parseAnnouncement(details?.plugin);
  const announcedNames = new Set(announced.map((a) => a.name));
  const params = plugin.parameters ?? [];
  const extra = params.filter((p): p is (typeof params)[number] & { name: string } => typeof p.name === 'string' && p.name !== '' && !announcedNames.has(p.name));
  const available = (plugins.data?.plugins ?? []).map((p) => p.name);
  const env = (plugin.env ?? []).map((e) => ({ key: e.name, value: e.value }));
  return (
    <div className={styles.stack}>
      <FormField label="Plugin name" help={available.length > 0 ? `Available plugins: ${available.join(', ')}. Leave empty to let the plugin discover the source.` : 'Leave empty to let the plugin discover the source.'}>
        <Input mono value={plugin.name ?? ''} onChange={(e) => setPlugin({ name: e.target.value || undefined })} />
      </FormField>
      <KeyValueEditor
        legend="Environment"
        itemLabel="environment variable"
        keyLabel="Name"
        rows={env}
        onChange={(rows) => setPlugin({ env: rows.length > 0 ? rows.map((r) => ({ name: r.key, value: r.value })) : undefined })}
      />
      <fieldset className={styles.fieldset}>
        <legend>Parameters</legend>
        {detailsFailed ? <p className={styles.muted}>The plugin's parameter announcement could not be loaded; only parameters already set are shown.</p> : null}
        {announced.length === 0 && extra.length === 0 ? <p className={styles.muted}>This plugin announces no parameters.</p> : null}
        <div className={styles.stack}>
          {announced.map((a) => {
            const current = params.find((p) => p.name === a.name);
            if (a.collectionType === 'array') {
              return (
                <FormField key={a.name} label={a.title} help={a.tooltip} required={a.required}>
                  <LinesField value={current?.array ?? a.array} onChange={(array) => setPlugin({ parameters: setPluginParam(params, { name: a.name, array }) })} />
                </FormField>
              );
            }
            if (a.collectionType === 'map') {
              return (
                <KeyValueEditor
                  key={a.name}
                  legend={a.title}
                  itemLabel={`${a.title} entry`}
                  rows={mapRows(a.name, current?.map ?? a.map)}
                  onChange={(rows) => setMapRows(a.name, rows)}
                  errors={errors.pluginMaps?.[a.name]}
                />
              );
            }
            return (
              <FormField key={a.name} label={a.title} help={a.tooltip} required={a.required}>
                <Input mono value={current?.string ?? a.string ?? ''} onChange={(e) => setPlugin({ parameters: setPluginParam(params, { name: a.name, string: e.target.value }) })} />
              </FormField>
            );
          })}
          {extra.map((p) =>
            p.map ? (
              <KeyValueEditor
                key={p.name}
                legend={`${p.name} (not announced by the plugin)`}
                itemLabel={`${p.name} entry`}
                rows={mapRows(p.name, p.map)}
                onChange={(rows) => setMapRows(p.name, rows)}
                errors={errors.pluginMaps?.[p.name]}
              />
            ) : (
              <FormField key={p.name} label={<Mono>{p.name}</Mono>} help="Set on the application but not announced by the plugin.">
                {p.array ? (
                  <LinesField value={p.array} onChange={(array) => setPlugin({ parameters: setPluginParam(params, { name: p.name, array }) })} />
                ) : (
                  <Input mono value={p.string ?? ''} onChange={(e) => setPlugin({ parameters: setPluginParam(params, { name: p.name, string: e.target.value }) })} />
                )}
              </FormField>
            ),
          )}
        </div>
      </fieldset>
    </div>
  );
}

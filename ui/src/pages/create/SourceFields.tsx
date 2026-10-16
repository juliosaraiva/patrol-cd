import { useId, useState } from 'react';
import { useRepoApps, useRepoAppDetails, useRepoHelmCharts, useRepoOCITags, useRepoRefs } from '../../api/hooks/create';
import { isApiError } from '../../api/errors';
import type { ApplicationSource, Repository } from '../../api/types';
import { FormField, IconButton, Input, Select } from '../../design';
import { IconClose } from '../../design/icons';
import { applyKind, produce, sourceKind, type FieldErrors, type SourceKind } from './model';
import { ParametersFields } from './ParametersFields';
import { useCommitted } from './useCommitted';
import styles from './Create.module.css';

const KIND_LABELS: Record<SourceKind, string> = { git: 'Git', helm: 'Helm', oci: 'OCI' };

function lookupHelp(query: { isError: boolean; error: unknown; isFetching: boolean }, what: string): string | undefined {
  if (query.isFetching) return `Loading ${what}…`;
  if (query.isError) return `${what[0]?.toUpperCase() ?? ''}${what.slice(1)} could not be loaded${isApiError(query.error) ? ` (${query.error.detail})` : ''}; type a value.`;
  return undefined;
}

export interface SourceFieldsProps {
  index: number;
  multi: boolean;
  source: ApplicationSource;
  onChange: (next: ApplicationSource) => void;
  onRemove?: (() => void) | undefined;
  repos: Repository[];
  project: string;
  appName: string;
  pluginNames: string[];
  errors: FieldErrors;
  showErrors: boolean;
}

/**
 * One application source: repository (picker of registered repositories), type, revision
 * (branches and tags, chart versions or OCI tags), path (suggested from the repository) or
 * chart, ref key for multi-source, then the tool parameters.
 */
export function SourceFields({ index, multi, source, onChange, onRemove, repos, project, appName, pluginNames, errors, showErrors }: SourceFieldsProps) {
  const id = `sources.${index}`;
  const lists = { repos: useId(), revisions: useId(), paths: useId(), charts: useId(), versions: useId() };
  const [focus, setFocus] = useState<string | null>(null);
  const kind = sourceKind(source, repos);
  const registered = repos.some((r) => r.spec.url === source.repoURL);
  const repoURL = useCommitted(source.repoURL, focus === 'repoURL', registered);
  const revision = useCommitted(source.targetRevision ?? '', focus === 'targetRevision');
  const path = useCommitted(source.path ?? '', focus === 'path');
  const chart = useCommitted(source.chart ?? '', focus === 'chart');

  const refs = useRepoRefs(kind === 'git' ? repoURL : '', project);
  const ociTags = useRepoOCITags(repoURL, project, kind === 'oci');
  const charts = useRepoHelmCharts(repoURL, project, kind === 'helm');
  const apps = useRepoApps(repoURL, { revision: revision || undefined, appProject: project }, kind !== 'helm');
  const detailsSource: ApplicationSource | null =
    repoURL !== '' && (kind === 'helm' ? chart !== '' : path !== '')
      ? {
          repoURL,
          targetRevision: revision || undefined,
          ...(kind === 'helm' ? { chart } : { path }),
          ...(source.helm?.valueFiles?.length ? { helm: { valueFiles: source.helm.valueFiles } } : {}),
        }
      : null;
  const details = useRepoAppDetails(detailsSource, { appName: appName || undefined, appProject: project });

  const update = (recipe: (s: ApplicationSource) => void) => onChange(produce(source, recipe));
  const err = (field: string) => (showErrors ? errors[`${id}.${field}`] : undefined);
  const focusProps = (field: string) => ({ onFocus: () => setFocus(field), onBlur: () => setFocus(null) });

  const revisions =
    kind === 'git'
      ? [...(refs.data?.branches ?? []).map((b) => ({ value: b, label: 'branch' })), ...(refs.data?.tags ?? []).map((t) => ({ value: t, label: 'tag' }))]
      : kind === 'oci'
        ? (ociTags.data?.tags ?? []).map((t) => ({ value: t, label: 'tag' }))
        : ((charts.data?.items ?? []).find((c) => c.name === source.chart)?.versions ?? []).map((v) => ({ value: v, label: 'version' }));

  return (
    <fieldset className={styles.sourceFieldset}>
      <legend className={styles.legend}>{multi ? `Source ${index + 1}` : 'Source'}</legend>
      {multi && onRemove ? (
        <div className={styles.sourceActions}>
          <IconButton aria-label={`Remove source ${index + 1}`} icon={<IconClose />} onClick={onRemove} />
        </div>
      ) : null}
      <datalist id={lists.repos}>
        {repos.map((r) => (
          <option key={r.metadata.name ?? r.spec.url} value={r.spec.url} label={`${r.spec.name ?? r.metadata.name ?? ''} (${KIND_LABELS[r.spec.type ?? 'git']})`} />
        ))}
      </datalist>
      <div className={styles.row}>
        <FormField
          label="Repository URL"
          required
          id={`${id}.repoURL`}
          error={err('repoURL')}
          help={repos.length === 0 ? 'No repositories are registered; public repositories work without registration.' : 'Choose a registered repository or type a URL.'}
          className={styles.grow}
        >
          <Input
            mono
            list={lists.repos}
            autoComplete="off"
            placeholder="https://github.com/org/repo.git"
            value={source.repoURL}
            onChange={(e) => {
              const url = e.target.value;
              const repo = repos.find((r) => r.spec.url === url);
              onChange(repo ? applyKind({ ...source, repoURL: url }, repo.spec.type ?? 'git') : { ...source, repoURL: url });
            }}
            {...focusProps('repoURL')}
          />
        </FormField>
        <FormField label="Type" id={`${id}.kind`}>
          <Select value={kind} disabled={registered} onChange={(e) => onChange(applyKind(source, e.target.value as SourceKind))}>
            {(Object.keys(KIND_LABELS) as SourceKind[]).map((k) => (
              <option key={k} value={k}>
                {KIND_LABELS[k]}
              </option>
            ))}
          </Select>
        </FormField>
      </div>

      <datalist id={lists.revisions}>
        {revisions.map((r) => (
          <option key={`${r.label}-${r.value}`} value={r.value} label={r.label} />
        ))}
      </datalist>
      {kind === 'helm' ? (
        <>
          <datalist id={lists.charts}>
            {(charts.data?.items ?? []).map((c) => (
              <option key={c.name} value={c.name} />
            ))}
          </datalist>
          <div className={styles.row}>
            <FormField label="Chart" required id={`${id}.chart`} error={err('chart')} help={lookupHelp(charts, 'charts')} className={styles.grow}>
              <Input mono list={lists.charts} autoComplete="off" value={source.chart ?? ''} onChange={(e) => update((s) => void (s.chart = e.target.value))} {...focusProps('chart')} />
            </FormField>
            <FormField label="Version" required id={`${id}.targetRevision`} error={err('targetRevision')} help="A chart version or a semver range such as 1.*.">
              <Input mono list={lists.revisions} autoComplete="off" value={source.targetRevision ?? ''} onChange={(e) => update((s) => void (s.targetRevision = e.target.value))} {...focusProps('targetRevision')} />
            </FormField>
          </div>
        </>
      ) : (
        <>
          <datalist id={lists.paths}>
            {(apps.data?.items ?? []).map((a) => (
              <option key={a.path} value={a.path} label={a.type} />
            ))}
          </datalist>
          <div className={styles.row}>
            <FormField
              label="Revision"
              id={`${id}.targetRevision`}
              error={err('targetRevision')}
              help={lookupHelp(kind === 'git' ? refs : ociTags, kind === 'git' ? 'branches and tags' : 'tags') ?? (kind === 'git' ? 'Branch, tag or commit; HEAD follows the default branch.' : 'Artifact tag or digest.')}
            >
              <Input mono list={lists.revisions} autoComplete="off" value={source.targetRevision ?? ''} onChange={(e) => update((s) => void (s.targetRevision = e.target.value))} {...focusProps('targetRevision')} />
            </FormField>
            <FormField label="Path" required={!(multi && source.ref)} id={`${id}.path`} error={err('path')} help={lookupHelp(apps, 'paths') ?? 'Directory with the manifests; suggestions come from the repository.'} className={styles.grow}>
              <Input mono list={lists.paths} autoComplete="off" value={source.path ?? ''} onChange={(e) => update((s) => void (s.path = e.target.value))} {...focusProps('path')} />
            </FormField>
          </div>
        </>
      )}

      {multi ? (
        <div className={styles.row}>
          <FormField
            label="Ref"
            id={`${id}.ref`}
            error={err('ref')}
            help='Name other sources use to read this one, e.g. "$values/env/prod.yaml". A source with a ref and no path only provides files.'
          >
            <Input mono value={source.ref ?? ''} onChange={(e) => update((s) => void (s.ref = e.target.value))} />
          </FormField>
          <FormField label="Name" id={`${id}.name`} help="Optional label for this source.">
            <Input value={source.name ?? ''} onChange={(e) => update((s) => void (s.name = e.target.value))} />
          </FormField>
        </div>
      ) : null}

      {multi && source.ref && !source.path && !source.chart ? null : (
        <ParametersFields idPrefix={id} source={source} onChange={onChange} details={details} pluginNames={pluginNames} />
      )}
    </fieldset>
  );
}

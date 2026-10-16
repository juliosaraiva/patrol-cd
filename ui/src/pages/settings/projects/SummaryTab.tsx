import { useState, type ReactNode } from 'react';
import { Link } from 'react-router';
import { useProjectLinks } from '../../../api/hooks/settings';
import type { ProjectDetailed } from '../../../api/types';
import { isHttpURL } from '../../../lib/url';
import { Badge, FormField, KeyValueList, Mono, Table, TextArea, Timestamp } from '../../../design';
import { clusterPath } from '../clusters/clusterUtil';
import { mapToRows, nonEmpty, rowsToMap, RowListEditor, StringListEditor, type Row } from '../common/editors';
import { useFormErrors, type FieldError } from '../common/feedback';
import styles from '../common/common.module.css';
import { validateLabelKey, validateLabelValue } from '../common/validation';
import { ProjectSection, SectionDrawer, useProjectSave, type ProjectContext } from './shared';

type Editing = 'general' | 'repos' | 'namespaces' | 'destinations' | 'serviceAccounts' | null;

function ListView({ values, empty, label }: { values: readonly string[] | undefined; empty: string; label: string }) {
  if (!values || values.length === 0) return <p className={styles.muted}>{empty}</p>;
  return (
    <ul className={styles.tagList} aria-label={label}>
      {values.map((v) => (
        <li key={v}>
          <Badge tone={v.startsWith('!') ? 'warning' : 'neutral'}>
            <Mono>{v}</Mono>
            {v.startsWith('!') ? <span className="sr-only"> (denied)</span> : null}
          </Badge>
        </li>
      ))}
    </ul>
  );
}

function rowErrors(rows: readonly Row[], check: (row: Row) => string | null): (string | null)[] {
  return rows.map(check);
}

function collect(prefix: string, firstKey: string, noun: string, errors: readonly (string | null)[]): FieldError[] {
  return errors.flatMap((e, i) => (e ? [{ id: `${prefix}-${i}-${firstKey}`, message: `${noun} ${i + 1}: ${e}` }] : []));
}

const validateSourceRepo = (value: string) => {
  const v = value.trim();
  if (v === '') return 'Enter a repository URL or pattern.';
  if (v === '!') return 'A "!" must be followed by a URL pattern.';
  return null;
};

/** Summary tab: general information, sources, destinations and service accounts (API-105, TEN-064). */
export function SummaryTab({ ctx, detailed }: { ctx: ProjectContext; detailed: ProjectDetailed | undefined }) {
  const { project } = ctx;
  const spec = project.spec;
  const name = project.metadata.name ?? '';
  const saver = useProjectSave(ctx);
  const projectLinks = useProjectLinks(name);
  const form = useFormErrors();
  const [editing, setEditing] = useState<Editing>(null);
  const [description, setDescription] = useState('');
  const [labels, setLabels] = useState<Row[]>([]);
  const [list, setList] = useState<string[]>([]);
  const [rows, setRows] = useState<Row[]>([]);

  const open = (which: Exclude<Editing, null>) => {
    form.reset();
    saver.reset();
    setDescription(spec.description ?? '');
    setLabels(mapToRows(project.metadata.labels));
    setList(which === 'repos' ? [...(spec.sourceRepos ?? [])] : which === 'namespaces' ? [...(spec.sourceNamespaces ?? [])] : []);
    if (which === 'destinations') setRows((spec.destinations ?? []).map((d) => ({ server: d.server ?? '', name: d.name ?? '', namespace: d.namespace ?? '' })));
    if (which === 'serviceAccounts') {
      setRows((spec.destinationServiceAccounts ?? []).map((d) => ({ server: d.server, namespace: d.namespace ?? '', defaultServiceAccount: d.defaultServiceAccount })));
    }
    setEditing(which);
  };
  const close = () => setEditing(null);

  const labelErrors = rowErrors(labels, (r) => validateLabelKey(r.key ?? '') ?? validateLabelValue(r.value ?? ''));
  const listErrors = list.map((v) => (editing === 'repos' ? validateSourceRepo(v) : v.trim() === '' ? 'Enter a namespace or pattern.' : null));
  const destinationErrors = rowErrors(rows, (r) => {
    if (!(r.server ?? '').trim() && !(r.name ?? '').trim()) return 'Enter a server URL or a cluster name (globs allowed).';
    if (!(r.namespace ?? '').trim()) return 'Enter a namespace, or * for all namespaces.';
    return null;
  });
  const serviceAccountErrors = rowErrors(rows, (r) => {
    if (!(r.server ?? '').trim()) return 'Enter the destination server URL (globs allowed).';
    if (!(r.defaultServiceAccount ?? '').trim()) return 'Enter the service account to impersonate.';
    return null;
  });

  const submit = () => {
    switch (editing) {
      case 'general':
        if (!form.check(collect('project-label', 'key', 'Label', labelErrors))) return;
        saver.save((s) => ({ ...s, description: description.trim() || undefined }), 'Project updated', close, { labels: rowsToMap(labels) });
        return;
      case 'repos':
        if (!form.check(collect('project-repo', 'value', 'Repository', listErrors))) return;
        saver.save((s) => ({ ...s, sourceRepos: nonEmpty(list) }), 'Source repositories updated', close);
        return;
      case 'namespaces':
        if (!form.check(collect('project-srcns', 'value', 'Namespace', listErrors))) return;
        saver.save((s) => ({ ...s, sourceNamespaces: nonEmpty(list) }), 'Source namespaces updated', close);
        return;
      case 'destinations':
        if (!form.check(collect('project-dest', 'server', 'Destination', destinationErrors))) return;
        saver.save(
          (s) => ({
            ...s,
            destinations: rows.map((r) => ({
              server: (r.server ?? '').trim() || undefined,
              name: (r.name ?? '').trim() || undefined,
              namespace: (r.namespace ?? '').trim(),
            })),
          }),
          'Destinations updated',
          close,
        );
        return;
      case 'serviceAccounts':
        if (!form.check(collect('project-sa', 'server', 'Service account', serviceAccountErrors))) return;
        saver.save(
          (s) => ({
            ...s,
            destinationServiceAccounts: rows.map((r) => ({
              server: (r.server ?? '').trim(),
              namespace: (r.namespace ?? '').trim() || undefined,
              defaultServiceAccount: (r.defaultServiceAccount ?? '').trim(),
            })),
          }),
          'Destination service accounts updated',
          close,
        );
        return;
      default:
        return;
    }
  };

  const drawer = (title: string, body: ReactNode, description_?: ReactNode) => (
    <SectionDrawer
      title={title}
      description={description_}
      onClose={close}
      onSubmit={submit}
      pending={saver.pending}
      error={saver.error}
      onReload={ctx.reload}
      errors={form.errors}
      attempt={form.attempt}
    >
      {body}
    </SectionDrawer>
  );

  const labelEntries = Object.entries(project.metadata.labels ?? {});
  const links = (projectLinks.data?.items ?? []).filter((l) => isHttpURL(l.url));
  const globalProjects = detailed?.globalProjects ?? [];

  return (
    <div className={styles.stack}>
      <ProjectSection id="project-general" title="General" onEdit={() => open('general')} editLabel="Edit general information">
        <KeyValueList
          items={[
            { key: 'name', label: 'Name', value: <Mono>{name}</Mono> },
            { key: 'description', label: 'Description', value: spec.description },
            {
              key: 'labels',
              label: 'Labels',
              value:
                labelEntries.length > 0 ? (
                  <ul className={styles.tagList} aria-label="Labels">
                    {labelEntries.map(([k, v]) => (
                      <li key={k}>
                        <Badge>
                          <Mono>
                            {k}={v}
                          </Mono>
                        </Badge>
                      </li>
                    ))}
                  </ul>
                ) : (
                  'None'
                ),
            },
            { key: 'created', label: 'Created', value: <Timestamp value={project.metadata.creationTimestamp} /> },
            {
              key: 'links',
              label: 'Links',
              value:
                links.length > 0 ? (
                  <ul className={styles.tagList} aria-label="Project links">
                    {links.map((l) => (
                      <li key={l.url}>
                        <a href={l.url} target="_blank" rel="noopener noreferrer" title={l.description}>
                          {l.title}
                          <span className="sr-only"> (opens in a new tab)</span>
                        </a>
                      </li>
                    ))}
                  </ul>
                ) : undefined,
            },
          ]}
        />
      </ProjectSection>

      <ProjectSection
        id="project-repos"
        title="Source repositories"
        description="Repositories applications in this project may deploy from. Patterns use globs; a leading ! denies matches and wins over allows."
        onEdit={() => open('repos')}
        editLabel="Edit source repositories"
      >
        <ListView values={spec.sourceRepos} empty="No source repositories: applications in this project cannot deploy anything." label="Source repositories" />
      </ProjectSection>

      <ProjectSection
        id="project-srcns"
        title="Source namespaces"
        description="Namespaces outside the control-plane namespace where Applications of this project may be created."
        onEdit={() => open('namespaces')}
        editLabel="Edit source namespaces"
      >
        <ListView values={spec.sourceNamespaces} empty="Only the control-plane namespace." label="Source namespaces" />
      </ProjectSection>

      <ProjectSection
        id="project-destinations"
        title="Destinations"
        description="Clusters and namespaces applications may deploy to. Server, name and namespace accept globs; prefix a value with ! to deny it."
        onEdit={() => open('destinations')}
        editLabel="Edit destinations"
      >
        {(spec.destinations ?? []).length === 0 ? (
          <p className={styles.muted}>No destinations: applications in this project cannot deploy anywhere.</p>
        ) : (
          <Table
            caption="Destinations"
            className={styles.mediumTable}
            rows={spec.destinations ?? []}
            getRowKey={(d) => `${d.server ?? ''}|${d.name ?? ''}|${d.namespace ?? ''}`}
            columns={[
              { id: 'server', header: 'Server', cell: (d) => (d.server ? <Mono title={d.server}>{d.server}</Mono> : '—') },
              { id: 'name', header: 'Cluster name', width: '160px', cell: (d) => (d.name ? <Mono>{d.name}</Mono> : '—') },
              { id: 'namespace', header: 'Namespace', width: '160px', cell: (d) => <Mono>{d.namespace ?? '—'}</Mono> },
            ]}
          />
        )}
      </ProjectSection>

      <ProjectSection
        id="project-sa"
        title="Destination service accounts"
        description="Service accounts impersonated when syncing to matching destinations (requires impersonation to be enabled)."
        onEdit={() => open('serviceAccounts')}
        editLabel="Edit destination service accounts"
      >
        {(spec.destinationServiceAccounts ?? []).length === 0 ? (
          <p className={styles.muted}>None: syncs use the control-plane service account.</p>
        ) : (
          <Table
            caption="Destination service accounts"
            className={styles.mediumTable}
            rows={spec.destinationServiceAccounts ?? []}
            getRowKey={(d) => `${d.server}|${d.namespace ?? ''}|${d.defaultServiceAccount}`}
            columns={[
              { id: 'server', header: 'Server', cell: (d) => <Mono title={d.server}>{d.server}</Mono> },
              { id: 'namespace', header: 'Namespace', width: '160px', cell: (d) => <Mono>{d.namespace ?? '*'}</Mono> },
              { id: 'sa', header: 'Service account', width: '200px', cell: (d) => <Mono>{d.defaultServiceAccount}</Mono> },
            ]}
          />
        )}
      </ProjectSection>

      <ProjectSection id="project-scoped" title="Project-scoped repositories and clusters" description="Repositories and clusters registered for this project only.">
        <KeyValueList
          items={[
            {
              key: 'repos',
              label: 'Repositories',
              value:
                (detailed?.repositories ?? []).length > 0 ? (
                  <ul className={styles.tagList} aria-label="Scoped repositories">
                    {(detailed?.repositories ?? []).map((r) => (
                      <li key={r.spec.url}>
                        <Mono>{r.spec.url}</Mono>
                      </li>
                    ))}
                  </ul>
                ) : (
                  'None'
                ),
            },
            {
              key: 'clusters',
              label: 'Clusters',
              value:
                (detailed?.clusters ?? []).length > 0 ? (
                  <ul className={styles.tagList} aria-label="Scoped clusters">
                    {(detailed?.clusters ?? []).map((c) => (
                      <li key={c.spec.server}>
                        <Link to={clusterPath(c.spec.server)}>{c.spec.name}</Link>
                      </li>
                    ))}
                  </ul>
                ) : (
                  'None'
                ),
            },
          ]}
        />
      </ProjectSection>

      {globalProjects.length > 0 ? (
        <ProjectSection id="project-global" title="Inherited from global projects" description="Settings merged into this project from global projects whose selector matches its labels.">
          <ul className={styles.tagList} aria-label="Global projects">
            {globalProjects.map((g) => (
              <li key={g.metadata.name}>
                <Link to={`/settings/projects/${encodeURIComponent(g.metadata.name ?? '')}`}>{g.metadata.name}</Link>
              </li>
            ))}
          </ul>
        </ProjectSection>
      ) : null}

      {editing === 'general'
        ? drawer(
            'Edit general information',
            <>
              <FormField label="Description" id="project-description">
                <TextArea value={description} onChange={(e) => setDescription(e.target.value)} rows={3} />
              </FormField>
              <RowListEditor
                legend="Labels"
                noun="label"
                idPrefix="project-label"
                rows={labels}
                onChange={setLabels}
                fields={[
                  { key: 'key', label: 'Key', mono: true },
                  { key: 'value', label: 'Value', mono: true },
                ]}
                errors={form.submitted ? labelErrors : undefined}
                help="Global projects select the projects they apply to by label."
              />
            </>,
          )
        : null}
      {editing === 'repos'
        ? drawer(
            'Edit source repositories',
            <StringListEditor
              legend="Source repositories"
              label="Repository URL or pattern"
              noun="repository"
              idPrefix="project-repo"
              values={list}
              onChange={setList}
              placeholder="https://git.example.com/team/*"
              errors={form.submitted ? listErrors : undefined}
              help={
                <>
                  Use <Mono>*</Mono> to allow any repository, globs such as <Mono>https://git.example.com/team/*</Mono>, and a leading{' '}
                  <Mono>!</Mono> to deny matching URLs (deny wins).
                </>
              }
              emptyText="No repositories are allowed."
            />,
          )
        : null}
      {editing === 'namespaces'
        ? drawer(
            'Edit source namespaces',
            <StringListEditor
              legend="Source namespaces"
              label="Namespace or pattern"
              noun="namespace"
              idPrefix="project-srcns"
              values={list}
              onChange={setList}
              placeholder="team-*"
              errors={form.submitted ? listErrors : undefined}
              help="Applications in these namespaces may use this project. Globs are allowed; a leading ! excludes a namespace."
              emptyText="Only the control-plane namespace."
            />,
          )
        : null}
      {editing === 'destinations'
        ? drawer(
            'Edit destinations',
            <RowListEditor
              legend="Destinations"
              noun="destination"
              idPrefix="project-dest"
              rows={rows}
              onChange={setRows}
              fields={[
                { key: 'server', label: 'Server', mono: true, placeholder: 'https://kubernetes.default.svc' },
                { key: 'name', label: 'Cluster name', mono: true, placeholder: 'in-cluster' },
                { key: 'namespace', label: 'Namespace', mono: true, placeholder: '*' },
              ]}
              errors={form.submitted ? destinationErrors : undefined}
              help="Give a server URL or a cluster name (or both). Globs are allowed; prefix with ! to deny."
              emptyText="No destinations are allowed."
            />,
          )
        : null}
      {editing === 'serviceAccounts'
        ? drawer(
            'Edit destination service accounts',
            <RowListEditor
              legend="Destination service accounts"
              noun="service account"
              idPrefix="project-sa"
              rows={rows}
              onChange={setRows}
              fields={[
                { key: 'server', label: 'Server', mono: true, placeholder: 'https://kubernetes.default.svc' },
                { key: 'namespace', label: 'Namespace', mono: true, placeholder: '*' },
                { key: 'defaultServiceAccount', label: 'Service account', mono: true, placeholder: 'deployer' },
              ]}
              errors={form.submitted ? serviceAccountErrors : undefined}
              help="The first entry matching an application's destination is used. Use namespace:name to impersonate an account in another namespace."
              emptyText="No service accounts configured."
            />,
          )
        : null}
    </div>
  );
}

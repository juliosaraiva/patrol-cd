import { useEffect, useMemo, useRef } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router';
import { useApplicationSets, useSettings } from '../../api/queries/resources';
import type { ApplicationSet } from '../../api/types';
import {
  Button,
  ButtonLink,
  CodeBlock,
  EmptyState,
  FilterBar,
  FilterChips,
  FormField,
  HEALTH_STATES,
  HealthStatus,
  Input,
  Mono,
  PageHeader,
  SearchInput,
  Select,
  Skeleton,
  Table,
  Timestamp,
  type Column,
  type FilterChip,
} from '../../design';
import { IconApplicationSets, IconWarning } from '../../design/icons';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import { ErrorBanner } from '../../shell/errors';
import { appCount, appSetHealth, appSetPath, appSetProjects, generatorSummary, problemConditions } from './model';
import styles from './ApplicationSets.module.css';

/** Health values offered by the filter (upstream: Healthy, Progressing, Degraded, Unknown). */
const HEALTH_FILTERS = ['Healthy', 'Progressing', 'Degraded', 'Unknown'] as const;

interface LabelTerm {
  key: string;
  op: '=' | '!=' | 'exists';
  value: string;
}

/** Parses "k=v,k2!=v2,k3" (equality-based label selector). */
export function parseLabelSelector(text: string): LabelTerm[] | null {
  const terms: LabelTerm[] = [];
  for (const raw of text.split(',')) {
    const part = raw.trim();
    if (part === '') continue;
    const m = /^([^=!\s]+)\s*(!=|==|=)\s*(\S*)$/.exec(part);
    if (m) terms.push({ key: m[1] ?? '', op: m[2] === '!=' ? '!=' : '=', value: m[3] ?? '' });
    else if (/^[^=!\s]+$/.test(part)) terms.push({ key: part, op: 'exists', value: '' });
    else return null;
  }
  return terms;
}

function matchesLabels(set: ApplicationSet, terms: LabelTerm[]): boolean {
  const labels = set.metadata.labels ?? {};
  return terms.every((t) => (t.op === 'exists' ? t.key in labels : t.op === '=' ? labels[t.key] === t.value : labels[t.key] !== t.value));
}

interface Row {
  key: string;
  set: ApplicationSet;
  health: string;
  projects: string[];
  problems: number;
}

/** /applicationsets — live ApplicationSet list with URL-synced search and filters (API-118, ASET-124). */
export function Component() {
  useDocumentTitle('ApplicationSets');
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const search = params.get('search') ?? '';
  const health = params.get('health') ?? '';
  const project = params.get('project') ?? '';
  const namespace = params.get('namespace') ?? '';
  const labels = params.get('labels') ?? '';
  const settings = useSettings();
  const cp = settings.data?.controllerNamespace;
  const query = useApplicationSets();
  const searchRef = useRef<HTMLInputElement>(null);

  // "/" focuses this page's search (API-133). Capture phase, so the shell's global search
  // shortcut sees the event as handled.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== '/' || event.ctrlKey || event.metaKey || event.altKey || event.defaultPrevented) return;
      const t = event.target;
      if (t instanceof HTMLElement && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return;
      if (document.querySelector('[aria-modal="true"]')) return;
      event.preventDefault();
      searchRef.current?.focus();
      searchRef.current?.select();
    };
    window.addEventListener('keydown', onKeyDown, true);
    return () => window.removeEventListener('keydown', onKeyDown, true);
  }, []);

  const setParam = (key: string, value: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (value) next.set(key, value);
        else next.delete(key);
        return next;
      },
      { replace: true },
    );

  const items = useMemo(() => query.data?.items ?? [], [query.data]);
  const allRows: Row[] = useMemo(
    () =>
      items.map((set) => ({
        key: `${set.metadata.namespace ?? ''}/${set.metadata.name ?? ''}`,
        set,
        health: appSetHealth(set),
        projects: appSetProjects(set),
        problems: problemConditions(set).length,
      })),
    [items],
  );
  const labelTerms = parseLabelSelector(labels);
  const needle = search.trim().toLowerCase();
  const rows = allRows.filter(
    (r) =>
      (!needle || `${r.set.metadata.namespace ?? ''}/${r.set.metadata.name ?? ''}`.toLowerCase().includes(needle)) &&
      (!health || r.health === health) &&
      (!project || r.projects.includes(project)) &&
      (!namespace || r.set.metadata.namespace === namespace) &&
      (!labelTerms || matchesLabels(r.set, labelTerms)),
  );
  const projects = [...new Set(allRows.flatMap((r) => r.projects))].sort();
  const namespaces = [...new Set(allRows.map((r) => r.set.metadata.namespace ?? '').filter(Boolean))].sort();
  const counts = HEALTH_STATES.map((h) => ({ h, n: rows.filter((r) => r.health === h).length })).filter((c) => c.n > 0);

  const chips: FilterChip[] = [
    search && { id: 'search', label: `Search: ${search}` },
    health && { id: 'health', label: `Health: ${health}` },
    project && { id: 'project', label: `Project: ${project}` },
    namespace && { id: 'namespace', label: `Namespace: ${namespace}` },
    labels && { id: 'labels', label: `Labels: ${labels}` },
  ].filter((c): c is FilterChip => Boolean(c));
  const clearAll = () => setParams(new URLSearchParams(), { replace: true });

  const columns: Column<Row>[] = [
    {
      id: 'name',
      header: 'Name',
      sortValue: (r) => r.set.metadata.name,
      cell: (r) => (
        <span className={styles.nameCell}>
          <Link to={appSetPath(r.set, cp)}>
            <Mono>{r.set.metadata.name}</Mono>
          </Link>
          <span className={styles.secondary}>{generatorSummary(r.set)}</span>
        </span>
      ),
    },
    { id: 'namespace', header: 'Namespace', sortValue: (r) => r.set.metadata.namespace, cell: (r) => <Mono>{r.set.metadata.namespace ?? ''}</Mono> },
    { id: 'project', header: 'Project', sortValue: (r) => r.projects.join(','), cell: (r) => r.projects.join(', ') },
    { id: 'health', header: 'Health', sortValue: (r) => r.health, cell: (r) => <HealthStatus status={r.health} /> },
    {
      id: 'conditions',
      header: 'Conditions',
      sortValue: (r) => r.problems,
      cell: (r) =>
        r.problems > 0 ? (
          <span className={[styles.conditionCount, styles.warningText].join(' ')}>
            <IconWarning />
            {`${r.problems} need attention`}
          </span>
        ) : (
          <span className={styles.secondary}>OK</span>
        ),
    },
    { id: 'apps', header: 'Applications', align: 'end', sortValue: (r) => appCount(r.set), cell: (r) => appCount(r.set) },
    { id: 'created', header: 'Created', sortValue: (r) => r.set.metadata.creationTimestamp, cell: (r) => <Timestamp value={r.set.metadata.creationTimestamp} /> },
  ];

  return (
    <>
      <PageHeader
        title="ApplicationSets"
        description="Templates that generate Applications from generators such as Git directories, clusters, SCM providers and pull requests."
      />
      {query.isError ? <ErrorBanner error={query.error} onRetry={() => void query.refetch()} title="ApplicationSets could not be loaded" /> : null}
      {query.isPending ? (
        <div aria-busy="true" className={styles.stack}>
          <span className="sr-only">Loading ApplicationSets</span>
          <Skeleton height={32} />
          <Skeleton height={240} />
        </div>
      ) : null}
      {query.isSuccess && items.length === 0 ? (
        <EmptyState
          icon={<IconApplicationSets size={24} />}
          title="No ApplicationSets yet"
          action={
            <ButtonLink variant="primary" to="/help">
              Get the patrol CLI
            </ButtonLink>
          }
          secondaryAction={<ButtonLink to="/applications">View applications</ButtonLink>}
        >
          <p>
            An ApplicationSet generates one Application per parameter set of its generators, for example one per cluster or per pull request.
            Create one from a manifest with the CLI or kubectl:
          </p>
          <CodeBlock label="Create command" code="patrol appset create applicationset.yaml" />
        </EmptyState>
      ) : null}
      {query.isSuccess && items.length > 0 ? (
        <div className={styles.stack}>
          <FilterBar
            label="ApplicationSet filters"
            chips={<FilterChips chips={chips} onRemove={(id) => setParam(id, '')} onClearAll={clearAll} />}
          >
            <div className={styles.filters}>
              <div className={styles.searchControl}>
                <SearchInput
                  ref={searchRef}
                  aria-label="Search ApplicationSets"
                  placeholder="Search by name or namespace"
                  shortcut="/"
                  value={search}
                  onValueChange={(v) => setParam('search', v)}
                />
              </div>
              <FormField label="Health" className={styles.filterControl}>
                <Select value={health} onChange={(e) => setParam('health', e.target.value)} options={[{ value: '', label: 'Any health' }, ...HEALTH_FILTERS.map((h) => ({ value: h, label: h }))]} />
              </FormField>
              <FormField label="Project" className={styles.filterControl}>
                <Select value={project} onChange={(e) => setParam('project', e.target.value)} options={[{ value: '', label: 'Any project' }, ...projects.map((p) => ({ value: p, label: p }))]} />
              </FormField>
              <FormField label="Namespace" className={styles.filterControl}>
                <Select
                  value={namespace}
                  onChange={(e) => setParam('namespace', e.target.value)}
                  options={[{ value: '', label: 'Any namespace' }, ...namespaces.map((n) => ({ value: n, label: n }))]}
                />
              </FormField>
              <FormField
                label="Labels"
                className={styles.filterControl}
                error={labels && !labelTerms ? 'Use key=value, key!=value or key, separated by commas.' : undefined}
              >
                <Input mono placeholder="team=web" defaultValue={labels} key={labels} onBlur={(e) => setParam('labels', e.target.value.trim())} onKeyDown={(e) => {
                  if (e.key === 'Enter') setParam('labels', e.currentTarget.value.trim());
                }} />
              </FormField>
            </div>
          </FilterBar>
          <ul className={styles.summary} aria-label="Health summary">
            <li>{`${rows.length} of ${items.length} ApplicationSets`}</li>
            {counts.map((c) => (
              <li key={c.h}>
                <HealthStatus status={c.h} />
                <span>{c.n}</span>
              </li>
            ))}
          </ul>
          {rows.length === 0 ? (
            <EmptyState title="No ApplicationSets match the filters" headingLevel={2} action={<Button onClick={clearAll}>Clear all filters</Button>}>
              <p>Change or remove a filter to see more ApplicationSets.</p>
            </EmptyState>
          ) : (
            <Table
              caption="ApplicationSets"
              columns={columns}
              rows={rows}
              getRowKey={(r) => r.key}
              getRowLabel={(r) => r.set.metadata.name ?? r.key}
              onRowActivate={(r) => void navigate(appSetPath(r.set, cp))}
              defaultSort={{ columnId: 'name', direction: 'ascending' }}
              height={rows.length > 100 ? 640 : undefined}
            />
          )}
        </div>
      ) : null}
    </>
  );
}

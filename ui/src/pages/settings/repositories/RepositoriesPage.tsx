import { useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useDeleteRepository } from '../../../api/hooks/settings';
import { listRepositories } from '../../../api/endpoints/repositories';
import { queryKeys } from '../../../api/queries/keys';
import { useProjects, useRepositories, useSettings } from '../../../api/queries/resources';
import type { RepositoryInfo } from '../../../api/types';
import {
  Badge,
  Banner,
  Button,
  ConnectionStatus,
  EmptyState,
  FilterBar,
  FormField,
  IconButton,
  IconMore,
  IconPlus,
  IconRefresh,
  Menu,
  Mono,
  PageHeader,
  SearchInput,
  Select,
  Table,
  Tabs,
  Timestamp,
  useToast,
  type Column,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { useTabParam } from '../common/editors';
import { ConfirmDialog, LoadingBlock } from '../common/feedback';
import styles from '../common/common.module.css';
import { RepositoryForm } from './RepositoryForm';

type Purpose = 'read' | 'write';

const TYPE_OPTIONS = [
  { value: '', label: 'All types' },
  { value: 'git', label: 'Git' },
  { value: 'helm', label: 'Helm' },
  { value: 'oci', label: 'OCI' },
];
const STATUS_OPTIONS = [
  { value: '', label: 'All statuses' },
  { value: 'Successful', label: 'Successful' },
  { value: 'Failed', label: 'Failed' },
  { value: 'Unknown', label: 'Unknown' },
];

const repoKey = (r: RepositoryInfo) => `${r.spec.project ?? ''}|${r.spec.url}`;

/** Credentials column: own credentials, a matching template, or none (never the values). */
function credentialsLabel(r: RepositoryInfo): string {
  if (r.hasCredentials) return 'Stored';
  if (r.inheritedCredentials) return 'From template';
  return 'None';
}

function RepositoryList({ purpose }: { purpose: Purpose }) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const repos = useRepositories({}, purpose);
  const projects = useProjects();
  const [search, setSearch] = useState('');
  const [type, setType] = useState('');
  const [status, setStatus] = useState('');
  const [project, setProject] = useState('');
  const [connectOpen, setConnectOpen] = useState(false);
  const [editing, setEditing] = useState<RepositoryInfo | undefined>();
  const [deleting, setDeleting] = useState<RepositoryInfo | undefined>();
  const [refreshing, setRefreshing] = useState(false);
  const remove = useDeleteRepository(purpose);
  const noun = purpose === 'write' ? 'write repository' : 'repository';

  const items = useMemo(() => repos.data?.items ?? [], [repos.data]);
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return items.filter(
      (r) =>
        (!q || r.spec.url.toLowerCase().includes(q) || (r.spec.name ?? '').toLowerCase().includes(q)) &&
        (!type || (r.spec.type ?? 'git') === type) &&
        (!status || (r.connectionState.status ?? 'Unknown') === status) &&
        (!project || (r.spec.project ?? '') === project),
    );
  }, [items, search, type, status, project]);
  const filtersActive = Boolean(search || type || status || project);

  const refresh = async () => {
    setRefreshing(true);
    try {
      const data = await listRepositories({ forceRefresh: true }, { purpose });
      queryClient.setQueryData(queryKeys.repositories.list(purpose, {}), data);
      toast({ tone: 'success', title: 'Connection status refreshed' });
    } catch (error) {
      toast({ tone: 'error', title: 'Could not refresh connection status', description: error instanceof Error ? error.message : undefined });
    } finally {
      setRefreshing(false);
    }
  };

  const columns: Column<RepositoryInfo>[] = [
    { id: 'type', header: 'Type', width: '72px', cell: (r) => <Badge>{r.spec.type ?? 'git'}</Badge>, sortValue: (r) => r.spec.type ?? 'git' },
    { id: 'name', header: 'Name', width: '110px', cell: (r) => r.spec.name ?? '—', sortValue: (r) => r.spec.name },
    {
      id: 'url',
      header: 'Repository',
      cell: (r) => <Mono title={r.spec.url}>{r.spec.url}</Mono>,
      sortValue: (r) => r.spec.url,
    },
    { id: 'project', header: 'Project', width: '120px', cell: (r) => r.spec.project ?? 'All projects', sortValue: (r) => r.spec.project },
    {
      id: 'status',
      header: 'Connection',
      width: '260px',
      cell: (r) => {
        const state = r.connectionState;
        return (
          <span className={styles.inlineStatus} title={state.message}>
            <ConnectionStatus status={state.status} />
            {state.message ? <span className={styles.small}>{state.message}</span> : null}
          </span>
        );
      },
      sortValue: (r) => r.connectionState.status,
    },
    {
      id: 'credentials',
      header: 'Credentials',
      width: '120px',
      cell: credentialsLabel,
      sortValue: credentialsLabel,
    },
    {
      id: 'attempted',
      header: 'Last attempt',
      width: '130px',
      cell: (r) => <Timestamp value={r.connectionState.attemptedAt} fallback="Not attempted" />,
      sortValue: (r) => r.connectionState.attemptedAt,
    },
    {
      id: 'actions',
      header: 'Actions',
      hideHeader: true,
      width: '56px',
      align: 'end',
      cell: (r) => (
        <Menu
          label={`Actions for ${r.spec.url}`}
          align="end"
          trigger={(props) => <IconButton {...props} aria-label={`Actions for ${r.spec.url}`} icon={<IconMore />} size="sm" />}
          items={[
            { id: 'edit', label: 'Edit', onSelect: () => setEditing(r) },
            { id: 'delete', label: 'Disconnect', danger: true, onSelect: () => setDeleting(r) },
          ]}
        />
      ),
    },
  ];

  const projectOptions = [
    { value: '', label: 'All projects' },
    ...(projects.data?.items ?? []).map((p) => ({ value: p.metadata.name ?? '', label: p.metadata.name ?? '' })),
  ];

  let content;
  if (repos.isPending) {
    content = <LoadingBlock label={`Loading ${noun === 'repository' ? 'repositories' : 'write repositories'}`} />;
  } else if (repos.isError) {
    content = <ErrorBanner error={repos.error} onRetry={() => void repos.refetch()} />;
  } else if (items.length === 0) {
    content = (
      <EmptyState
        title={purpose === 'write' ? 'No write repositories' : 'No repositories connected'}
        action={
          <Button variant="primary" icon={<IconPlus />} onClick={() => setConnectOpen(true)}>
            {purpose === 'write' ? 'Connect write repository' : 'Connect repository'}
          </Button>
        }
      >
        {purpose === 'write'
          ? 'Add push credentials for each repository the source hydrator commits rendered manifests to.'
          : 'Public repositories work without being connected. Connect a repository to store its credentials, TLS or proxy settings, or to use a Helm or OCI registry.'}
      </EmptyState>
    );
  } else {
    content = (
      <Table
        caption={purpose === 'write' ? 'Write repositories' : 'Repositories'}
        className={styles.extraWideTable}
        columns={columns}
        rows={filtered}
        getRowKey={repoKey}
        defaultSort={{ columnId: 'url', direction: 'ascending' }}
        onRowActivate={(r) => setEditing(r)}
        empty={
          <EmptyState
            headingLevel={3}
            title="No repositories match the filters"
            action={
              <Button
                onClick={() => {
                  setSearch('');
                  setType('');
                  setStatus('');
                  setProject('');
                }}
              >
                Clear filters
              </Button>
            }
          />
        }
      />
    );
  }

  return (
    <div className={styles.stack}>
      <div className={styles.sectionHeader}>
        <p className={styles.muted}>
          {purpose === 'write'
            ? 'The source hydrator renders manifests from the dry source and commits them to a hydrated branch. It needs push access, which is kept apart from the read credentials used to fetch sources.'
            : 'Connection status is checked periodically; refresh to test every repository now.'}
        </p>
        <div className={styles.actions}>
          <Button icon={<IconRefresh />} onClick={() => void refresh()} loading={refreshing}>
            Refresh status
          </Button>
          <Button variant="primary" icon={<IconPlus />} onClick={() => setConnectOpen(true)}>
            {purpose === 'write' ? 'Connect write repository' : 'Connect repository'}
          </Button>
        </div>
      </div>
      {items.length > 0 ? (
        <FilterBar label={`Filter ${purpose === 'write' ? 'write repositories' : 'repositories'}`}>
          <div className={styles.toolbarGrow}>
            <SearchInput value={search} onValueChange={setSearch} placeholder="Search by URL or name" aria-label="Search repositories" />
          </div>
          <div className={styles.toolbarField}>
            <FormField label="Type">
              <Select value={type} onChange={(e) => setType(e.target.value)} options={TYPE_OPTIONS} size="sm" />
            </FormField>
          </div>
          <div className={styles.toolbarField}>
            <FormField label="Connection">
              <Select value={status} onChange={(e) => setStatus(e.target.value)} options={STATUS_OPTIONS} size="sm" />
            </FormField>
          </div>
          <div className={styles.toolbarField}>
            <FormField label="Project">
              <Select value={project} onChange={(e) => setProject(e.target.value)} options={projectOptions} size="sm" />
            </FormField>
          </div>
        </FilterBar>
      ) : null}
      {items.length > 0 ? (
        <p className={styles.small} aria-live="polite">
          {filtersActive ? `${filtered.length} of ${items.length} shown` : `${items.length} total`}
        </p>
      ) : null}
      {content}
      {connectOpen ? <RepositoryForm open onClose={() => setConnectOpen(false)} mode="repository" purpose={purpose} /> : null}
      {editing ? <RepositoryForm open onClose={() => setEditing(undefined)} mode="repository" purpose={purpose} existing={editing} /> : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onClose={() => {
          setDeleting(undefined);
          remove.reset();
        }}
        title={`Disconnect ${noun}?`}
        confirmLabel="Disconnect"
        action={`disconnect the ${noun}`}
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          if (!deleting) return;
          remove.mutate(
            { url: deleting.spec.url, appProject: deleting.spec.project },
            {
              onSuccess: () => {
                toast({ tone: 'success', title: purpose === 'write' ? 'Write repository disconnected' : 'Repository disconnected', description: deleting.spec.url });
                setDeleting(undefined);
              },
            },
          );
        }}
      >
        <p>
          <Mono>{deleting?.spec.url}</Mono> and its stored credentials will be removed.{' '}
          {purpose === 'write'
            ? 'The source hydrator can no longer push to it.'
            : 'Applications that use it keep working only if the repository is public or a credential template matches it.'}
        </p>
      </ConfirmDialog>
    </div>
  );
}

/** /settings/repos — repositories and, with the source hydrator, write repositories (API-101). */
export function Component() {
  useDocumentTitle('Repositories', 'Settings');
  const settings = useSettings();
  const [tab, setTab] = useTabParam(['read', 'write'] as const, 'read', 'purpose');
  const hydrator = settings.data?.hydratorEnabled === true;
  return (
    <>
      <PageHeader title="Repositories" description="Git, Helm and OCI repositories Patrol CD reads from, and their connection status." />
      <Tabs
        label="Repository purpose"
        value={tab}
        onValueChange={setTab}
        items={[
          { id: 'read', label: 'Repositories', content: <RepositoryList purpose="read" /> },
          {
            id: 'write',
            label: 'Write repositories',
            content: hydrator ? (
              <RepositoryList purpose="write" />
            ) : (
              <div className={styles.stack}>
                <Banner tone="info" title="The source hydrator is disabled">
                  Write repositories hold push credentials for the source hydrator, which commits rendered manifests to a branch. Enable
                  it with <Mono>hydrator.enabled: &quot;true&quot;</Mono> in the <Mono>patrol-cmd-params-cm</Mono> ConfigMap and restart
                  the control plane to manage them here.
                </Banner>
              </div>
            ),
          },
        ]}
      />
    </>
  );
}

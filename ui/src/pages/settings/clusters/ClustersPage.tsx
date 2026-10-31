import { useMemo, useState } from 'react';
import { Link, useNavigate } from 'react-router';
import { useClusters, useSettings } from '../../../api/queries/resources';
import type { RegisteredCluster } from '../../../api/types';
import {
  Button,
  CodeBlock,
  ConnectionStatus,
  Dialog,
  EmptyState,
  IconPlus,
  Mono,
  PageHeader,
  SearchInput,
  Table,
  Timestamp,
  type Column,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { LoadingBlock } from '../common/feedback';
import styles from '../common/common.module.css';
import { clusterPath, isImplicit, namespacesSummary, noCacheSyncText } from './clusterUtil';

function AddClusterDialog({ onClose }: { onClose: () => void }) {
  const settings = useSettings();
  const namespace = settings.data?.controllerNamespace ?? 'patrol';
  const manifest = [
    'apiVersion: v1',
    'kind: Secret',
    'metadata:',
    '  name: cluster-prod',
    `  namespace: ${namespace}`,
    'stringData:',
    '  bearerToken: <token of a service account in the target cluster>',
    '  caData: <base64 CA bundle of the target API server>',
    '---',
    'apiVersion: cd.patrol.example/v1alpha1',
    'kind: Cluster',
    'metadata:',
    '  name: prod',
    `  namespace: ${namespace}`,
    'spec:',
    '  name: prod',
    '  server: https://prod.k8s.example.com',
    '  config:',
    '    credentialsRef:',
    '      name: cluster-prod',
  ].join('\n');
  return (
    <Dialog
      open
      onClose={onClose}
      size="lg"
      title="Add a cluster"
      description="Cluster credentials are created from a kubeconfig, so clusters are added with the CLI or declaratively, not in the browser."
      footer={<Button onClick={onClose}>Close</Button>}
    >
      <div className={styles.stack}>
        <section className={styles.section} aria-labelledby="add-cluster-cli">
          <h3 id="add-cluster-cli">With the CLI</h3>
          <p className={styles.muted}>
            Uses a context from your kubeconfig: creates a service account with the needed permissions in the target cluster and
            registers its token with Patrol.
          </p>
          <CodeBlock title="Commands" code={'patrol login <patrol-server>\npatrol cluster add <kubeconfig-context> --name <cluster-name>'} />
        </section>
        <section className={styles.section} aria-labelledby="add-cluster-declarative">
          <h3 id="add-cluster-declarative">Declaratively</h3>
          <p className={styles.muted}>
            Apply a Cluster resource in the control-plane namespace that references a Secret with the credentials. The Cluster object
            holds only the reference, never the credentials.
          </p>
          <CodeBlock title="cluster.yaml" code={manifest} language="yaml" />
        </section>
      </div>
    </Dialog>
  );
}

/** /settings/clusters — destination clusters (API-104). */
export function Component() {
  useDocumentTitle('Clusters', 'Settings');
  const navigate = useNavigate();
  const clusters = useClusters();
  const [adding, setAdding] = useState(false);
  const [search, setSearch] = useState('');
  const items = useMemo(() => clusters.data?.items ?? [], [clusters.data]);
  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return q ? items.filter((c) => c.spec.name.toLowerCase().includes(q) || c.spec.server.toLowerCase().includes(q)) : items;
  }, [items, search]);

  const columns: Column<RegisteredCluster>[] = [
    {
      id: 'name',
      header: 'Name',
      width: '140px',
      cell: (c) => <Link to={clusterPath(c.spec.server)}>{c.spec.name}</Link>,
      sortValue: (c) => c.spec.name,
    },
    { id: 'server', header: 'Server', cell: (c) => <Mono title={c.spec.server}>{c.spec.server}</Mono>, sortValue: (c) => c.spec.server },
    { id: 'version', header: 'Version', width: '80px', cell: (c) => c.status?.serverVersion ?? '—', sortValue: (c) => c.status?.serverVersion },
    {
      id: 'status',
      header: 'Connection',
      width: '210px',
      cell: (c) => {
        const state = c.status?.connectionState;
        return (
          <span className={styles.inlineStatus} title={state?.message}>
            <ConnectionStatus status={state?.status} />
            {state?.message ? <span className={styles.small}>{state.message}</span> : null}
          </span>
        );
      },
      sortValue: (c) => c.status?.connectionState?.status,
    },
    {
      id: 'apps',
      header: 'Applications',
      width: '110px',
      align: 'end',
      cell: (c) => (isImplicit(c) ? '—' : (c.status?.info?.applicationsCount ?? 0)),
      sortValue: (c) => c.status?.info?.applicationsCount ?? 0,
    },
    {
      id: 'cache',
      header: 'Cache',
      width: '170px',
      cell: (c) => {
        const cache = c.status?.info?.cacheInfo;
        if (!cache?.lastCacheSyncTime) return <span className={styles.small}>{noCacheSyncText(c)}</span>;
        return (
          <span>
            {cache.resourcesCount ?? 0} resources · <Timestamp value={cache.lastCacheSyncTime} />
          </span>
        );
      },
      sortValue: (c) => c.status?.info?.cacheInfo?.resourcesCount,
    },
    {
      id: 'shard',
      header: 'Shard',
      width: '70px',
      align: 'end',
      cell: (c) => c.spec.shard ?? c.status?.assignedShard ?? '—',
      sortValue: (c) => c.spec.shard ?? c.status?.assignedShard,
    },
    { id: 'project', header: 'Project', width: '110px', cell: (c) => c.spec.project ?? '—', sortValue: (c) => c.spec.project },
    { id: 'namespaces', header: 'Namespaces', width: '160px', cell: (c) => <span title={namespacesSummary(c)}>{namespacesSummary(c)}</span> },
  ];

  let content;
  if (clusters.isPending) content = <LoadingBlock label="Loading clusters" />;
  else if (clusters.isError) content = <ErrorBanner error={clusters.error} onRetry={() => void clusters.refetch()} />;
  else if (items.length === 0) {
    content = (
      <EmptyState
        title="No clusters"
        action={
          <Button variant="primary" icon={<IconPlus />} onClick={() => setAdding(true)}>
            Add a cluster
          </Button>
        }
      >
        The in-cluster destination may be disabled on this instance. Add clusters with the CLI or a Cluster resource; they appear here
        with their connection state.
      </EmptyState>
    );
  } else {
    content = (
      <>
        <div className={styles.toolbar}>
          <div className={styles.toolbarGrow}>
            <SearchInput value={search} onValueChange={setSearch} aria-label="Search clusters" placeholder="Search by name or server" />
          </div>
        </div>
        <Table
          caption="Clusters"
          className={styles.widestTable}
          columns={columns}
          rows={filtered}
          getRowKey={(c) => c.spec.server}
          defaultSort={{ columnId: 'name', direction: 'ascending' }}
          onRowActivate={(c) => void navigate(clusterPath(c.spec.server))}
          empty={<EmptyState headingLevel={3} title="No clusters match the search" action={<Button onClick={() => setSearch('')}>Clear search</Button>} />}
        />
      </>
    );
  }

  return (
    <>
      <PageHeader
        title="Clusters"
        description="Kubernetes clusters applications can be deployed to."
        actions={
          items.length > 0 ? (
            <Button variant="primary" icon={<IconPlus />} onClick={() => setAdding(true)}>
              Add a cluster
            </Button>
          ) : undefined
        }
      />
      <div className={styles.stack}>{content}</div>
      {adding ? <AddClusterDialog onClose={() => setAdding(false)} /> : null}
    </>
  );
}

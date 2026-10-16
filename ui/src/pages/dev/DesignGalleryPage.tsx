import { useState } from 'react';
import {
  Badge,
  Banner,
  Button,
  Checkbox,
  Code,
  CodeBlock,
  ConnectionStatus,
  CONNECTION_STATES,
  CopyButton,
  EmptyState,
  FilterChips,
  FormField,
  HEALTH_STATES,
  HealthStatus,
  IconButton,
  Input,
  KeyValueList,
  Menu,
  Mono,
  OPERATION_PHASES,
  OperationPhaseStatus,
  PageHeader,
  Pagination,
  RadioGroup,
  SearchInput,
  Select,
  Skeleton,
  Spinner,
  Switch,
  SYNC_STATES,
  SyncStatus,
  Table,
  Tabs,
  Timestamp,
  Tooltip,
  useToast,
  type Column,
} from '../../design';
import { IconMore, IconPlus, IconRefresh } from '../../design/icons';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import styles from './DesignGallery.module.css';

interface DemoRow {
  name: string;
  health: string;
  sync: string;
  phase: string;
}

const HEALTH = HEALTH_STATES;
const rows: DemoRow[] = Array.from({ length: 5000 }, (_, i) => ({
  name: `app-${i}`,
  health: HEALTH[i % HEALTH.length] ?? 'Unknown',
  sync: SYNC_STATES[i % SYNC_STATES.length] ?? 'Unknown',
  phase: OPERATION_PHASES[i % OPERATION_PHASES.length] ?? 'Pending',
}));

const columns: Column<DemoRow>[] = [
  { id: 'name', header: 'Name', cell: (r) => <Mono>{r.name}</Mono>, sortValue: (r) => r.name },
  { id: 'health', header: 'Health', cell: (r) => <HealthStatus status={r.health} />, sortValue: (r) => r.health },
  { id: 'sync', header: 'Sync', cell: (r) => <SyncStatus status={r.sync} />, sortValue: (r) => r.sync },
  { id: 'phase', header: 'Last operation', cell: (r) => <OperationPhaseStatus phase={r.phase} /> },
];

/** Development-only catalog of design system components (route /dev/design, not in builds). */
export function Component() {
  useDocumentTitle('Design system');
  const { toast } = useToast();
  const [on, setOn] = useState(true);
  const [radio, setRadio] = useState<'foreground' | 'background'>('foreground');
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(1);
  return (
    <>
      <PageHeader title="Design system" description="Development-only gallery. Not part of production builds." />
      <section className={styles.section} aria-labelledby="status-h">
        <h2 id="status-h">Status</h2>
        <div className={styles.row}>
          {HEALTH_STATES.map((s) => (
            <HealthStatus key={s} status={s} />
          ))}
        </div>
        <div className={styles.row}>
          {SYNC_STATES.map((s) => (
            <SyncStatus key={s} status={s} />
          ))}
        </div>
        <div className={styles.row}>
          {OPERATION_PHASES.map((s) => (
            <OperationPhaseStatus key={s} phase={s} />
          ))}
        </div>
        <div className={styles.row}>
          {CONNECTION_STATES.map((s) => (
            <ConnectionStatus key={s} status={s} />
          ))}
        </div>
      </section>
      <section className={styles.section} aria-labelledby="buttons-h">
        <h2 id="buttons-h">Buttons and menus</h2>
        <div className={styles.row}>
          <Button variant="primary" icon={<IconPlus />}>
            Primary
          </Button>
          <Button>Secondary</Button>
          <Button variant="danger">Danger</Button>
          <Button variant="ghost">Ghost</Button>
          <Button size="sm">Small</Button>
          <Button loading>Loading</Button>
          <Tooltip content="Refresh from Git">
            <IconButton aria-label="Refresh" icon={<IconRefresh />} variant="secondary" />
          </Tooltip>
          <Menu
            trigger={(p) => <IconButton {...p} aria-label="More actions" icon={<IconMore />} variant="secondary" />}
            items={[
              { id: 'a', label: 'Sync', onSelect: () => toast({ title: 'Sync started', tone: 'success' }) },
              { id: 'b', label: 'Refresh', onSelect: () => toast({ title: 'Refreshing', tone: 'info' }) },
              { type: 'separator', id: 's' },
              { id: 'c', label: 'Delete', danger: true, onSelect: () => toast({ title: 'Delete failed', tone: 'error', description: 'permission denied' }) },
            ]}
          />
          <Spinner label="Loading" showLabel />
        </div>
      </section>
      <section className={styles.section} aria-labelledby="forms-h">
        <h2 id="forms-h">Forms</h2>
        <div className={styles.grid}>
          <FormField label="Repository URL" help="HTTPS or SSH URL" required>
            <Input mono placeholder="https://github.com/org/repo.git" />
          </FormField>
          <FormField label="Revision" error="Branch not found">
            <Input defaultValue="mian" />
          </FormField>
          <FormField label="Type">
            <Select options={[{ value: 'git', label: 'Git' }, { value: 'helm', label: 'Helm' }, { value: 'oci', label: 'OCI' }]} />
          </FormField>
          <SearchInput aria-label="Search applications" value={search} onValueChange={setSearch} shortcut="/" />
          <Checkbox label="Prune resources" description="Delete resources no longer in Git" />
          <Switch checked={on} onChange={setOn} label="Self heal" />
          <RadioGroup
            legend="Propagation policy"
            value={radio}
            onChange={setRadio}
            options={[
              { value: 'foreground', label: 'Foreground' },
              { value: 'background', label: 'Background' },
            ]}
          />
        </div>
        <FilterChips chips={[{ id: 'h', label: 'Health: Degraded' }, { id: 's', label: 'Sync: OutOfSync' }]} onRemove={() => undefined} onClearAll={() => undefined} />
      </section>
      <section className={styles.section} aria-labelledby="feedback-h">
        <h2 id="feedback-h">Feedback</h2>
        <Banner tone="info" title="Sync window open">
          Automated sync allowed until 18:00 UTC.
        </Banner>
        <Banner tone="warning" title="Out of sync">
          3 resources differ from Git.
        </Banner>
        <Banner tone="error" title="Sync failed">
          one or more objects failed to apply
        </Banner>
        <div className={styles.row}>
          <Badge>default</Badge>
          <Badge tone="accent">auto-sync</Badge>
          <Badge tone="warning">2 warnings</Badge>
          <Badge tone="danger">1 error</Badge>
          <Skeleton width={160} />
        </div>
        <EmptyState title="No applications yet" action={<Button variant="primary">New application</Button>}>
          Create an application to deploy manifests from a repository.
        </EmptyState>
      </section>
      <section className={styles.section} aria-labelledby="data-h">
        <h2 id="data-h">Data</h2>
        <KeyValueList
          items={[
            { key: 'rev', label: 'Revision', value: <Mono>4f2c1a9d3e7b</Mono> },
            { key: 'synced', label: 'Last synced', value: <Timestamp value={new Date(Date.now() - 420_000).toISOString()} /> },
            { key: 'cmd', label: 'CLI', value: <Code>patrol app sync guestbook</Code> },
            { key: 'copy', label: 'Copy', value: <CopyButton value="4f2c1a9d3e7b" label="Copy revision" /> },
          ]}
        />
        <CodeBlock title="Live manifest" code={'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: guestbook\n'} />
        <Tabs
          label="Gallery tabs"
          items={[
            {
              id: 'table',
              label: 'Table (5,000 rows)',
              content: <Table caption="Demo applications" columns={columns} rows={rows} getRowKey={(r) => r.name} selectable height={360} />,
            },
            { id: 'paging', label: 'Pagination', content: <Pagination page={page} pageCount={12} onPageChange={setPage} /> },
          ]}
        />
      </section>
    </>
  );
}

import { useState, type ReactNode } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router';
import { useApplicationSetApplications } from '../../api/hooks/applicationsets';
import { useApplicationSet, useApplicationSetEvents, useApplicationSets, useApplicationSetTree, useSettings } from '../../api/queries/resources';
import type { ApplicationSet } from '../../api/types';
import {
  Badge,
  Button,
  ButtonLink,
  CodeBlock,
  KeyValueList,
  Mono,
  PageHeader,
  Skeleton,
  SkeletonText,
  Tabs,
  Timestamp,
  useToast,
  type TabItem,
} from '../../design';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import { ErrorBanner, missingOrForbiddenMessage } from '../../shell/errors';
import { DeleteApplicationSetDialog } from './DeleteApplicationSetDialog';
import { toYAML } from './diff';
import { GeneratorList } from './GeneratorSummary';
import { applicationPath, appSetProjects, generatedAppRows, hasPullRequestGenerator, syncPolicyText, type GeneratedAppRow } from './model';
import { AppSetTree, ConditionsList, EventsTable, GeneratedAppsTable, StatusBar } from './parts';
import { PreviewEnvironments } from './PreviewEnvironments';
import { PreviewPanel } from './PreviewPanel';
import styles from './ApplicationSets.module.css';

function labelsText(map: Record<string, string> | undefined): ReactNode {
  const entries = Object.entries(map ?? {});
  return entries.length === 0 ? undefined : <Mono>{entries.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono>;
}

function Summary({ set }: { set: ApplicationSet }) {
  const steps = set.spec.strategy?.rollingSync?.steps ?? [];
  return (
    <div className={styles.stack}>
      <section className={styles.section} aria-labelledby="appset-summary">
        <h2 id="appset-summary" className={styles.sectionTitle}>
          Summary
        </h2>
        <KeyValueList
          items={[
            { key: 'name', label: 'Name', value: <Mono>{set.metadata.name}</Mono> },
            { key: 'namespace', label: 'Namespace', value: <Mono>{set.metadata.namespace}</Mono> },
            { key: 'created', label: 'Created', value: <Timestamp value={set.metadata.creationTimestamp} /> },
            { key: 'projects', label: 'Projects', value: appSetProjects(set).join(', ') },
            { key: 'labels', label: 'Labels', value: labelsText(set.metadata.labels) },
            { key: 'annotations', label: 'Annotations', value: labelsText(set.metadata.annotations) },
            { key: 'templating', label: 'Templating', value: set.spec.goTemplate ? `Go templates${set.spec.goTemplateOptions?.length ? ` (${set.spec.goTemplateOptions.join(', ')})` : ''}` : 'fasttemplate ({{param}})' },
            { key: 'policy', label: 'Sync policy', value: syncPolicyText(set) },
            {
              key: 'preserve',
              label: 'On deletion',
              value: set.spec.syncPolicy?.preserveResourcesOnDeletion ? 'Deployed resources are preserved' : 'Deployed resources are deleted with the Applications',
            },
            {
              key: 'strategy',
              label: 'Strategy',
              value:
                set.spec.strategy?.type === 'RollingSync'
                  ? `RollingSync in ${steps.length} step${steps.length === 1 ? '' : 's'}`
                  : 'AllAtOnce: every Application is updated at the same time',
            },
          ]}
        />
        {steps.length > 0 ? (
          <ol className={styles.plainList} aria-label="RollingSync steps">
            {steps.map((s, i) => (
              <li key={i}>
                {(s.matchExpressions ?? []).map((e) => `${e.key} ${e.operator} (${(e.values ?? []).join(', ')})`).join(' and ') || 'All remaining Applications'}
                {s.maxUpdate !== undefined ? `, at most ${String(s.maxUpdate)} at a time` : ''}
              </li>
            ))}
          </ol>
        ) : null}
      </section>
      <section className={styles.section} aria-labelledby="appset-generators">
        <h2 id="appset-generators" className={styles.sectionTitle}>
          Generators
        </h2>
        <GeneratorList generators={set.spec.generators} />
      </section>
      <section className={styles.section} aria-labelledby="appset-template">
        <h2 id="appset-template" className={styles.sectionTitle}>
          Template
        </h2>
        <CodeBlock title="Application template" code={toYAML(set.spec.template)} maxHeight={360} />
        {set.spec.templatePatch ? <CodeBlock title="Template patch" code={set.spec.templatePatch} maxHeight={240} /> : null}
      </section>
    </div>
  );
}

const TABS = ['summary', 'applications', 'tree', 'conditions', 'events', 'preview', 'previews', 'manifest'] as const;

/** /applicationsets/[:appsetNamespace/]:name — ApplicationSet details (API-118, ASET-125, ASET-126). */
export function Component() {
  const { name = '', appsetNamespace } = useParams();
  useDocumentTitle(name, 'ApplicationSets');
  const navigate = useNavigate();
  const { toast } = useToast();
  const [params, setParams] = useSearchParams();
  const settings = useSettings();
  const cp = settings.data?.controllerNamespace;
  const query = useApplicationSet(name, appsetNamespace);
  // Keeps the cached details live: the list watch also writes detail entries.
  useApplicationSets(appsetNamespace ? { appsetNamespace } : {});
  const set = query.data;
  const tree = useApplicationSetTree(name, appsetNamespace);
  const events = useApplicationSetEvents(name, appsetNamespace);
  const apps = useApplicationSetApplications(set);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [runToken, setRunToken] = useState(0);

  const requested = params.get('tab') ?? 'summary';
  const tab = (TABS as readonly string[]).includes(requested) ? requested : 'summary';
  const setTab = (id: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (id === 'summary') next.delete('tab');
        else next.set('tab', id);
        return next;
      },
      { replace: true },
    );

  const description = appsetNamespace ? `ApplicationSet in namespace ${appsetNamespace}` : 'ApplicationSet';

  if (query.isPending) {
    return (
      <>
        <PageHeader title={<Mono>{name}</Mono>} description={description} />
        <div aria-busy="true" className={styles.stack}>
          <span className="sr-only">Loading ApplicationSet</span>
          <Skeleton height={64} />
          <SkeletonText lines={6} />
        </div>
      </>
    );
  }
  if (query.isError || !set) {
    return (
      <>
        <PageHeader title={<Mono>{name}</Mono>} description={description} />
        <div className={styles.stack}>
          <ErrorBanner
            error={query.error}
            onRetry={() => void query.refetch()}
            title={`ApplicationSet ${name} could not be loaded`}
            message={missingOrForbiddenMessage(query.error, 'ApplicationSet')}
          />
          <div>
            <ButtonLink to="/applicationsets">Back to ApplicationSets</ButtonLink>
          </div>
        </div>
      </>
    );
  }

  const rows: GeneratedAppRow[] = generatedAppRows(set, apps.owned);
  const showProgressive = set.spec.strategy?.type === 'RollingSync' || (set.status?.applicationStatus?.length ?? 0) > 0;
  const previews = hasPullRequestGenerator(set);
  const conditions = set.status?.conditions ?? [];

  const items: TabItem[] = [
    { id: 'summary', label: 'Summary', content: <Summary set={set} /> },
    {
      id: 'applications',
      label: `Applications (${rows.length})`,
      content: (
        <div className={styles.stack}>
          {apps.isError ? (
            <p className={styles.muted}>Live Application status is unavailable; health and sync come from the ApplicationSet status.</p>
          ) : null}
          <GeneratedAppsTable
            rows={rows}
            controlPlaneNamespace={cp}
            showProgressive={showProgressive}
            onOpen={(r) => void navigate(applicationPath(r.name, r.namespace, cp))}
          />
        </div>
      ),
    },
    {
      id: 'tree',
      label: 'Resource tree',
      content: (
        <div className={styles.stack} aria-busy={tree.isPending || undefined}>
          {tree.isError ? <ErrorBanner error={tree.error} onRetry={() => void tree.refetch()} title="The resource tree could not be loaded" /> : null}
          {tree.isPending ? <SkeletonText lines={4} /> : <AppSetTree set={set} tree={tree.data} rows={rows} controlPlaneNamespace={cp} />}
        </div>
      ),
    },
    {
      id: 'conditions',
      label: (
        <>
          Conditions <Badge tone={conditions.some((c) => c.type === 'ErrorOccurred' && c.status === 'True') ? 'danger' : 'neutral'}>{String(conditions.length)}</Badge>
        </>
      ),
      content: <ConditionsList conditions={conditions} />,
    },
    {
      id: 'events',
      label: 'Events',
      content: (
        <div className={styles.stack} aria-busy={events.isPending || undefined}>
          {events.isError ? <ErrorBanner error={events.error} onRetry={() => void events.refetch()} title="Events could not be loaded" /> : null}
          {events.isPending ? <SkeletonText lines={4} /> : null}
          {events.isSuccess ? <EventsTable events={events.data.items} /> : null}
        </div>
      ),
    },
    {
      id: 'preview',
      label: 'Preview',
      content: <PreviewPanel set={set} existing={apps.isSuccess ? apps.owned : apps.isError ? null : undefined} runToken={runToken} />,
    },
    ...(previews
      ? [
          {
            id: 'previews',
            label: 'Preview environments',
            content: <PreviewEnvironments set={set} rows={rows} controlPlaneNamespace={cp} />,
          },
        ]
      : []),
    { id: 'manifest', label: 'Manifest', content: <CodeBlock title="ApplicationSet manifest" code={toYAML(set)} maxHeight={640} /> },
  ];

  return (
    <>
      <PageHeader
        title={<Mono>{name}</Mono>}
        description={description}
        actions={
          <>
            <Button
              onClick={() => {
                setTab('preview');
                setRunToken((n) => n + 1);
              }}
            >
              Generate preview
            </Button>
            <Button variant="danger" onClick={() => setDeleteOpen(true)} disabled={Boolean(set.metadata.deletionTimestamp)}>
              {set.metadata.deletionTimestamp ? 'Deleting…' : 'Delete'}
            </Button>
          </>
        }
      />
      <StatusBar set={set} onShowConditions={() => setTab('conditions')} />
      <Tabs label="ApplicationSet views" items={items} value={items.some((i) => i.id === tab) ? tab : 'summary'} onValueChange={setTab} />
      <DeleteApplicationSetDialog
        set={set}
        rows={rows}
        open={deleteOpen}
        onClose={() => setDeleteOpen(false)}
        onDeleted={() => {
          setDeleteOpen(false);
          toast({ tone: 'success', title: `ApplicationSet ${name} deleted` });
          void navigate('/applicationsets');
        }}
      />
    </>
  );
}

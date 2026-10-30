import { useId, useState, type FormEvent } from 'react';
import { useNavigate, useParams } from 'react-router';
import { useDeleteCluster, useInvalidateClusterCache, useRotateClusterAuth, useUpdateCluster } from '../../../api/hooks/settings';
import { isApiError } from '../../../api/errors';
import { useCluster, useProjects } from '../../../api/queries/resources';
import type { RegisteredCluster } from '../../../api/types';
import {
  Badge,
  Banner,
  Button,
  ButtonLink,
  Checkbox,
  ConnectionStatus,
  Drawer,
  EmptyState,
  FormField,
  Input,
  KeyValueList,
  Mono,
  PageHeader,
  Select,
  Table,
  Timestamp,
  useToast,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { mapToRows, nonEmpty, RowListEditor, StringListEditor } from '../common/editors';
import { ActionError, ConfirmDialog, ErrorSummary, LoadingBlock, useFormErrors, type FieldError } from '../common/feedback';
import styles from '../common/common.module.css';
import { validateLabelKey, validateLabelValue, validateNamespace } from '../common/validation';
import { changedClusterFields, clusterUpdateInput, credentialsType, isInCluster, namespacesSummary, type ClusterEditState } from './clusterUtil';

/** Upstream polls cluster details every second; every 5 s keeps status fresh at lower cost. */
const POLL_MS = 5000;

function EditClusterDrawer({ cluster, onClose }: { cluster: RegisteredCluster; onClose: () => void }) {
  const formId = useId();
  const { toast } = useToast();
  const update = useUpdateCluster();
  const projects = useProjects();
  const [edit, setEdit] = useState<ClusterEditState>(() => ({
    name: cluster.spec.name,
    namespaces: cluster.spec.namespaces ?? [],
    clusterResources: cluster.spec.clusterResources ?? false,
    project: cluster.spec.project ?? '',
    labels: mapToRows(cluster.metadata.labels),
    annotations: mapToRows(cluster.metadata.annotations),
  }));
  // What the form opened with, not the polled cluster: a change by another writer is not this edit.
  const [opened] = useState(cluster);

  const nsErrors = edit.namespaces.map((n) => (n.trim() === '' ? null : validateNamespace(n)));
  const labelErrors = edit.labels.map((r) => validateLabelKey(r.key ?? '') ?? validateLabelValue(r.value ?? ''));
  const annotationErrors = edit.annotations.map((r) => validateLabelKey(r.key ?? ''));
  const errors: FieldError[] = [];
  if (edit.name.trim() === '') errors.push({ id: 'cluster-name', message: 'Name is required.' });
  nsErrors.forEach((e, i) => e && errors.push({ id: `cluster-ns-${i}-value`, message: `Namespace ${i + 1}: ${e}` }));
  labelErrors.forEach((e, i) => e && errors.push({ id: `cluster-label-${i}-key`, message: `Label ${i + 1}: ${e}` }));
  annotationErrors.forEach((e, i) => e && errors.push({ id: `cluster-annotation-${i}-key`, message: `Annotation ${i + 1}: ${e}` }));
  const form = useFormErrors(errors);

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    const updatedFields = changedClusterFields(opened, edit);
    if (updatedFields.length === 0) {
      toast({ tone: 'info', title: 'No changes to save' });
      onClose();
      return;
    }
    const body = clusterUpdateInput(cluster, edit);
    update.mutate(
      { ref: { server: cluster.spec.server }, body, updatedFields },
      {
        onSuccess: () => {
          toast({ tone: 'success', title: 'Cluster updated', description: `Changed: ${updatedFields.join(', ')}` });
          onClose();
        },
      },
    );
  };

  return (
    <Drawer
      open
      onClose={onClose}
      width="640px"
      title="Edit cluster"
      description={<Mono>{cluster.spec.server}</Mono>}
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form={formId} loading={update.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id={formId} className={styles.form} onSubmit={onSubmit} noValidate aria-label="Edit cluster">
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {update.error ? <ActionError error={update.error} action="update the cluster" /> : null}
        <p className={styles.small}>Only the fields you change are sent, so concurrent edits of other fields are kept.</p>
        <FormField label="Name" id="cluster-name" required error={form.errorFor('cluster-name')} help="Applications can target the cluster by this name.">
          <Input value={edit.name} onChange={(e) => setEdit({ ...edit, name: e.target.value })} autoComplete="off" />
        </FormField>
        <FormField label="Project" id="cluster-project" help="Scope the cluster to one project; leave unscoped to share it.">
          <Select
            value={edit.project}
            onChange={(e) => setEdit({ ...edit, project: e.target.value })}
            options={[{ value: '', label: 'No project' }, ...(projects.data?.items ?? []).map((p) => ({ value: p.metadata.name ?? '', label: p.metadata.name ?? '' }))]}
          />
        </FormField>
        <StringListEditor
          legend="Namespaces"
          help="Limit Patrol to these namespaces. Leave empty to manage all namespaces."
          label="Namespace"
          noun="namespace"
          idPrefix="cluster-ns"
          values={edit.namespaces}
          onChange={(namespaces) => setEdit({ ...edit, namespaces })}
          errors={form.submitted ? nsErrors : undefined}
          emptyText="All namespaces."
        />
        {nonEmpty(edit.namespaces).length > 0 ? (
          <Checkbox
            label="Also manage cluster-scoped resources"
            description="When namespaces are listed, cluster-scoped resources are ignored unless this is set."
            checked={edit.clusterResources}
            onChange={(e) => setEdit({ ...edit, clusterResources: e.target.checked })}
          />
        ) : null}
        <RowListEditor
          legend="Labels"
          noun="label"
          idPrefix="cluster-label"
          rows={edit.labels}
          onChange={(labels) => setEdit({ ...edit, labels })}
          fields={[
            { key: 'key', label: 'Key', mono: true, placeholder: 'env' },
            { key: 'value', label: 'Value', mono: true, placeholder: 'prod' },
          ]}
          errors={form.submitted ? labelErrors : undefined}
          help="Used by ApplicationSet cluster generators to select clusters."
        />
        <RowListEditor
          legend="Annotations"
          noun="annotation"
          idPrefix="cluster-annotation"
          rows={edit.annotations}
          onChange={(annotations) => setEdit({ ...edit, annotations })}
          fields={[
            { key: 'key', label: 'Key', mono: true },
            { key: 'value', label: 'Value' },
          ]}
          errors={form.submitted ? annotationErrors : undefined}
        />
      </form>
    </Drawer>
  );
}

function TagList({ map, label }: { map: Record<string, string> | undefined; label: string }) {
  const entries = Object.entries(map ?? {});
  if (entries.length === 0) return <>None</>;
  return (
    <ul className={styles.tagList} aria-label={label}>
      {entries.map(([k, v]) => (
        <li key={k}>
          <Badge>
            <Mono>
              {k}={v}
            </Mono>
          </Badge>
        </li>
      ))}
    </ul>
  );
}

/** /settings/clusters/:server — details and maintenance of one cluster (API-104). */
export function Component() {
  const { server = '' } = useParams();
  const navigate = useNavigate();
  const { toast } = useToast();
  const cluster = useCluster({ server }, { refetchInterval: POLL_MS });
  const invalidate = useInvalidateClusterCache();
  const rotate = useRotateClusterAuth();
  const remove = useDeleteCluster();
  const [editing, setEditing] = useState(false);
  const [confirm, setConfirm] = useState<'delete' | 'rotate' | null>(null);
  const data = cluster.data;
  useDocumentTitle(data?.spec.name ?? server, 'Clusters');

  if (cluster.isPending) {
    return (
      <>
        <PageHeader title="Cluster" description={<Mono>{server}</Mono>} />
        <LoadingBlock label="Loading cluster" />
      </>
    );
  }
  if (cluster.isError && !data) {
    // The server answers 403 for missing and forbidden clusters alike.
    const notFound = isApiError(cluster.error) && (cluster.error.status === 404 || cluster.error.status === 403);
    return (
      <>
        <PageHeader title="Cluster" description={<Mono>{server}</Mono>} />
        {notFound ? (
          <EmptyState title="Cluster not found" action={<ButtonLink to="/settings/clusters">Back to clusters</ButtonLink>}>
            No cluster is registered for <Mono>{server}</Mono>, or you are not allowed to see it. It may have been removed.
          </EmptyState>
        ) : (
          <ErrorBanner error={cluster.error} onRetry={() => void cluster.refetch()} />
        )}
      </>
    );
  }
  if (!data) return null;

  const state = data.status?.connectionState;
  const info = data.status?.info;
  const apps = info?.applicationsCount ?? 0;
  const ref = { server: data.spec.server };
  const inCluster = isInCluster(data);

  return (
    <>
      <PageHeader
        title={data.spec.name}
        description={<Mono>{data.spec.server}</Mono>}
        actions={
          <div className={styles.actions}>
            <Button onClick={() => setEditing(true)}>Edit</Button>
            <Button
              loading={invalidate.isPending}
              onClick={() =>
                invalidate.mutate(ref, {
                  onSuccess: () => toast({ tone: 'success', title: 'Cache invalidated', description: 'The cluster cache is rebuilt on next use.' }),
                  onError: (error) => toast({ tone: 'error', title: 'Could not invalidate the cache', description: error.message }),
                })
              }
            >
              Invalidate cache
            </Button>
            <Button onClick={() => setConfirm('rotate')}>Rotate credentials</Button>
            <Button variant="danger" onClick={() => setConfirm('delete')}>
              Remove
            </Button>
          </div>
        }
      />
      <div className={styles.stack}>
        {cluster.isError ? <ErrorBanner error={cluster.error} title="Showing the last known state" onRetry={() => void cluster.refetch()} /> : null}
        <section className={styles.section} aria-labelledby="cluster-connection">
          <h2 id="cluster-connection">Connection</h2>
          <KeyValueList
            items={[
              { key: 'status', label: 'Status', value: <ConnectionStatus status={state?.status} /> },
              { key: 'message', label: 'Message', value: state?.message },
              { key: 'attempted', label: 'State since', value: state?.attemptedAt ? <Timestamp value={state.attemptedAt} /> : undefined },
              { key: 'version', label: 'Kubernetes version', value: data.status?.serverVersion },
              { key: 'credentials', label: 'Credentials', value: credentialsType(data) },
            ]}
          />
          {state?.status === 'Failed' ? (
            <Banner tone="warning" title="Patrol cannot reach this cluster">
              Applications deploying here cannot sync. Check network access from the control plane to the API server and that the
              credentials are valid; rotate them if they expired.
            </Banner>
          ) : null}
        </section>
        <section className={styles.section} aria-labelledby="cluster-general">
          <h2 id="cluster-general">General</h2>
          <KeyValueList
            items={[
              { key: 'name', label: 'Name', value: data.spec.name },
              { key: 'server', label: 'Server', value: <Mono>{data.spec.server}</Mono> },
              { key: 'project', label: 'Project', value: data.spec.project },
              { key: 'namespaces', label: 'Namespaces', value: namespacesSummary(data) },
              {
                key: 'clusterResources',
                label: 'Cluster-scoped resources',
                value: (data.spec.namespaces?.length ?? 0) === 0 || data.spec.clusterResources ? 'Managed' : 'Ignored',
              },
              { key: 'shard', label: 'Shard', value: data.spec.shard !== undefined ? String(data.spec.shard) : 'Automatic' },
              { key: 'assigned', label: 'Assigned shard', value: data.status?.assignedShard !== undefined ? String(data.status.assignedShard) : undefined },
              { key: 'labels', label: 'Labels', value: <TagList map={data.metadata.labels} label="Labels" /> },
              { key: 'annotations', label: 'Annotations', value: <TagList map={data.metadata.annotations} label="Annotations" /> },
            ]}
          />
        </section>
        <section className={styles.section} aria-labelledby="cluster-cache">
          <h2 id="cluster-cache">Applications and cache</h2>
          <KeyValueList
            items={[
              { key: 'apps', label: 'Applications', value: String(apps) },
              { key: 'apis', label: 'API groups and kinds cached', value: String(info?.cacheInfo?.apisCount ?? 0) },
              { key: 'resources', label: 'Resources cached', value: String(info?.cacheInfo?.resourcesCount ?? 0) },
              {
                key: 'sync',
                label: 'Last cache sync',
                value: info?.cacheInfo?.lastCacheSyncTime ? <Timestamp value={info.cacheInfo.lastCacheSyncTime} /> : 'Not synced yet',
              },
            ]}
          />
        </section>
        {(data.status?.conditions?.length ?? 0) > 0 ? (
          <section className={styles.section} aria-labelledby="cluster-conditions">
            <h2 id="cluster-conditions">Conditions</h2>
            <Table
              caption="Cluster conditions"
              className={styles.mediumTable}
              rows={data.status?.conditions ?? []}
              getRowKey={(c) => c.type}
              columns={[
                { id: 'type', header: 'Type', width: '160px', cell: (c) => c.type },
                { id: 'status', header: 'Status', width: '90px', cell: (c) => c.status },
                { id: 'reason', header: 'Reason', width: '180px', cell: (c) => c.reason },
                { id: 'message', header: 'Message', cell: (c) => <span title={c.message}>{c.message}</span> },
              ]}
            />
          </section>
        ) : null}
      </div>
      {editing ? <EditClusterDrawer cluster={data} onClose={() => setEditing(false)} /> : null}
      <ConfirmDialog
        open={confirm === 'rotate'}
        onClose={() => {
          setConfirm(null);
          rotate.reset();
        }}
        danger={false}
        title="Rotate cluster credentials?"
        confirmLabel="Rotate"
        action="rotate the cluster credentials"
        busy={rotate.isPending}
        error={rotate.error}
        onConfirm={() =>
          rotate.mutate(ref, {
            onSuccess: () => {
              toast({ tone: 'success', title: 'Credentials rotated', description: data.spec.name });
              setConfirm(null);
            },
          })
        }
      >
        <p>
          Patrol creates a new token for its service account in the cluster and revokes the old one. This works only for clusters
          added with <Mono>patrol cluster add</Mono> (bearer token credentials).
        </p>
      </ConfirmDialog>
      <ConfirmDialog
        open={confirm === 'delete'}
        onClose={() => {
          setConfirm(null);
          remove.reset();
        }}
        title="Remove cluster?"
        confirmLabel="Remove cluster"
        action="remove the cluster"
        confirmText={data.spec.name}
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() =>
          remove.mutate(ref, {
            onSuccess: () => {
              toast({ tone: 'success', title: 'Cluster removed', description: data.spec.name });
              setConfirm(null);
              void navigate('/settings/clusters');
            },
          })
        }
      >
        {apps > 0 ? (
          <Banner tone="warning" title={`${apps} application${apps === 1 ? '' : 's'} deploy${apps === 1 ? 's' : ''} to this cluster`}>
            They will report errors and stop syncing until the cluster is added again or their destination changes. Deployed resources
            are not deleted.
          </Banner>
        ) : null}
        <p>
          Patrol forgets <Mono>{data.spec.server}</Mono> and deletes its stored credentials. Resources running in the cluster are left
          untouched.
          {inCluster ? ' The in-cluster destination stays available while it is enabled in the Patrol settings.' : ''}
        </p>
      </ConfirmDialog>
    </>
  );
}

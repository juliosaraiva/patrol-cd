import { useMemo, useState } from 'react';
import type { Application, ResourceDiff, ApplicationResourceResponse } from '../../../../api/types';
import { Banner, Button, CodeBlock, Dialog, EmptyState, FormField, Skeleton, Switch, TextArea, useToast } from '../../../../design';
import { isApiError } from '../../../../api/errors';
import { usePatchResource, type ResourcePermission } from '../../../../api/hooks/apps-resources';
import { ErrorBanner, describeError } from '../../../../shell/errors';
import { createMergePatch, isMaskedKind, manifestYAML, parseJSONManifest, parseYAML, toYAML, withoutManagedFields, SECRET_MASK } from '../model/manifest';
import { identifierOf, type ResourceItem } from '../model/resources';
import { DiffViewer } from '../shared/DiffViewer';
import styles from './Details.module.css';

interface QueryLike<T> {
  data: T | undefined;
  isPending: boolean;
  isError: boolean;
  error: unknown;
  refetch: () => unknown;
}

export interface LiveManifestTabProps {
  app: Application;
  item: ResourceItem;
  live: QueryLike<ApplicationResourceResponse>;
  /** Result of the can-i probe for patching this resource (undefined while it runs). */
  canEdit: ResourcePermission | undefined;
  /** RBAC action and object the server checks to patch it, named in the guidance. */
  permission: { action: string; object: string };
}

/** "applications, update/apps/Deployment/shop/web on default/shop" as RBAC policy text. */
function PermissionText({ action, object }: { action: string; object: string }) {
  return (
    <>
      <code>applications, {action}</code> on <code>{object}</code>
    </>
  );
}

function ManifestSkeleton() {
  return (
    <div aria-busy="true" className={styles.tabBody}>
      <span className="sr-only">Loading manifest</span>
      <Skeleton height={16} width="50%" />
      <Skeleton height={280} />
    </div>
  );
}

/** Live manifest as YAML; editable and applied as a merge patch after reviewing the diff. */
export function LiveManifestTab({ app, item, live, canEdit, permission }: LiveManifestTabProps) {
  const toast = useToast();
  const patch = usePatchResource();
  const [hideManaged, setHideManaged] = useState(true);
  const [wrap, setWrap] = useState(false);
  const [editing, setEditing] = useState<{ baseline: Record<string, unknown>; baselineYAML: string } | null>(null);
  const [draft, setDraft] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [review, setReview] = useState<{ patch: unknown; editedYAML: string } | null>(null);
  const masked = isMaskedKind(item.kind, item.group);
  const object = useMemo(() => parseJSONManifest(live.data?.manifest), [live.data]);
  const shown = useMemo(() => (object ? (hideManaged ? withoutManagedFields(object) : object) : null), [object, hideManaged]);
  const yamlText = useMemo(() => (shown ? toYAML(shown) : ''), [shown]);
  const label = `${item.kind} ${item.name}`;

  if (live.isPending) return <ManifestSkeleton />;
  if (live.isError) return <ErrorBanner error={live.error} onRetry={() => void live.refetch()} title="Could not load the live manifest" />;
  if (!shown) {
    return (
      <EmptyState title="No live manifest" headingLevel={3}>
        <p>The cluster returned no object for this resource. It may have just been deleted; refresh the application.</p>
      </EmptyState>
    );
  }

  const startEdit = () => {
    setEditing({ baseline: shown, baselineYAML: yamlText });
    setDraft(yamlText);
    setError(null);
  };

  const startReview = () => {
    if (!editing) return;
    const parsed = parseYAML(draft);
    if (!parsed.ok) {
      setError(parsed.line ? `Line ${parsed.line}: ${parsed.message}` : parsed.message);
      return;
    }
    if (typeof parsed.value !== 'object' || parsed.value === null || Array.isArray(parsed.value)) {
      setError('The manifest must be a YAML mapping (an object), not a list or a scalar.');
      return;
    }
    const mergePatch = createMergePatch(editing.baseline, parsed.value);
    if (mergePatch === undefined) {
      setError('There are no changes to apply.');
      return;
    }
    setError(null);
    patch.reset();
    setReview({ patch: mergePatch, editedYAML: toYAML(parsed.value) });
  };

  const apply = () => {
    if (!review) return;
    patch.mutate(
      {
        name: app.metadata.name ?? '',
        scope: { appNamespace: app.metadata.namespace, project: app.spec.project },
        resource: identifierOf(item),
        patch: { patch: JSON.stringify(review.patch), patchType: 'merge' },
      },
      {
        onSuccess: () => {
          setReview(null);
          setEditing(null);
          toast.toast({ title: `Changes applied to ${label}`, tone: 'success' });
        },
      },
    );
  };

  return (
    <div className={styles.tabBody}>
      {masked ? (
        <Banner tone="info" title="Secret values are masked">
          The server replaces every value under <code>data</code> and <code>stringData</code> with <code>{SECRET_MASK}</code>. Editing is disabled
          for Secrets because applying the masked text would overwrite the real values.
        </Banner>
      ) : null}
      <div className={styles.manifestToolbar}>
        <Switch checked={hideManaged} onChange={setHideManaged} label="Hide managed fields" disabled={editing !== null} />
        <Switch checked={wrap} onChange={setWrap} label="Wrap lines" />
        {editing === null && !masked && canEdit !== undefined && canEdit !== 'no' ? (
          <Button size="sm" onClick={startEdit}>
            Edit
          </Button>
        ) : null}
      </div>
      {canEdit === 'no' && !masked ? (
        <p className={styles.muted}>
          Editing needs the permission <PermissionText {...permission} />. Ask an administrator if you need it.
        </p>
      ) : null}
      {canEdit === 'inherited' && !masked ? (
        <p className={styles.muted}>
          Your role allows <code>applications, update</code> on this application but not <PermissionText {...permission} />. The server
          applies your edit only when fine-grained RBAC inheritance is enabled (
          <code>server.rbac.disableApplicationFineGrainedRBACInheritance: &quot;false&quot;</code>); otherwise it refuses it.
        </p>
      ) : null}
      {editing ? (
        <div className={styles.editor}>
          <FormField label={`Edit ${label} (YAML)`} help="Changes are sent as a JSON merge patch: only the fields you change are applied." error={error ?? undefined}>
            <TextArea mono rows={24} value={draft} onChange={(e) => setDraft(e.target.value)} spellCheck={false} className={wrap ? undefined : styles.noWrap} />
          </FormField>
          <div className={styles.editorActions}>
            <Button variant="primary" onClick={startReview}>
              Review changes
            </Button>
            <Button
              onClick={() => {
                setEditing(null);
                setError(null);
              }}
            >
              Cancel
            </Button>
          </div>
        </div>
      ) : (
        <CodeBlock code={yamlText} title="Live manifest" label={`Live manifest of ${label}`} language="yaml" wrap={wrap} maxHeight="60vh" />
      )}
      <Dialog
        open={review !== null}
        onClose={() => setReview(null)}
        size="lg"
        title={`Review changes to ${label}`}
        description="Check the difference between the current live object and your edit before applying it."
        footer={
          <>
            <Button onClick={() => setReview(null)}>Back to editing</Button>
            <Button variant="primary" loading={patch.isPending} onClick={apply}>
              Apply changes
            </Button>
          </>
        }
      >
        {patch.isError && isApiError(patch.error) && patch.error.code === 'permission_denied' ? (
          <Banner tone="error" live title="You may not change this resource">
            Applying needs the permission <PermissionText {...permission} />. Ask an administrator to grant it. Server response: {patch.error.detail}
          </Banner>
        ) : patch.isError ? (
          <ErrorBanner error={patch.error} title={`Could not apply: ${describeError(patch.error).title}`} />
        ) : null}
        {review && editing ? (
          <DiffViewer caption={`Changes to ${label}`} oldText={editing.baselineYAML} newText={review.editedYAML} oldLabel="Current" newLabel="Edited" compact inline />
        ) : null}
      </Dialog>
    </div>
  );
}

export interface DesiredManifestTabProps {
  item: ResourceItem;
  diff: ResourceDiff | undefined;
  loading: boolean;
  error: unknown;
  onRetry: () => void;
}

/** The manifest rendered from the source (target state). */
export function DesiredManifestTab({ item, diff, loading, error, onRetry }: DesiredManifestTabProps) {
  const [wrap, setWrap] = useState(false);
  if (loading) return <ManifestSkeleton />;
  if (error) return <ErrorBanner error={error} onRetry={onRetry} title="Could not load the desired manifest" />;
  const text = manifestYAML(diff?.targetState);
  if (!text) {
    return (
      <EmptyState title={item.requiresPruning ? 'Not in the source anymore' : 'No desired manifest'} headingLevel={3}>
        <p>
          {item.requiresPruning
            ? 'This resource was removed from the source. Sync with pruning enabled to delete it from the cluster.'
            : 'The source does not render this resource. Refresh the application if the source changed recently.'}
        </p>
      </EmptyState>
    );
  }
  return (
    <div className={styles.tabBody}>
      <div className={styles.manifestToolbar}>
        <Switch checked={wrap} onChange={setWrap} label="Wrap lines" />
      </div>
      <CodeBlock code={text} title="Desired manifest" label={`Desired manifest of ${item.kind} ${item.name}`} language="yaml" wrap={wrap} maxHeight="60vh" />
    </div>
  );
}

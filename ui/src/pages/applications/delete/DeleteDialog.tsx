import { useState } from 'react';
import { useDeleteApplication } from '../../../api/queries/applications';
import type { Application } from '../../../api/types';
import { Banner, Button, Dialog, FormField, Input, Mono, RadioGroup, useToast } from '../../../design';
import { appName, appScope, FINALIZER_RESOURCES, GROUP, isDeleting } from '../shared/app';
import { ActionError } from '../shared/errors';

export type DeletionMode = 'foreground' | 'background' | 'non-cascading';

const MODES: { value: DeletionMode; label: string; description: string }[] = [
  {
    value: 'foreground',
    label: 'Foreground (cascade)',
    description: 'Deletes the managed resources first and removes the application once they are gone.',
  },
  {
    value: 'background',
    label: 'Background (cascade)',
    description: 'Removes the application immediately; the managed resources are deleted asynchronously afterwards.',
  },
  {
    value: 'non-cascading',
    label: 'Non-cascading',
    description: 'Removes only the application. Managed resources stay in the cluster, unmanaged.',
  },
];

/** API query for a deletion mode (DELETE /applications/{name}?cascade=&propagationPolicy=). */
export function deletionQuery(mode: DeletionMode): { cascade: boolean; propagationPolicy?: 'foreground' | 'background' } {
  if (mode === 'non-cascading') return { cascade: false };
  return { cascade: true, propagationPolicy: mode };
}

const FINALIZER_HELP: Record<string, string> = {
  [FINALIZER_RESOURCES]: 'cascade-deletes managed resources (foreground) before the application disappears',
  [`${FINALIZER_RESOURCES}/foreground`]: 'cascade-deletes managed resources in the foreground',
  [`${FINALIZER_RESOURCES}/background`]: 'cascade-deletes managed resources in the background',
  [`pre-delete-finalizer.${GROUP}`]: 'runs PreDelete hooks before deletion proceeds',
  [`pre-delete-finalizer.${GROUP}/cleanup`]: 'cleans up PreDelete hooks',
  [`post-delete-finalizer.${GROUP}`]: 'runs PostDelete hooks after the resources are deleted',
  [`post-delete-finalizer.${GROUP}/cleanup`]: 'cleans up PostDelete hooks',
};

export interface DeleteDialogProps {
  app: Application;
  open: boolean;
  onClose: () => void;
  /** Called after the server accepted the deletion. */
  onDeleted?: () => void;
}

/** Type-to-confirm application deletion with cascade and propagation choices. */
export function DeleteDialog({ app, open, onClose, onDeleted }: DeleteDialogProps) {
  if (!open) return null;
  return <DeleteDialogBody app={app} onClose={onClose} onDeleted={onDeleted} />;
}

function DeleteDialogBody({ app, onClose, onDeleted }: Omit<DeleteDialogProps, 'open'>) {
  const name = appName(app);
  const del = useDeleteApplication();
  const { toast } = useToast();
  const [typed, setTyped] = useState('');
  const [mode, setMode] = useState<DeletionMode>('foreground');
  const [attempted, setAttempted] = useState(false);
  const deleting = isDeleting(app);
  const finalizers = app.metadata.finalizers ?? [];
  const matches = typed.trim() === name;

  const submit = async () => {
    setAttempted(true);
    if (!matches || deleting) return;
    try {
      await del.mutateAsync({ name, scope: appScope(app), ...deletionQuery(mode) });
      toast({
        tone: 'success',
        title: `Deleting ${name}`,
        description: mode === 'non-cascading' ? 'The application is removed; its resources stay in the cluster.' : 'The application and its resources are being deleted.',
      });
      onClose();
      onDeleted?.();
    } catch {
      // Rendered below.
    }
  };

  return (
    <Dialog
      open
      onClose={onClose}
      title={`Delete ${name}?`}
      description="Deleting an application cannot be undone."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" loading={del.isPending} disabled={deleting} onClick={() => void submit()}>
            Delete application
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
        style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}
      >
        {deleting ? (
          <Banner tone="info" title="Deletion in progress">
            This application is already being deleted; it disappears once its finalizers finish.
          </Banner>
        ) : null}
        <RadioGroup legend="Deletion mode" value={mode} onChange={setMode} options={MODES} />
        <div>
          <p>
            <strong>Finalizers.</strong> Kubernetes keeps the application until every finalizer is removed. Cascading deletion adds the
            resources finalizer so the controller can delete what the application manages first.
          </p>
          {finalizers.length > 0 ? (
            <ul>
              {finalizers.map((f) => (
                <li key={f}>
                  <Mono>{f}</Mono>
                  {FINALIZER_HELP[f] ? `: ${FINALIZER_HELP[f]}` : ''}
                </li>
              ))}
            </ul>
          ) : (
            <p>This application has no finalizers yet.</p>
          )}
          {mode === 'non-cascading' && finalizers.some((f) => f.startsWith(FINALIZER_RESOURCES)) ? (
            <p>Non-cascading deletion removes the resources finalizer first, so the resources are left in place.</p>
          ) : null}
        </div>
        <FormField
          label={
            <>
              Type <Mono>{name}</Mono> to confirm
            </>
          }
          error={attempted && !matches ? 'The name does not match.' : undefined}
        >
          <Input mono value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false} />
        </FormField>
        {del.isError ? <ActionError error={del.error} action="delete" onRetry={() => void submit()} /> : null}
      </form>
    </Dialog>
  );
}

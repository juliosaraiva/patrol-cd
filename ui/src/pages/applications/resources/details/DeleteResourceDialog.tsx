import { useState } from 'react';
import { useDeleteResource } from '../../../../api/hooks/apps-resources';
import type { Application } from '../../../../api/types';
import { Button, Dialog, FormField, Input, Mono, RadioGroup, useToast } from '../../../../design';
import { ErrorBanner } from '../../../../shell/errors';
import { identifierOf, type ResourceItem } from '../model/resources';
import styles from './Details.module.css';

export type DeletionMode = 'foreground' | 'force' | 'orphan';

const MODES: { value: DeletionMode; label: string; description: string }[] = [
  {
    value: 'foreground',
    label: 'Foreground delete',
    description: 'Kubernetes deletes the dependents first (for example the Pods of a ReplicaSet), then this resource.',
  },
  {
    value: 'force',
    label: 'Force delete',
    description: 'Deletes immediately without a grace period; dependents are removed in the background. Use only for stuck resources.',
  },
  {
    value: 'orphan',
    label: 'Non-cascading (orphan) delete',
    description: 'Deletes only this resource and leaves its dependents running without an owner.',
  },
];

export interface DeleteResourceDialogProps {
  open: boolean;
  onClose: () => void;
  app: Application;
  item: ResourceItem;
  onDeleted: () => void;
}

/** Confirms deleting one live resource, with the propagation mode explained. */
export function DeleteResourceDialog({ open, onClose, app, item, onDeleted }: DeleteResourceDialogProps) {
  const [mode, setMode] = useState<DeletionMode>('foreground');
  const [typed, setTyped] = useState('');
  const remove = useDeleteResource();
  const toast = useToast();
  const label = `${item.kind} ${item.name}`;
  const confirmed = typed.trim() === item.name;

  const submit = () => {
    if (!confirmed) return;
    remove.mutate(
      {
        name: app.metadata.name ?? '',
        scope: { appNamespace: app.metadata.namespace, project: app.spec.project },
        resource: identifierOf(item),
        force: mode === 'force' || undefined,
        orphan: mode === 'orphan' || undefined,
      },
      {
        onSuccess: () => {
          toast.toast({ title: `Deletion of ${label} requested`, tone: 'success' });
          setTyped('');
          onDeleted();
        },
      },
    );
  };

  return (
    <Dialog
      open={open}
      onClose={() => {
        remove.reset();
        setTyped('');
        onClose();
      }}
      title={`Delete ${label}?`}
      description="Deletes the live object from the cluster. The application's source is not changed."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" disabled={!confirmed} loading={remove.isPending} onClick={submit}>
            Delete {item.kind}
          </Button>
        </>
      }
    >
      <form
        className={styles.dialogBody}
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <RadioGroup legend="Deletion mode" value={mode} onChange={setMode} options={MODES} />
        {item.managed ? (
          <p className={styles.warningText}>
            This resource is still part of the application. If automated sync is enabled, Patrol creates it again on the next sync.
          </p>
        ) : null}
        <FormField
          label={
            <>
              Type <Mono>{item.name}</Mono> to confirm
            </>
          }
        >
          <Input value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false} mono />
        </FormField>
        {remove.isError ? <ErrorBanner error={remove.error} title="Deletion failed" /> : null}
      </form>
    </Dialog>
  );
}

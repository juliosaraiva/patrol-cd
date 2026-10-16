import { usePatchApplication } from '../../../api/hooks/apps-core';
import type { Application } from '../../../api/types';
import { Button, Dialog, Mono, useToast } from '../../../design';
import { ANNOTATION_DELETION_APPROVED, appName, appScope, resourceKey } from '../shared/app';
import { ActionError } from '../shared/errors';

/** Resources held by Prune=confirm or Delete=confirm. */
export function resourcesAwaitingApproval(app: Application) {
  return (app.status?.resources ?? []).filter((r) => r.requiresDeletionConfirmation);
}

/** Approves pending deletions by setting cd.patrol.example/deletion-approved to now. */
export function ApproveDeletionDialog({ app, open, onClose }: { app: Application; open: boolean; onClose: () => void }) {
  const approve = usePatchApplication();
  const { toast } = useToast();
  const awaiting = resourcesAwaitingApproval(app);
  const run = async () => {
    try {
      await approve.mutateAsync({
        name: appName(app),
        scope: appScope(app),
        patch: { patchType: 'merge', patch: JSON.stringify({ metadata: { annotations: { [ANNOTATION_DELETION_APPROVED]: new Date().toISOString() } } }) },
      });
      toast({ tone: 'success', title: 'Deletion approved', description: 'Resources waiting for confirmation will be pruned or deleted.' });
      approve.reset();
      onClose();
    } catch {
      // Rendered below.
    }
  };
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Approve deletion?"
      description="Sets the deletion approval annotation so the waiting resources are pruned or deleted."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" loading={approve.isPending} onClick={() => void run()}>
            Approve deletion
          </Button>
        </>
      }
    >
      <ul>
        {awaiting.slice(0, 50).map((r) => (
          <li key={resourceKey(r)}>
            <Mono>
              {r.kind} {r.namespace ? `${r.namespace}/` : ''}
              {r.name}
            </Mono>
          </li>
        ))}
        {awaiting.length > 50 ? <li>and {awaiting.length - 50} more</li> : null}
      </ul>
      {approve.isError ? <ActionError error={approve.error} action="approve deletion" onRetry={() => void run()} /> : null}
    </Dialog>
  );
}

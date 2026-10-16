import { useTerminateOperation } from '../../../api/queries/applications';
import type { Application } from '../../../api/types';
import { Button, Dialog, useToast } from '../../../design';
import { appName, appScope } from '../shared/app';
import { ActionError } from '../shared/errors';

/** Confirmation before DELETE .../operation (cancels the running sync or rollback). */
export function TerminateDialog({ app, open, onClose, onTerminated }: { app: Application; open: boolean; onClose: () => void; onTerminated?: () => void }) {
  const terminate = useTerminateOperation();
  const { toast } = useToast();
  const name = appName(app);
  const run = async () => {
    try {
      await terminate.mutateAsync({ name, scope: appScope(app) });
      toast({ tone: 'success', title: 'Termination requested', description: `The running operation of ${name} is being cancelled.` });
      terminate.reset();
      onClose();
      onTerminated?.();
    } catch {
      // Rendered below.
    }
  };
  return (
    <Dialog
      open={open}
      onClose={() => {
        terminate.reset();
        onClose();
      }}
      title="Terminate the running operation?"
      description="The controller stops after the current step. Resources already applied stay applied, so the application may be left partially synced."
      footer={
        <>
          <Button onClick={onClose}>Keep running</Button>
          <Button variant="danger" loading={terminate.isPending} onClick={() => void run()}>
            Terminate
          </Button>
        </>
      }
    >
      {terminate.isError ? <ActionError error={terminate.error} action="terminate the operation" onRetry={() => void run()} /> : null}
    </Dialog>
  );
}

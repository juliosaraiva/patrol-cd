import { useUpdateApplication } from '../../../api/queries/applications';
import type { Application } from '../../../api/types';
import { Button, Dialog, useToast } from '../../../design';
import { appName, cloneJSON, isAutoSyncEnabled } from '../shared/app';
import { ActionError } from '../shared/errors';

/** Application with automated sync switched on or off (prune/self-heal settings kept). */
export function withAutoSync(app: Application, enabled: boolean): Application {
  const next = cloneJSON(app);
  const policy = { ...(next.spec.syncPolicy ?? {}) };
  policy.automated = { ...policy.automated, enabled };
  next.spec.syncPolicy = policy;
  return next;
}

/** Confirmation for the action bar's auto-sync toggle (parity API-093). */
export function AutoSyncDialog({ app, open, onClose }: { app: Application; open: boolean; onClose: () => void }) {
  const update = useUpdateApplication();
  const { toast } = useToast();
  const enabled = isAutoSyncEnabled(app);
  const run = async () => {
    try {
      await update.mutateAsync({ app: withAutoSync(app, !enabled), validate: false });
      toast({ tone: 'success', title: enabled ? `Auto-sync disabled for ${appName(app)}` : `Auto-sync enabled for ${appName(app)}` });
      update.reset();
      onClose();
    } catch {
      // Rendered below.
    }
  };
  return (
    <Dialog
      open={open}
      onClose={() => {
        update.reset();
        onClose();
      }}
      title={enabled ? 'Disable auto-sync?' : 'Enable auto-sync?'}
      description={
        enabled
          ? 'New revisions are no longer applied automatically; sync manually until you enable it again. Prune and self-heal settings are kept.'
          : 'The application syncs automatically whenever it is out of sync with its target revision.'
      }
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={update.isPending} onClick={() => void run()}>
            {enabled ? 'Disable auto-sync' : 'Enable auto-sync'}
          </Button>
        </>
      }
    >
      {update.isError ? <ActionError error={update.error} action="change auto-sync" onRetry={() => void run()} onReload={() => void run()} /> : null}
    </Dialog>
  );
}

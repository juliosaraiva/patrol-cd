import { useRef, useState } from 'react';
import { useDeleteApplicationSet } from '../../api/hooks/applicationsets';
import type { ApplicationSet } from '../../api/types';
import { Banner, Button, Dialog, FormField, Input, Mono } from '../../design';
import { ErrorBanner } from '../../shell/errors';
import { deletionImpact, type GeneratedAppRow } from './model';
import styles from './ApplicationSets.module.css';

const LISTED = 10;

/**
 * Delete confirmation (API-028 delete): explains what happens to generated Applications and
 * their resources (preserveResourcesOnDeletion, deletion order) and requires typing the name.
 */
export function DeleteApplicationSetDialog({
  set,
  rows,
  open,
  onClose,
  onDeleted,
}: {
  set: ApplicationSet;
  rows: GeneratedAppRow[];
  open: boolean;
  onClose: () => void;
  onDeleted: () => void;
}) {
  const name = set.metadata.name ?? '';
  const [typed, setTyped] = useState('');
  const del = useDeleteApplicationSet();
  const inputRef = useRef<HTMLInputElement>(null);
  const impact = deletionImpact(set);
  const close = () => {
    setTyped('');
    del.reset();
    onClose();
  };
  return (
    <Dialog
      open={open}
      onClose={close}
      title={`Delete ApplicationSet ${name}?`}
      description="The ApplicationSet stops generating Applications. This cannot be undone."
      initialFocusRef={inputRef}
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button
            variant="danger"
            disabled={typed !== name}
            loading={del.isPending}
            onClick={() =>
              del.mutate(
                { name, namespace: set.metadata.namespace },
                {
                  onSuccess: () => {
                    setTyped('');
                    onDeleted();
                  },
                },
              )
            }
          >
            Delete ApplicationSet
          </Button>
        </>
      }
    >
      <div className={styles.stack}>
        <Banner tone={impact.preserve ? 'info' : 'warning'} title={impact.preserve ? 'Deployed resources are kept' : 'Deployed resources may be deleted'}>
          {impact.summary}
        </Banner>
        {impact.order ? <p className={styles.muted}>{impact.order}</p> : null}
        {rows.length > 0 ? (
          <div>
            <p className={styles.muted}>{`${rows.length} generated Application${rows.length === 1 ? '' : 's'} will be deleted:`}</p>
            <ul className={styles.plainList}>
              {rows.slice(0, LISTED).map((r) => (
                <li key={r.key}>
                  <Mono>{r.name}</Mono>
                </li>
              ))}
              {rows.length > LISTED ? <li>{`and ${rows.length - LISTED} more`}</li> : null}
            </ul>
          </div>
        ) : (
          <p className={styles.muted}>No generated Applications exist.</p>
        )}
        <FormField label={<>Type <Mono>{name}</Mono> to confirm</>}>
          <Input ref={inputRef} mono autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />
        </FormField>
        {del.isError ? <ErrorBanner error={del.error} title="The ApplicationSet was not deleted" /> : null}
      </div>
    </Dialog>
  );
}

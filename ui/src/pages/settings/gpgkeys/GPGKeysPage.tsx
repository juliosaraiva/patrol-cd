import { useId, useState, type FormEvent } from 'react';
import { Link } from 'react-router';
import { useCreateGPGKey, useDeleteGPGKey } from '../../../api/hooks/settings';
import { useGPGKeys } from '../../../api/queries/resources';
import type { GnuPGPublicKey } from '../../../api/types';
import {
  Badge,
  Button,
  Dialog,
  EmptyState,
  FormField,
  IconButton,
  IconClose,
  IconPlus,
  Mono,
  PageHeader,
  Table,
  TextArea,
  useToast,
  type BadgeTone,
  type Column,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { ActionError, ConfirmDialog, ErrorSummary, LoadingBlock, useFormErrors } from '../common/feedback';
import styles from '../common/common.module.css';
import { checkArmoredPublicKey } from '../common/trustData';

const TRUST_TONE: Record<string, BadgeTone> = { ultimate: 'success', full: 'success', marginal: 'warning', never: 'danger' };

function AddKeyDialog({ onClose }: { onClose: () => void }) {
  const formId = useId();
  const { toast } = useToast();
  const create = useCreateGPGKey();
  const [keyData, setKeyData] = useState('');
  const keyError = checkArmoredPublicKey(keyData);
  const form = useFormErrors(keyError ? [{ id: 'gpg-key', message: keyError }] : []);

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    create.mutate(
      { keyData: keyData.trim() },
      {
        onSuccess: (result) => {
          const created = result.created?.items ?? [];
          const skipped = result.skipped ?? [];
          if (created.length > 0) {
            toast({ tone: 'success', title: created.length === 1 ? 'GnuPG key added' : `${created.length} GnuPG keys added`, description: created.map((k) => k.keyID).join(', ') });
          }
          if (skipped.length > 0) {
            toast({ tone: 'info', title: 'Already trusted', description: `Skipped existing key ${skipped.join(', ')}.` });
          }
          onClose();
        },
      },
    );
  };

  return (
    <Dialog
      open
      onClose={onClose}
      size="lg"
      title="Add GnuPG public key"
      description="Keys added here can be listed as signature keys in projects; commits signed by them pass verification."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form={formId} loading={create.isPending}>
            Add key
          </Button>
        </>
      }
    >
      <form id={formId} className={styles.form} onSubmit={onSubmit} noValidate aria-label="Add GnuPG public key">
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {create.error ? <ActionError error={create.error} action="add the GnuPG key" /> : null}
        <FormField
          label="ASCII-armored public key"
          id="gpg-key"
          required
          error={form.errorFor('gpg-key')}
          help={
            <>
              Export it with <Mono>gpg --armor --export &lt;key ID&gt;</Mono>. Private keys are rejected.
            </>
          }
        >
          <TextArea value={keyData} onChange={(e) => setKeyData(e.target.value)} mono rows={10} placeholder="-----BEGIN PGP PUBLIC KEY BLOCK-----" spellCheck={false} />
        </FormField>
      </form>
    </Dialog>
  );
}

/** /settings/gpgkeys — GnuPG public keys for commit verification (API-103). */
export function Component() {
  useDocumentTitle('GnuPG keys', 'Settings');
  const { toast } = useToast();
  const keys = useGPGKeys();
  const remove = useDeleteGPGKey();
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState<GnuPGPublicKey | undefined>();
  const items = keys.data?.items ?? [];

  const columns: Column<GnuPGPublicKey>[] = [
    { id: 'id', header: 'Key ID', width: '170px', cell: (k) => <Mono>{k.keyID}</Mono>, sortValue: (k) => k.keyID },
    { id: 'fingerprint', header: 'Fingerprint', cell: (k) => <Mono title={k.fingerprint}>{k.fingerprint ?? '—'}</Mono> },
    { id: 'owner', header: 'Owner', cell: (k) => <span title={k.owner}>{k.owner ?? '—'}</span>, sortValue: (k) => k.owner },
    { id: 'type', header: 'Type', width: '100px', cell: (k) => k.subType ?? '—', sortValue: (k) => k.subType },
    { id: 'trust', header: 'Trust', width: '100px', cell: (k) => <Badge tone={TRUST_TONE[k.trust ?? ''] ?? 'neutral'}>{k.trust ?? 'unknown'}</Badge>, sortValue: (k) => k.trust },
    {
      id: 'actions',
      header: 'Actions',
      hideHeader: true,
      align: 'end',
      width: '56px',
      cell: (k) => <IconButton aria-label={`Remove key ${k.keyID ?? ''}`} icon={<IconClose />} size="sm" onClick={() => setDeleting(k)} />,
    },
  ];

  let content;
  if (keys.isPending) content = <LoadingBlock label="Loading GnuPG keys" />;
  else if (keys.isError) content = <ErrorBanner error={keys.error} onRetry={() => void keys.refetch()} />;
  else if (items.length === 0) {
    content = (
      <EmptyState
        title="No GnuPG keys"
        action={
          <Button variant="primary" icon={<IconPlus />} onClick={() => setAdding(true)}>
            Add GnuPG key
          </Button>
        }
      >
        Add the public keys of everyone who signs commits, then list them as signature keys in{' '}
        <Link to="/settings/projects">projects</Link> that require signed commits.
      </EmptyState>
    );
  } else {
    content = <Table caption="GnuPG public keys" className={styles.wideTable} columns={columns} rows={items} getRowKey={(k) => k.keyID ?? ''} defaultSort={{ columnId: 'id', direction: 'ascending' }} />;
  }

  return (
    <>
      <PageHeader
        title="GnuPG keys"
        description="Public keys trusted for commit signature verification."
        actions={
          items.length > 0 ? (
            <Button variant="primary" icon={<IconPlus />} onClick={() => setAdding(true)}>
              Add GnuPG key
            </Button>
          ) : undefined
        }
      />
      {content}
      {adding ? <AddKeyDialog onClose={() => setAdding(false)} /> : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onClose={() => {
          setDeleting(undefined);
          remove.reset();
        }}
        title="Remove GnuPG key?"
        confirmLabel="Remove key"
        action="remove the GnuPG key"
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          if (!deleting?.keyID) return;
          remove.mutate(deleting.keyID, {
            onSuccess: () => {
              toast({ tone: 'success', title: 'GnuPG key removed', description: deleting.keyID });
              setDeleting(undefined);
            },
          });
        }}
      >
        <p>
          Commits signed only by <Mono>{deleting?.keyID}</Mono> ({deleting?.owner}) will fail verification in projects that require
          signatures.
        </p>
      </ConfirmDialog>
    </>
  );
}

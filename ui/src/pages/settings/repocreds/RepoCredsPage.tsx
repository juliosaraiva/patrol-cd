import { useState } from 'react';
import { Link } from 'react-router';
import { useDeleteRepositoryCredential } from '../../../api/hooks/settings';
import { useRepositoryCredentials, useSettings } from '../../../api/queries/resources';
import type { RepositoryCredentialInfo } from '../../../api/types';
import {
  Badge,
  Button,
  EmptyState,
  IconButton,
  IconClose,
  IconPlus,
  Mono,
  PageHeader,
  Table,
  Tabs,
  useToast,
  type Column,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { useTabParam } from '../common/editors';
import { ConfirmDialog, LoadingBlock } from '../common/feedback';
import styles from '../common/common.module.css';
import { METHOD_LABELS, methodOf } from '../repositories/model';
import { RepositoryForm } from '../repositories/RepositoryForm';

type Purpose = 'read' | 'write';

function authSummary(cred: RepositoryCredentialInfo): string {
  const spec = cred.spec;
  if (spec.scmProviderRef) return `SCM provider ${spec.scmProviderRef.name}`;
  if (spec.useAzureWorkloadIdentity) return 'Azure workload identity';
  return METHOD_LABELS[methodOf(spec, spec.urlPrefix)];
}

function CredentialList({ purpose }: { purpose: Purpose }) {
  const { toast } = useToast();
  const creds = useRepositoryCredentials(purpose);
  const remove = useDeleteRepositoryCredential(purpose);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<RepositoryCredentialInfo | undefined>();
  const noun = purpose === 'write' ? 'write credential template' : 'credential template';
  const items = creds.data?.items ?? [];

  const columns: Column<RepositoryCredentialInfo>[] = [
    { id: 'prefix', header: 'URL prefix', cell: (c) => <Mono title={c.spec.urlPrefix}>{c.spec.urlPrefix}</Mono>, sortValue: (c) => c.spec.urlPrefix },
    { id: 'type', header: 'Type', width: '80px', cell: (c) => <Badge>{c.spec.type ?? 'git'}</Badge>, sortValue: (c) => c.spec.type },
    { id: 'auth', header: 'Authentication', width: '200px', cell: authSummary, sortValue: authSummary },
    {
      // hasCredentials and the (non-secret) username come from RepositoryCredentialInfo.
      id: 'credentials',
      header: 'Credentials',
      width: '200px',
      cell: (c) => (c.hasCredentials ? (c.username ? `Stored (user ${c.username})` : 'Stored') : 'None'),
    },
    {
      id: 'actions',
      header: 'Actions',
      hideHeader: true,
      width: '56px',
      align: 'end',
      cell: (c) => <IconButton aria-label={`Delete template ${c.spec.urlPrefix}`} icon={<IconClose />} size="sm" onClick={() => setDeleting(c)} />,
    },
  ];

  let content;
  if (creds.isPending) content = <LoadingBlock label="Loading credential templates" />;
  else if (creds.isError) content = <ErrorBanner error={creds.error} onRetry={() => void creds.refetch()} />;
  else if (items.length === 0) {
    content = (
      <EmptyState
        title={purpose === 'write' ? 'No write credential templates' : 'No credential templates'}
        action={
          <Button variant="primary" icon={<IconPlus />} onClick={() => setCreating(true)}>
            New template
          </Button>
        }
      >
        Templates let many repositories share one set of credentials: any repository whose URL starts with the template prefix uses
        them unless it has its own.
      </EmptyState>
    );
  } else {
    content = (
      <Table
        caption={purpose === 'write' ? 'Write credential templates' : 'Credential templates'}
        className={styles.mediumTable}
        columns={columns}
        rows={items}
        getRowKey={(c) => c.spec.urlPrefix}
        defaultSort={{ columnId: 'prefix', direction: 'ascending' }}
      />
    );
  }

  return (
    <div className={styles.stack}>
      <div className={styles.sectionHeader}>
        <p className={styles.muted}>
          {purpose === 'write'
            ? 'Push credentials for the source hydrator, matched by URL prefix.'
            : 'Credentials matched by URL prefix. Secret values are write-only and stored in Kubernetes Secrets.'}
        </p>
        <Button variant="primary" icon={<IconPlus />} onClick={() => setCreating(true)}>
          New template
        </Button>
      </div>
      {content}
      {creating ? <RepositoryForm open onClose={() => setCreating(false)} mode="template" purpose={purpose} /> : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onClose={() => {
          setDeleting(undefined);
          remove.reset();
        }}
        title={`Delete ${noun}?`}
        confirmLabel="Delete"
        action={`delete the ${noun}`}
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          if (!deleting) return;
          remove.mutate(deleting.spec.urlPrefix, {
            onSuccess: () => {
              toast({ tone: 'success', title: 'Credential template deleted', description: deleting.spec.urlPrefix });
              setDeleting(undefined);
            },
          });
        }}
      >
        <p>
          Repositories under <Mono>{deleting?.spec.urlPrefix}</Mono> that rely on this template lose access unless they have their own
          credentials.
        </p>
      </ConfirmDialog>
    </div>
  );
}

/** /settings/repocreds — credential templates (API-101). */
export function Component() {
  useDocumentTitle('Repository credentials', 'Settings');
  const settings = useSettings();
  const [tab, setTab] = useTabParam(['read', 'write'] as const, 'read', 'purpose');
  const hydrator = settings.data?.hydratorEnabled === true;
  return (
    <>
      <PageHeader
        title="Repository credentials"
        description={
          <>
            Credential templates applied to every repository whose URL starts with the template prefix. Individual repositories are
            managed under <Link to="/settings/repos">Repositories</Link>.
          </>
        }
      />
      {hydrator ? (
        <Tabs
          label="Credential purpose"
          value={tab}
          onValueChange={setTab}
          items={[
            { id: 'read', label: 'Read', content: <CredentialList purpose="read" /> },
            { id: 'write', label: 'Write (source hydrator)', content: <CredentialList purpose="write" /> },
          ]}
        />
      ) : (
        <CredentialList purpose="read" />
      )}
    </>
  );
}

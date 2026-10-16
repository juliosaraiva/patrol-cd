import { useEffect, useId, useState, type FormEvent } from 'react';
import { useCreateCertificates, useDeleteCertificates } from '../../../api/hooks/settings';
import { useCertificates } from '../../../api/queries/resources';
import type { RepositoryCertificate } from '../../../api/types';
import {
  Badge,
  Button,
  Checkbox,
  Dialog,
  EmptyState,
  FormField,
  IconButton,
  IconClose,
  IconPlus,
  Input,
  Mono,
  PageHeader,
  Table,
  Tabs,
  TextArea,
  useToast,
  type Column,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { useTabParam } from '../common/editors';
import { ActionError, ConfirmDialog, ErrorSummary, LoadingBlock, useFormErrors } from '../common/feedback';
import styles from '../common/common.module.css';
import { checkPEMCertificates, parseKnownHosts, sshFingerprint } from '../common/trustData';

type CertKind = 'https' | 'ssh';

const certKey = (c: RepositoryCertificate) => `${c.certType}|${c.certSubType ?? ''}|${c.serverName}`;

/** Host names only: no scheme, path or port. */
function validateServerName(value: string): string | null {
  const v = value.trim();
  if (v === '') return 'Server name is required.';
  if (/[/:\s]/.test(v)) return 'Enter only the host name, for example git.example.com (no https://, port or path).';
  if (!/^[A-Za-z0-9*]([A-Za-z0-9-*]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$/.test(v)) return 'Enter a valid host name.';
  return null;
}

function AddTLSDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const formId = useId();
  const { toast } = useToast();
  const create = useCreateCertificates();
  const [serverName, setServerName] = useState('');
  const [pem, setPem] = useState('');
  const [replace, setReplace] = useState(false);
  const pemCheck = checkPEMCertificates(pem);
  const form = useFormErrors(
    [
      { id: 'tls-server', message: validateServerName(serverName) },
      { id: 'tls-pem', message: pemCheck.error },
    ].filter((e): e is { id: string; message: string } => e.message !== null),
  );

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    create.mutate(
      { body: { items: [{ serverName: serverName.trim(), certType: 'https', certData: pem.trim() }] }, upsert: replace },
      {
        onSuccess: () => {
          toast({ tone: 'success', title: 'TLS certificate added', description: serverName.trim() });
          onClose();
        },
      },
    );
  };

  return (
    <Dialog
      open={open}
      onClose={onClose}
      size="lg"
      title="Add TLS certificate"
      description="Trust a self-signed or private CA certificate when connecting to a repository server over HTTPS."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form={formId} loading={create.isPending}>
            Add certificate
          </Button>
        </>
      }
    >
      <form id={formId} className={styles.form} onSubmit={onSubmit} noValidate aria-label="Add TLS certificate">
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {create.error ? <ActionError error={create.error} action="add the TLS certificate" /> : null}
        <FormField label="Repository server name" id="tls-server" required error={form.errorFor('tls-server')} help="The host name in repository URLs, e.g. git.example.com.">
          <Input value={serverName} onChange={(e) => setServerName(e.target.value)} mono autoComplete="off" spellCheck={false} />
        </FormField>
        <FormField
          label="Certificate (PEM)"
          id="tls-pem"
          required
          error={form.errorFor('tls-pem')}
          help={pemCheck.count > 0 ? `${pemCheck.count} certificate${pemCheck.count === 1 ? '' : 's'} detected.` : 'Paste one or more certificates, each between BEGIN CERTIFICATE and END CERTIFICATE lines.'}
        >
          <TextArea value={pem} onChange={(e) => setPem(e.target.value)} mono rows={8} placeholder="-----BEGIN CERTIFICATE-----" spellCheck={false} />
        </FormField>
        <Checkbox
          label="Replace an existing certificate for this server"
          checked={replace}
          onChange={(e) => setReplace(e.target.checked)}
        />
      </form>
    </Dialog>
  );
}

function AddKnownHostsDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const formId = useId();
  const { toast } = useToast();
  const create = useCreateCertificates();
  const [text, setText] = useState('');
  const [replace, setReplace] = useState(false);
  const [fingerprints, setFingerprints] = useState<Record<string, string | null>>({});
  const parsed = parseKnownHosts(text);
  const form = useFormErrors([
    ...(parsed.entries.length === 0 && parsed.errors.length === 0 ? [{ id: 'ssh-data', message: 'Paste at least one known_hosts line.' }] : []),
    ...parsed.errors.map((e) => ({ id: 'ssh-data', message: `Line ${e.line}: ${e.message}` })),
  ]);

  useEffect(() => {
    let cancelled = false;
    const keys = [...new Set(parsed.entries.map((e) => e.key))].filter((k) => !(k in fingerprints));
    if (keys.length === 0) return undefined;
    void Promise.all(keys.map(async (k) => [k, await sshFingerprint(k)] as const)).then((pairs) => {
      if (!cancelled) setFingerprints((prev) => ({ ...prev, ...Object.fromEntries(pairs) }));
    });
    return () => {
      cancelled = true;
    };
    // Recompute only when the pasted text changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [text]);

  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    const items = parsed.entries.flatMap((entry) =>
      entry.hosts.map((host) => ({ serverName: host, certType: 'ssh' as const, certSubType: entry.keyType, certData: entry.key })),
    );
    create.mutate(
      { body: { items }, upsert: replace },
      {
        onSuccess: (result) => {
          const count = result.items.length;
          toast({ tone: 'success', title: `${count} known host ${count === 1 ? 'entry' : 'entries'} added` });
          onClose();
        },
      },
    );
  };

  return (
    <Dialog
      open={open}
      onClose={onClose}
      size="lg"
      title="Add SSH known hosts"
      description="Host keys of repository servers reached over SSH. Without a matching entry, SSH connections are refused."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" type="submit" form={formId} loading={create.isPending}>
            Add known hosts
          </Button>
        </>
      }
    >
      <form id={formId} className={styles.form} onSubmit={onSubmit} noValidate aria-label="Add SSH known hosts">
        <ErrorSummary errors={form.errors} focusKey={form.attempt} />
        {create.error ? <ActionError error={create.error} action="add the known hosts" /> : null}
        <FormField
          label="Known hosts data"
          id="ssh-data"
          required
          error={form.errorFor('ssh-data')}
          help={
            <>
              One entry per line in known_hosts format, e.g. the output of <Mono>ssh-keyscan git.example.com</Mono>. Verify the
              fingerprints below with the server administrator before adding them.
            </>
          }
        >
          <TextArea value={text} onChange={(e) => setText(e.target.value)} mono rows={8} placeholder="git.example.com ssh-ed25519 AAAA…" spellCheck={false} />
        </FormField>
        {parsed.entries.length > 0 ? (
          <section aria-label="Entries to add">
            <h3>Entries to add</h3>
            <ul className={styles.preview}>
              {parsed.entries.map((entry) => (
                <li key={`${entry.line}-${entry.text}`}>
                  <Mono>{entry.hosts.join(', ')}</Mono> · {entry.keyType} ·{' '}
                  <Mono>{fingerprints[entry.key] ?? (entry.key in fingerprints ? 'fingerprint unavailable' : 'computing fingerprint…')}</Mono>
                </li>
              ))}
            </ul>
          </section>
        ) : null}
        <Checkbox label="Replace existing entries for the same host and key type" checked={replace} onChange={(e) => setReplace(e.target.checked)} />
      </form>
    </Dialog>
  );
}

function CertificateTable({ kind }: { kind: CertKind }) {
  const { toast } = useToast();
  const certs = useCertificates();
  const remove = useDeleteCertificates();
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState<RepositoryCertificate | undefined>();
  const items = (certs.data?.items ?? []).filter((c) => c.certType === kind);
  const label = kind === 'https' ? 'TLS certificates' : 'SSH known hosts';

  const columns: Column<RepositoryCertificate>[] = [
    { id: 'server', header: 'Server name', cell: (c) => <Mono>{c.serverName}</Mono>, sortValue: (c) => c.serverName, width: '30%' },
    { id: 'subtype', header: kind === 'https' ? 'Type' : 'Key type', width: '170px', cell: (c) => <Badge>{c.certSubType ?? c.certType}</Badge>, sortValue: (c) => c.certSubType },
    {
      id: 'info',
      header: kind === 'https' ? 'Certificate' : 'Fingerprint',
      cell: (c) => (c.certInfo ? <Mono title={c.certInfo}>{c.certInfo}</Mono> : '—'),
      sortValue: (c) => c.certInfo,
    },
    {
      id: 'actions',
      header: 'Actions',
      hideHeader: true,
      align: 'end',
      width: '56px',
      cell: (c) => <IconButton aria-label={`Remove ${c.certSubType ?? c.certType} entry for ${c.serverName}`} icon={<IconClose />} size="sm" onClick={() => setDeleting(c)} />,
    },
  ];

  let content;
  if (certs.isPending) content = <LoadingBlock label={`Loading ${label}`} />;
  else if (certs.isError) content = <ErrorBanner error={certs.error} onRetry={() => void certs.refetch()} />;
  else if (items.length === 0) {
    content = (
      <EmptyState
        headingLevel={3}
        title={kind === 'https' ? 'No TLS certificates' : 'No SSH known hosts'}
        action={
          <Button variant="primary" icon={<IconPlus />} onClick={() => setAdding(true)}>
            {kind === 'https' ? 'Add TLS certificate' : 'Add known hosts'}
          </Button>
        }
      >
        {kind === 'https'
          ? 'Repository servers with publicly trusted certificates need nothing here. Add a certificate for servers using a private CA.'
          : 'SSH repositories are only reachable once their host keys are trusted. Add the keys of each SSH server.'}
      </EmptyState>
    );
  } else {
    content = <Table caption={label} className={styles.mediumTable} columns={columns} rows={items} getRowKey={certKey} defaultSort={{ columnId: 'server', direction: 'ascending' }} />;
  }

  return (
    <div className={styles.stack}>
      <div className={styles.sectionHeader}>
        <p className={styles.muted}>
          {kind === 'https'
            ? 'Certificates trusted for HTTPS connections to repository servers, by server name.'
            : 'Host keys trusted for SSH connections to repository servers.'}
        </p>
        <Button variant="primary" icon={<IconPlus />} onClick={() => setAdding(true)}>
          {kind === 'https' ? 'Add TLS certificate' : 'Add known hosts'}
        </Button>
      </div>
      {content}
      {adding && kind === 'https' ? <AddTLSDialog open onClose={() => setAdding(false)} /> : null}
      {adding && kind === 'ssh' ? <AddKnownHostsDialog open onClose={() => setAdding(false)} /> : null}
      <ConfirmDialog
        open={deleting !== undefined}
        onClose={() => {
          setDeleting(undefined);
          remove.reset();
        }}
        title={kind === 'https' ? 'Remove TLS certificate?' : 'Remove known host entry?'}
        confirmLabel="Remove"
        action="remove the entry"
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          if (!deleting) return;
          remove.mutate(
            { hostNamePattern: deleting.serverName, certType: deleting.certType === 'ssh' ? 'ssh' : 'https', certSubType: deleting.certSubType ?? '' },
            {
              onSuccess: () => {
                toast({ tone: 'success', title: 'Entry removed', description: deleting.serverName });
                setDeleting(undefined);
              },
            },
          );
        }}
      >
        <p>
          Connections to <Mono>{deleting?.serverName}</Mono> will no longer trust this{' '}
          {kind === 'https' ? 'certificate' : `${deleting?.certSubType ?? ''} host key`}; repositories on that server may fail to
          connect.
        </p>
      </ConfirmDialog>
    </div>
  );
}

/** /settings/certs — TLS certificates and SSH known hosts (API-102). */
export function Component() {
  useDocumentTitle('Certificates and known hosts', 'Settings');
  const [tab, setTab] = useTabParam(['tls', 'ssh'] as const, 'tls');
  return (
    <>
      <PageHeader title="Certificates and known hosts" description="Trust anchors used when connecting to repository servers." />
      <Tabs
        label="Certificate type"
        value={tab}
        onValueChange={setTab}
        items={[
          { id: 'tls', label: 'TLS certificates', content: <CertificateTable kind="https" /> },
          { id: 'ssh', label: 'SSH known hosts', content: <CertificateTable kind="ssh" /> },
        ]}
      />
    </>
  );
}

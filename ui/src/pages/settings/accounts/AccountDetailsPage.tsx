import { useId, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router';
import { useCreateAccountToken, useDeleteAccountToken } from '../../../api/hooks/settings';
import { isApiError } from '../../../api/errors';
import { useAccount, useUserInfo } from '../../../api/queries/resources';
import type { AccountToken } from '../../../api/types';
import {
  Badge,
  Banner,
  Button,
  ButtonLink,
  EmptyState,
  FormField,
  IconButton,
  IconClose,
  IconPlus,
  Input,
  KeyValueList,
  Mono,
  PageHeader,
  Table,
  Timestamp,
  useToast,
} from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { formatDuration, parseTokenLifetime } from '../common/duration';
import { ActionError, ConfirmDialog, ErrorSummary, LoadingBlock, useFormErrors, type FieldError } from '../common/feedback';
import { TokenDialog } from '../common/TokenDialog';
import styles from '../common/common.module.css';
import { CAPABILITY_LABELS } from './accountUtil';

const fromUnix = (s: number | undefined) => (s ? new Date(s * 1000) : undefined);

/** /settings/accounts/:name — account details and API tokens (API-106, TEN-112). */
export function Component() {
  const { name = '' } = useParams();
  useDocumentTitle(name, 'Accounts');
  const formId = useId();
  const { toast } = useToast();
  const account = useAccount(name);
  const me = useUserInfo().data;
  const create = useCreateAccountToken(name);
  const remove = useDeleteAccountToken(name);
  const [tokenId, setTokenId] = useState('');
  const [expires, setExpires] = useState('');
  const [issued, setIssued] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<AccountToken | null>(null);
  const lifetime = parseTokenLifetime(expires);
  const tokenErrors: FieldError[] = [];
  if (lifetime === null) tokenErrors.push({ id: `${formId}-expires`, message: 'Use a duration such as 30m, 12h, 7d or 1w; leave empty for no expiry.' });
  if (tokenId.trim() !== '' && !/^[A-Za-z0-9._-]+$/.test(tokenId.trim())) tokenErrors.push({ id: `${formId}-id`, message: 'Token IDs use letters, digits, ".", "-" and "_".' });
  if (tokenId.trim() !== '' && (account.data?.tokens ?? []).some((t) => t.id === tokenId.trim())) {
    tokenErrors.push({ id: `${formId}-id`, message: `A token with ID "${tokenId.trim()}" already exists.` });
  }
  const form = useFormErrors(tokenErrors);

  const header = <PageHeader title={name} description="Local account" />;
  if (account.isPending) {
    return (
      <>
        {header}
        <LoadingBlock label="Loading account" />
      </>
    );
  }
  if (!account.data) {
    const notFound = isApiError(account.error) && account.error.status === 404;
    return (
      <>
        {header}
        {notFound ? (
          <EmptyState title="Account not found" action={<ButtonLink to="/settings/accounts">Back to accounts</ButtonLink>}>
            No local account named <Mono>{name}</Mono> exists.
          </EmptyState>
        ) : (
          <ErrorBanner error={account.error} onRetry={() => void account.refetch()} />
        )}
      </>
    );
  }

  const data = account.data;
  const capabilities = data.capabilities ?? [];
  const canHaveTokens = capabilities.includes('apiKey');
  const isMe = me?.loggedIn === true && me.username === data.name;
  const tokens = data.tokens ?? [];
  const now = Date.now();

  const onGenerate = (event: FormEvent) => {
    event.preventDefault();
    if (!form.check()) return;
    create.mutate(
      { id: tokenId.trim() || undefined, expiresIn: lifetime ?? 0 },
      {
        onSuccess: (result) => {
          setIssued(result.token);
          setTokenId('');
          setExpires('');
          form.reset();
        },
      },
    );
  };

  return (
    <>
      <PageHeader
        title={data.name}
        description="Local account"
        actions={
          isMe && capabilities.includes('login') ? (
            <ButtonLink to="/user-info">Change your password</ButtonLink>
          ) : undefined
        }
      />
      <div className={styles.stack}>
        <KeyValueList
          items={[
            { key: 'status', label: 'Status', value: <Badge tone={data.enabled === false ? 'neutral' : 'success'}>{data.enabled === false ? 'Disabled' : 'Enabled'}</Badge> },
            { key: 'capabilities', label: 'Capabilities', value: capabilities.length ? capabilities.map((c) => CAPABILITY_LABELS[c] ?? c).join(', ') : 'None' },
            { key: 'tokens', label: 'API tokens', value: String(tokens.length) },
          ]}
        />
        {data.enabled === false ? (
          <Banner tone="info" title="This account is disabled">
            It cannot sign in and its tokens are rejected until it is enabled again in <Mono>patrol-cm</Mono>.
          </Banner>
        ) : null}

        <section className={styles.section} aria-labelledby="account-tokens">
          <h2 id="account-tokens">API tokens</h2>
          {tokens.length === 0 ? (
            <p className={styles.muted}>No tokens.</p>
          ) : (
            <Table
              caption="API tokens"
              className={styles.mediumTable}
              rows={tokens}
              getRowKey={(t) => t.id}
              defaultSort={{ columnId: 'issued', direction: 'descending' }}
              columns={[
                { id: 'id', header: 'ID', cell: (t) => <Mono>{t.id}</Mono>, sortValue: (t) => t.id },
                { id: 'issued', header: 'Issued', width: '150px', cell: (t) => <Timestamp value={fromUnix(t.issuedAt)} />, sortValue: (t) => t.issuedAt },
                {
                  id: 'expires',
                  header: 'Expires',
                  width: '170px',
                  cell: (t) =>
                    t.expiresAt ? (
                      t.expiresAt * 1000 < now ? (
                        <span>
                          Expired <Timestamp value={fromUnix(t.expiresAt)} />
                        </span>
                      ) : (
                        <Timestamp value={fromUnix(t.expiresAt)} />
                      )
                    ) : (
                      'Never'
                    ),
                  sortValue: (t) => t.expiresAt ?? Number.MAX_SAFE_INTEGER,
                },
                {
                  id: 'actions',
                  header: 'Actions',
                  hideHeader: true,
                  width: '56px',
                  align: 'end',
                  cell: (t) => <IconButton aria-label={`Delete token ${t.id}`} icon={<IconClose />} size="sm" onClick={() => setDeleting(t)} />,
                },
              ]}
            />
          )}

          {canHaveTokens ? (
            <form id={formId} className={styles.form} onSubmit={onGenerate} noValidate aria-labelledby={`${formId}-title`}>
              <h3 id={`${formId}-title`}>Generate a token</h3>
              <ErrorSummary errors={form.errors} focusKey={form.attempt} />
              {create.error ? <ActionError error={create.error} action="generate the token" /> : null}
              <div className={styles.formGrid}>
                <FormField label="Token ID" id={`${formId}-id`} error={form.errorFor(`${formId}-id`)} help="Optional; generated when empty.">
                  <Input value={tokenId} onChange={(e) => setTokenId(e.target.value)} mono autoComplete="off" />
                </FormField>
                <FormField
                  label="Expires in"
                  id={`${formId}-expires`}
                  error={form.errorFor(`${formId}-expires`)}
                  help={lifetime ? `Expires ${formatDuration(lifetime * 1000)} after creation.` : 'For example 12h, 7d or 1w. Empty: never expires.'}
                >
                  <Input value={expires} onChange={(e) => setExpires(e.target.value)} placeholder="30d" autoComplete="off" />
                </FormField>
              </div>
              <div>
                <Button type="submit" variant="primary" icon={<IconPlus />} loading={create.isPending}>
                  Generate token
                </Button>
              </div>
            </form>
          ) : (
            <Banner tone="info" title="Tokens are not enabled for this account">
              Add the <Mono>apiKey</Mono> capability (<Mono>accounts.{data.name}: apiKey</Mono>) in the <Mono>patrol-cm</Mono> ConfigMap to
              generate API tokens.
            </Banner>
          )}
        </section>
        <p className={styles.small}>
          <Link to="/settings/accounts">All accounts</Link>
        </p>
      </div>
      {issued ? <TokenDialog token={issued} title={`New token for ${data.name}`} onClose={() => setIssued(null)} /> : null}
      <ConfirmDialog
        open={deleting !== null}
        onClose={() => {
          setDeleting(null);
          remove.reset();
        }}
        title="Delete token?"
        confirmLabel="Delete token"
        action="delete the token"
        busy={remove.isPending}
        error={remove.error}
        onConfirm={() => {
          if (!deleting) return;
          remove.mutate(deleting.id, {
            onSuccess: () => {
              toast({ tone: 'success', title: 'Token deleted', description: deleting.id });
              setDeleting(null);
            },
          });
        }}
      >
        <p>
          Clients using <Mono>{deleting?.id}</Mono> are rejected from now on.
        </p>
      </ConfirmDialog>
    </>
  );
}

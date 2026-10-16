import { Link, useNavigate } from 'react-router';
import { useAccounts, useUserInfo } from '../../../api/queries/resources';
import type { Account } from '../../../api/types';
import { Badge, EmptyState, Mono, PageHeader, Table, type Column } from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { ErrorBanner } from '../../../shell/errors';
import { LoadingBlock } from '../common/feedback';
import styles from '../common/common.module.css';
import { accountPath, CAPABILITY_LABELS } from './accountUtil';

/** /settings/accounts — local accounts (API-106, TEN-112). */
export function Component() {
  useDocumentTitle('Accounts', 'Settings');
  const navigate = useNavigate();
  const accounts = useAccounts();
  const me = useUserInfo().data?.username;
  const items = accounts.data?.items ?? [];

  const columns: Column<Account>[] = [
    {
      id: 'name',
      header: 'Name',
      cell: (a) => (
        <>
          <Link to={accountPath(a.name)}>{a.name}</Link>
          {a.name === me ? <span className={styles.small}> (you)</span> : null}
        </>
      ),
      sortValue: (a) => a.name,
    },
    {
      id: 'enabled',
      header: 'Status',
      width: '110px',
      cell: (a) => <Badge tone={a.enabled === false ? 'neutral' : 'success'}>{a.enabled === false ? 'Disabled' : 'Enabled'}</Badge>,
      sortValue: (a) => a.enabled !== false,
    },
    {
      id: 'capabilities',
      header: 'Capabilities',
      width: '200px',
      cell: (a) => ((a.capabilities ?? []).length ? (a.capabilities ?? []).map((c) => CAPABILITY_LABELS[c] ?? c).join(', ') : 'None'),
    },
    { id: 'tokens', header: 'Tokens', width: '90px', align: 'end', cell: (a) => a.tokens?.length ?? 0, sortValue: (a) => a.tokens?.length ?? 0 },
  ];

  let content;
  if (accounts.isPending) content = <LoadingBlock label="Loading accounts" />;
  else if (accounts.isError) content = <ErrorBanner error={accounts.error} onRetry={() => void accounts.refetch()} />;
  else if (items.length === 0) {
    content = (
      <EmptyState title="No local accounts">
        Local accounts are declared in the <Mono>patrol-cm</Mono> ConfigMap as <Mono>accounts.&lt;name&gt;: login, apiKey</Mono>. SSO
        users do not appear here.
      </EmptyState>
    );
  } else {
    content = (
      <Table
        caption="Local accounts"
        className={styles.mediumTable}
        columns={columns}
        rows={items}
        getRowKey={(a) => a.name}
        defaultSort={{ columnId: 'name', direction: 'ascending' }}
        onRowActivate={(a) => void navigate(accountPath(a.name))}
      />
    );
  }

  return (
    <>
      <PageHeader
        title="Accounts"
        description={
          <>
            Local accounts and their API tokens. Accounts are declared in the <Mono>patrol-cm</Mono> ConfigMap. To change your own password,
            open <Link to="/user-info">User info</Link>.
          </>
        }
      />
      <div className={styles.stack}>{content}</div>
    </>
  );
}

import { useState, type FormEvent } from 'react';
import { updatePassword } from '../../api/endpoints/session';
import { useUserInfo } from '../../api/queries/resources';
import { useSession } from '../../app/session';
import { Badge, Banner, Button, ButtonLink, FormField, Input, KeyValueList, Mono, PageHeader, useToast } from '../../design';
import { ErrorBanner } from '../../shell/errors';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import styles from './UserInfo.module.css';

/** Local accounts are issued by Patrol itself; SSO users have the IdP issuer. */
const LOCAL_ISSUER = 'patrol';

function PasswordForm({ username }: { username: string }) {
  const { toast } = useToast();
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [mismatch, setMismatch] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (next !== confirm) {
      setMismatch(true);
      return;
    }
    setMismatch(false);
    setSaving(true);
    setError(null);
    try {
      await updatePassword({ name: username, currentPassword: current, newPassword: next });
      setCurrent('');
      setNext('');
      setConfirm('');
      toast({ tone: 'success', title: 'Your password has been updated.' });
    } catch (err) {
      setError(err);
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className={styles.form} onSubmit={(e) => void onSubmit(e)} aria-labelledby="password-heading">
      <h2 id="password-heading">Change password</h2>
      {error ? <ErrorBanner error={error} title="Unable to update your password" /> : null}
      <FormField label="Current password" required>
        <Input type="password" autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
      </FormField>
      <FormField label="New password" required>
        <Input type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />
      </FormField>
      <FormField label="Confirm new password" required error={mismatch ? 'The new passwords do not match.' : undefined}>
        <Input type="password" autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
      </FormField>
      <div>
        <Button type="submit" variant="primary" loading={saving} disabled={!current || !next || !confirm}>
          Update password
        </Button>
      </div>
    </form>
  );
}

/** /user-info — identity of the current session (API-108, TEN-112). */
export function Component() {
  useDocumentTitle('User info');
  const { anonymous } = useSession();
  const user = useUserInfo();
  const info = user.data;

  return (
    <>
      <PageHeader title="User info" description="Identity and groups for the current session, as used for RBAC." />
      {user.isError ? <ErrorBanner error={user.error} onRetry={() => void user.refetch()} /> : null}
      {anonymous ? (
        <Banner
          tone="info"
          title="You are browsing anonymously"
          actions={
            <ButtonLink size="sm" to="/login">
              Sign in
            </ButtonLink>
          }
        >
          Anonymous access is enabled on this instance; actions are limited to the anonymous role.
        </Banner>
      ) : null}
      {info?.loggedIn ? (
        <div className={styles.layout}>
          <KeyValueList
            items={[
              { key: 'username', label: 'Username', value: <Mono>{info.username}</Mono> },
              { key: 'issuer', label: 'Issuer', value: info.iss ? <Mono>{info.iss}</Mono> : undefined },
              {
                key: 'groups',
                label: 'Groups',
                value:
                  info.groups && info.groups.length > 0 ? (
                    <ul className={styles.groups}>
                      {info.groups.map((g) => (
                        <li key={g}>
                          <Badge>{g}</Badge>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    'None'
                  ),
              },
            ]}
          />
          {info.iss === LOCAL_ISSUER && info.username ? <PasswordForm username={info.username} /> : null}
        </div>
      ) : null}
    </>
  );
}

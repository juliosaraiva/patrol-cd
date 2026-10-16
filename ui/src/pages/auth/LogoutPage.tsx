import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef } from 'react';
import { useNavigate } from 'react-router';
import { deleteSession } from '../../api/endpoints/session';
import { queryKeys } from '../../api/queries/keys';
import { Spinner } from '../../design';
import { browser } from '../../lib/browser';
import { isHttpURL } from '../../lib/url';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import styles from './Auth.module.css';

/**
 * /logout — revokes the session (DELETE /api/v1/session), clears cached data, then follows the
 * OIDC end-session URL when the server returns one, else goes to the login page.
 */
export function Component() {
  useDocumentTitle('Signing out');
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const started = useRef(false);

  useEffect(() => {
    if (started.current) return;
    started.current = true;
    void (async () => {
      let logoutURL: string | undefined;
      try {
        logoutURL = (await deleteSession()).logoutURL;
      } catch {
        // Already signed out or the server is unreachable: clear local state regardless.
      }
      queryClient.removeQueries({ predicate: (q) => q.queryKey[0] !== 'settings' });
      queryClient.setQueryData(queryKeys.session.userInfo, { loggedIn: false });
      if (isHttpURL(logoutURL)) browser.assign(logoutURL);
      else await navigate('/login', { replace: true });
    })();
  }, [navigate, queryClient]);

  return (
    <div className={styles.page}>
      <Spinner size="lg" label="Signing out" showLabel />
    </div>
  );
}

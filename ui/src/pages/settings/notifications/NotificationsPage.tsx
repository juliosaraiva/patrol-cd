import { Mono, PageHeader } from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import { NotificationsCatalog } from '../../notifications/NotificationsCatalog';

/**
 * /settings/notifications — configured services, triggers (with descriptions) and templates
 * (API-032, read-only), rendered by the shared notifications catalog.
 */
export function Component() {
  useDocumentTitle('Notifications', 'Settings');
  return (
    <>
      <PageHeader
        title="Notifications"
        description={
          <>
            What the notifications controller is configured with, from the <Mono>patrol-notifications-cm</Mono> ConfigMap. Applications
            subscribe with <Mono>notifications.cd.patrol.example/subscribe.&lt;trigger&gt;.&lt;service&gt;</Mono> annotations.
          </>
        }
      />
      <NotificationsCatalog />
    </>
  );
}

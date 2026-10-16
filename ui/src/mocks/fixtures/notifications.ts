import type { NotificationServiceList, NotificationTemplateList, NotificationTriggerList } from '../../api/types';

/** Names from the default catalog plus two configured services. */
export const notificationServicesFixture: NotificationServiceList = {
  items: [{ name: 'email' }, { name: 'slack' }, { name: 'webhook.ops' }],
};

export const notificationTriggersFixture: NotificationTriggerList = {
  items: [
    { name: 'on-created' },
    { name: 'on-deleted' },
    { name: 'on-deployed' },
    { name: 'on-health-degraded' },
    { name: 'on-sync-failed' },
    { name: 'on-sync-running' },
    { name: 'on-sync-status-unknown' },
    { name: 'on-sync-succeeded' },
  ],
};

export const notificationTemplatesFixture: NotificationTemplateList = {
  items: [
    { name: 'app-created' },
    { name: 'app-deleted' },
    { name: 'app-deployed' },
    { name: 'app-health-degraded' },
    { name: 'app-sync-failed' },
    { name: 'app-sync-running' },
    { name: 'app-sync-status-unknown' },
    { name: 'app-sync-succeeded' },
  ],
};

import { http, HttpResponse } from 'msw';
import { notificationServicesFixture, notificationTemplatesFixture, notificationTriggersFixture } from '../fixtures/notifications';

export const notificationHandlers = [
  http.get('*/api/v1/notifications/services', () => HttpResponse.json(notificationServicesFixture)),
  http.get('*/api/v1/notifications/triggers', () => HttpResponse.json(notificationTriggersFixture)),
  http.get('*/api/v1/notifications/templates', () => HttpResponse.json(notificationTemplatesFixture)),
];

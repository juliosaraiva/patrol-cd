import { applicationSetHandlers } from './applicationsets';
import { appsCoreHandlers } from './apps-core';
import { appsResourcesHandlers } from './apps-resources';
import { createHandlers } from './create';
import { notificationHandlers } from './notifications';
import { resourceHandlers } from './resources';
import { sessionHandlers } from './session';
import { settingsHandlers } from './settings';

// Per-area handlers come before resourceHandlers, whose read-only list stubs are fallbacks.
export const handlers = [...sessionHandlers, ...settingsHandlers, ...applicationSetHandlers, ...notificationHandlers, ...appsCoreHandlers, ...appsResourcesHandlers, ...createHandlers, ...resourceHandlers];
export { mockState, problem, resetMockState } from './util';

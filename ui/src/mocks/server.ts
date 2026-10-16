import { setupServer } from 'msw/node';
import { handlers } from './handlers';

/** MSW server for vitest (see src/test/setup.ts). */
export const server = setupServer(...handlers);

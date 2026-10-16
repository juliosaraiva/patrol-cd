import { QueryClient } from '@tanstack/react-query';
import { isApiError } from './errors';

/** Errors that will not change on retry. */
function isPermanent(error: unknown): boolean {
  return isApiError(error) && error.status >= 400 && error.status < 500 && error.status !== 408 && error.status !== 429;
}

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 10_000,
        refetchOnWindowFocus: true,
        retry: (failureCount, error) => !isPermanent(error) && failureCount < 2,
      },
      mutations: { retry: false },
    },
  });
}

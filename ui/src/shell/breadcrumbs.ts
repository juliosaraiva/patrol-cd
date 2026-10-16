import { useMatches, type Params, type UIMatch } from 'react-router';
import type { Crumb } from '../design/components/Navigation';

/**
 * Route `handle` contract. `crumb` returns the breadcrumb(s) a route contributes; the last crumb
 * is the current page.
 */
export interface RouteHandle {
  crumb?: (params: Params) => Crumb | Crumb[];
}

export function useBreadcrumbs(): Crumb[] {
  const matches = useMatches() as UIMatch<unknown, RouteHandle | undefined>[];
  const crumbs: Crumb[] = [];
  for (const match of matches) {
    const produced = match.handle?.crumb?.(match.params);
    if (!produced) continue;
    for (const crumb of Array.isArray(produced) ? produced : [produced]) {
      crumbs.push({ label: crumb.label, to: crumb.to ?? match.pathname });
    }
  }
  return crumbs;
}

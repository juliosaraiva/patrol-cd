import { http, HttpResponse } from 'msw';
import type { Application, ApplicationSet, ApplicationSetGenerateRequest, ApplicationSetInput } from '../../api/types';
import {
  applicationSetsFixture,
  guestbookAppSetEvents,
  guestbookAppSetTree,
} from '../fixtures/applicationsets';
import { createClustersFixture, repoAppsFixture } from '../fixtures/create';
import { emptyObject, problem } from './util';

/** Pull requests the mock pull-request generator "finds". */
export const mockPullRequests = [
  { number: '42', title: 'Add cart', branch: 'feature/cart', target_branch: 'main', head_sha: '9f8e7d6c5b4a39281706f5e4d3c2b1a098765432', author: 'dev-a' },
  { number: '57', title: 'Faster search', branch: 'feature/search', target_branch: 'main', head_sha: '0123456789abcdef0123456789abcdef01234567', author: 'dev-b' },
];

type Params = Record<string, string>;
type Generator = ApplicationSetInput['spec']['generators'][number];

function generatorParams(gen: Generator): Params[] {
  if (gen.list) return (gen.list.elements ?? []).map((e) => Object.fromEntries(Object.entries(e as Record<string, unknown>).map(([k, v]) => [k, String(v)])));
  if (gen.pullRequest) return mockPullRequests.map((pr) => ({ ...pr, branch_slug: pr.branch.replace(/[^a-z0-9]+/gi, '-'), head_short_sha: pr.head_sha.slice(0, 8) }));
  if (gen.clusters) return createClustersFixture.items.map((c) => ({ name: c.spec.name ?? '', server: c.spec.server }));
  if (gen.git) {
    return (repoAppsFixture.items ?? []).map((a) => ({ path: a.path, 'path.basename': a.path.split('/').pop() ?? a.path }));
  }
  const nested = (gen.matrix?.generators ?? gen.merge?.generators ?? []) as Generator[];
  if (gen.matrix && nested.length >= 2) {
    const [a = [], b = []] = nested.map(generatorParams);
    return a.flatMap((pa) => b.map((pb) => ({ ...pa, ...pb })));
  }
  return nested[0] ? generatorParams(nested[0]) : [];
}

function substitute(value: unknown, params: Params): unknown {
  if (typeof value === 'string') {
    return value.replace(/\{\{\s*\.?([\w.]+)\s*\}\}/g, (match, key: string) => params[key] ?? match);
  }
  if (Array.isArray(value)) return value.map((v) => substitute(v, params));
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, substitute(v, params)]));
  return value;
}

/** A small, deterministic stand-in for the server's dry-run rendering. */
export function mockGenerate(set: ApplicationSetInput): Application[] {
  const params = set.spec.generators.flatMap(generatorParams);
  return params.map((p) => {
    const tpl = substitute(set.spec.template, p) as ApplicationSetInput['spec']['template'];
    return {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'Application',
      metadata: { ...tpl.metadata, namespace: set.metadata?.namespace ?? 'patrol' },
      spec: tpl.spec,
    };
  });
}

function findSet(name: string, namespace: string | null): ApplicationSet | undefined {
  return applicationSetsFixture.items.find((s) => s.metadata.name === name && (!namespace || s.metadata.namespace === namespace));
}

export const applicationSetHandlers = [
  http.get('*/api/v1/applicationsets', () => HttpResponse.json(applicationSetsFixture)),
  // Create (not persisted by the mock): 409 for an existing name unless upsert; dryRun renders
  // the generated Applications into status.resources.
  http.post('*/api/v1/applicationsets', async ({ request }) => {
    const url = new URL(request.url);
    const set = (await request.json()) as ApplicationSetInput;
    const name = set.metadata?.name ?? '';
    const namespace = set.metadata?.namespace ?? 'patrol';
    if (findSet(name, namespace) && url.searchParams.get('upsert') !== 'true') {
      return problem(409, 'already_exists', `existing ApplicationSet spec is different; use upsert flag to force update`);
    }
    const created: ApplicationSet = {
      apiVersion: 'cd.patrol.example/v1alpha1',
      kind: 'ApplicationSet',
      ...set,
      metadata: { ...set.metadata, name, namespace, uid: `uid-appset-${name}`, resourceVersion: '1' },
    };
    if (url.searchParams.get('dryRun') === 'true') {
      created.status = {
        resources: mockGenerate(set).map((a) => ({ group: 'cd.patrol.example', version: 'v1alpha1', kind: 'Application', name: a.metadata.name ?? '', namespace })),
      };
    }
    return HttpResponse.json(created);
  }),
  http.post('*/api/v1/applicationsets/generate', async ({ request }) => {
    const body = (await request.json()) as ApplicationSetGenerateRequest;
    const set = body.applicationSet;
    if (!set.spec.template.metadata.name) {
      return problem(400, 'invalid_argument', 'application set template is invalid: application name is empty');
    }
    if (set.spec.template.spec.project === 'restricted') {
      return problem(403, 'permission_denied', 'permission denied: applicationsets, create, restricted/' + (set.metadata?.name ?? ''));
    }
    return HttpResponse.json({ applications: mockGenerate(set) });
  }),
  // Missing and forbidden ApplicationSets are both 403 (as the server).
  http.get('*/api/v1/applicationsets/:name', ({ params, request }) => {
    const set = findSet(String(params.name), new URL(request.url).searchParams.get('appsetNamespace'));
    return set ? HttpResponse.json(set) : problem(403, 'permission_denied', `permission denied: applicationsets, get, ${String(params.name)}`);
  }),
  http.delete('*/api/v1/applicationsets/:name', ({ params, request }) => {
    const set = findSet(String(params.name), new URL(request.url).searchParams.get('appsetNamespace'));
    return set ? HttpResponse.json(emptyObject) : problem(403, 'permission_denied', `permission denied: applicationsets, delete, ${String(params.name)}`);
  }),
  http.get('*/api/v1/applicationsets/:name/resource-tree', ({ params }) =>
    HttpResponse.json(params.name === 'guestbook' ? guestbookAppSetTree : { nodes: [] }),
  ),
  http.get('*/api/v1/applicationsets/:name/events', ({ params }) =>
    HttpResponse.json(params.name === 'guestbook' ? guestbookAppSetEvents : { items: [] }),
  ),
];

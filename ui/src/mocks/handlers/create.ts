import { http, HttpResponse } from 'msw';
import type { ApplicationInput, RepositoryAppDetailsQuery } from '../../api/types';
import {
  appDetailsFixture,
  createdApplication,
  helmChartsFixture,
  ociTagsFixture,
  repoAppsFixture,
  repoRefsFixture,
} from '../fixtures/create';
import { problem } from './util';

/**
 * Repository discovery and application creation. Mock rules for POST /applications:
 * name "exists" → 409 already_exists unless ?upsert=true; ?validate=true with a repoURL
 * containing "unknown" → 400 invalid_argument (server-side spec validation).
 */
export const createHandlers = [
  http.get('*/api/v1/repositories/:repo/refs', () => HttpResponse.json(repoRefsFixture)),
  http.get('*/api/v1/repositories/:repo/oci-tags', () => HttpResponse.json(ociTagsFixture)),
  http.get('*/api/v1/repositories/:repo/helmcharts', () => HttpResponse.json(helmChartsFixture)),
  http.get('*/api/v1/repositories/:repo/apps', () => HttpResponse.json(repoAppsFixture)),
  http.post('*/api/v1/repositories/:repo/appdetails', async ({ request }) => {
    const body = (await request.json()) as RepositoryAppDetailsQuery;
    return HttpResponse.json(appDetailsFixture(body.source.path, body.source.chart));
  }),
  http.post('*/api/v1/applications', async ({ request }) => {
    const url = new URL(request.url);
    const app = (await request.json()) as ApplicationInput;
    if (app.metadata?.name === 'exists' && url.searchParams.get('upsert') !== 'true') {
      return problem(409, 'already_exists', `existing application spec is different, use upsert flag to force update`);
    }
    const repoURL = app.spec.source?.repoURL ?? app.spec.sources?.[0]?.repoURL ?? '';
    if (url.searchParams.get('validate') === 'true' && repoURL.includes('unknown')) {
      return problem(
        400,
        'invalid_argument',
        `application spec for ${app.metadata?.name ?? ''} is invalid: InvalidSpecError: repository not accessible: repository not found`,
      );
    }
    return HttpResponse.json(createdApplication(app));
  }),
  // Extension bundle: nothing installed in mock mode (the server answers an empty script).
  http.get('*/extensions.js', () => new HttpResponse('', { headers: { 'Content-Type': 'text/javascript; charset=utf-8' } })),
];

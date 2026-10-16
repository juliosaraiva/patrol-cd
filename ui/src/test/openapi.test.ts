// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { findOperation, loadSpec, matchPath, operationFor, parseEventStream, responseSchema, validateSchema, type Schema } from './openapi';

const schema = (name: string): Schema => loadSpec().components.schemas[name]!;

describe('OpenAPI test helpers', () => {
  it('reports missing required properties, wrong types, enums and undeclared properties (strict)', () => {
    expect(validateSchema(schema('RepositoryInfo'), { metadata: {}, spec: { url: 'https://x' }, connectionState: { status: 'Unknown' } })).toEqual(['/: missing required property "hasCredentials"']);
    expect(validateSchema(schema('LogEntry'), { content: 1, podName: 'p', last: false })).toEqual(['/content: expected string, got integer']);
    expect(validateSchema(schema('Problem'), { title: 'Bad Request', status: 400, code: 'nope' })[0]).toMatch(/is not one of/);
    expect(validateSchema(schema('CanIResponse'), { value: 'yes', extra: true })).toEqual([]);
    expect(validateSchema(schema('CanIResponse'), { value: 'yes', extra: true }, { strict: true })).toEqual(['/extra: property not declared by the schema']);
  });

  it('follows references into CRD schemas, anyOf branches, formats and open maps', () => {
    const app = {
      metadata: { name: 'a', labels: { team: 'web' } },
      spec: {
        project: 'default',
        destination: { server: 'https://kubernetes.default.svc' },
        source: { repoURL: 'https://x', kustomize: { replicas: [{ name: 'web', count: '3' }] }, helm: { valuesObject: { any: { nested: true } } } },
      },
      status: { reconciledAt: 'not a time' },
    };
    expect(validateSchema(schema('Application'), app, { strict: true })).toEqual(['/status/reconciledAt: not a date-time']);
    // Response-side schema: allOf the CRD schema, plus metadata required.
    expect(validateSchema(schema('StoredApplication'), app, { strict: true })).toEqual(['/status/reconciledAt: not a date-time']);
    expect(validateSchema(schema('StoredApplication'), { spec: app.spec }, { strict: true })).toEqual(['/: missing required property "metadata"']);
    const badCount = structuredClone(app);
    badCount.status.reconciledAt = '2026-10-16T09:00:00Z';
    (badCount.spec.source.kustomize.replicas[0] as { count: unknown }).count = true;
    expect(validateSchema(schema('Application'), badCount)).toEqual(['/spec/source/kustomize/replicas/0/count: matches no anyOf branch']);
  });

  it('matches request paths, including multi-segment parameters and literal segments first', () => {
    expect(matchPath(findOperation('canI'), '/api/v1/account/can-i/applications/sync/default/guestbook')).toEqual({
      resource: 'applications',
      action: 'sync',
      subresource: 'default/guestbook',
    });
    expect(matchPath(findOperation('getRepository'), '/api/v1/repositories/https%3A%2F%2Fgit.example.test%2Fa.git')).toEqual({ repo: 'https://git.example.test/a.git' });
    expect(matchPath(findOperation('getApplication'), '/api/v1/applications/a/b')).toBeNull();
    expect(operationFor('POST', '/api/v1/applicationsets/generate')?.operationId).toBe('generateApplicationSet');
    expect(responseSchema(findOperation('getApplication'), 404, 'application/problem+json')).toEqual({ $ref: '#/components/schemas/Problem' });
  });

  it('parses event streams', () => {
    expect(parseEventStream(': heartbeat\n\nid: 7\ndata: {"a":1}\n\nevent: error\ndata: {"status":401}\n\n')).toEqual([
      { event: 'message', data: '{"a":1}', id: '7' },
      { event: 'error', data: '{"status":401}', id: undefined },
    ]);
  });
});

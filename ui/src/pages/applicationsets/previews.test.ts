import { describe, expect, it } from 'vitest';
import { generatedApplications, previewsAppSet } from '../../mocks/fixtures/applicationsets';
import { previewCleanupPolicy, previewEnvironment, pullRequestSource, pullRequestSources } from './previews';

describe('pull request sources', () => {
  it.each([
    [{ github: { owner: 'acme', repo: 'shop' } }, 'https://github.com/acme/shop/pull/7'],
    [{ github: { owner: 'acme', repo: 'shop', api: 'https://ghe.example.test/api/v3/' } }, 'https://ghe.example.test/acme/shop/pull/7'],
    [{ gitlab: { project: 'group/sub/shop' } }, 'https://gitlab.com/group/sub/shop/-/merge_requests/7'],
    [{ gitlab: { project: '1234' } }, null],
    [{ gitea: { owner: 'acme', repo: 'shop', api: 'https://gitea.example.test/' } }, 'https://gitea.example.test/acme/shop/pulls/7'],
    [{ bitbucket: { owner: 'acme', repo: 'shop' } }, 'https://bitbucket.org/acme/shop/pull-requests/7'],
    [{ bitbucketServer: { project: 'PRJ', repo: 'shop', api: 'https://bb.example.test/rest' } }, 'https://bb.example.test/projects/PRJ/repos/shop/pull-requests/7'],
    [{ azuredevops: { organization: 'org', project: 'proj', repo: 'shop' } }, 'https://dev.azure.com/org/proj/_git/shop/pullrequest/7'],
    [{ github: { owner: 'acme', repo: 'shop', api: 'javascript:alert(1)' } }, 'https://github.com/acme/shop/pull/7'],
  ])('%j links pull request 7 to %s', (gen, url) => {
    expect(pullRequestSource(gen as never).urlFor('7')).toBe(url);
  });

  it('collects pull request generators of the set with defaults', () => {
    const [src] = pullRequestSources(previewsAppSet);
    expect(src).toMatchObject({ provider: 'GitHub', repository: 'acme/shop', labels: ['preview'], filters: ['branch ~ ^feature/.*'], requeueAfterSeconds: 300 });
  });
});

describe('previewEnvironment', () => {
  const [src] = pullRequestSources(previewsAppSet);

  it('reads number, branch and head SHA from labels and annotations', () => {
    const app = generatedApplications.find((a) => a.metadata.name === 'shop-pr-42')!;
    expect(previewEnvironment(app, src)).toEqual({
      number: '42',
      branch: 'feature/cart',
      headSha: '9f8e7d6c5b4a39281706f5e4d3c2b1a098765432',
      url: 'https://github.com/acme/shop/pull/42',
    });
  });

  it('falls back to the name and the source revision', () => {
    expect(previewEnvironment({ metadata: { name: 'web-pr-9-preview' }, spec: { source: { targetRevision: 'feature/x' } } }, src)).toEqual({
      number: '9',
      branch: 'feature/x',
      headSha: undefined,
      url: 'https://github.com/acme/shop/pull/9',
    });
    expect(previewEnvironment({ metadata: { name: 'web' }, spec: { source: { targetRevision: 'abcdef1' } } }, undefined)).toEqual({
      number: undefined,
      branch: undefined,
      headSha: 'abcdef1',
      url: null,
    });
  });
});

describe('previewCleanupPolicy', () => {
  it('describes deletion for sync policies and the lack of it for create-only', () => {
    expect(previewCleanupPolicy(previewsAppSet)).toMatch(/^Deleted when the pull request closes .*applicationsSync: sync/);
    const createOnly = { ...previewsAppSet, spec: { ...previewsAppSet.spec, syncPolicy: { applicationsSync: 'create-only' as const } } };
    expect(previewCleanupPolicy(createOnly)).toMatch(/^Not cleaned up automatically/);
  });
});

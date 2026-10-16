import { describe, expect, it } from 'vitest';
import type { Application, ApplicationSource } from '../../api/types';
import { createRepositoriesFixture, GIT_REPO, HELM_REPO, OCI_REPO } from '../../mocks/fixtures/create';
import {
  applyKind,
  cleanApplication,
  createdApplicationPath,
  defaultApplication,
  parseApplicationYAML,
  setMultiSource,
  setSyncOption,
  sourceKind,
  syncOptionValue,
  toYAML,
  validateApplication,
} from './model';

const repos = createRepositoriesFixture.items;

function valid(): Application {
  const app = defaultApplication({ repoURL: GIT_REPO });
  app.metadata.name = 'guestbook';
  app.spec.source = { repoURL: GIT_REPO, path: 'guestbook', targetRevision: 'HEAD' };
  app.spec.destination = { server: 'https://kubernetes.default.svc', namespace: 'guestbook' };
  return app;
}

describe('sources', () => {
  it('switches between single and multiple sources keeping the first one', () => {
    const multi = setMultiSource(valid().spec, true);
    expect(multi.source).toBeUndefined();
    expect(multi.sources).toEqual([{ repoURL: GIT_REPO, path: 'guestbook', targetRevision: 'HEAD' }]);
    const back = setMultiSource({ ...multi, sources: [{ repoURL: GIT_REPO, path: 'a', ref: 'values' }] }, false);
    expect(back.sources).toBeUndefined();
    expect(back.source).toEqual({ repoURL: GIT_REPO, path: 'a' });
  });

  it('detects the repository type from registrations and URLs', () => {
    expect(sourceKind({ repoURL: HELM_REPO }, repos)).toBe('helm');
    expect(sourceKind({ repoURL: OCI_REPO })).toBe('oci');
    expect(sourceKind({ repoURL: 'https://x.example.test/r.git' })).toBe('git');
    expect(sourceKind({ repoURL: 'https://charts.example.test', chart: 'x' })).toBe('helm');
  });

  it('reshapes a source for its repository type', () => {
    expect(applyKind({ repoURL: HELM_REPO, path: 'x', targetRevision: 'HEAD' }, 'helm')).toEqual({ repoURL: HELM_REPO, chart: '', targetRevision: '' });
    expect(applyKind({ repoURL: OCI_REPO, chart: 'x' }, 'oci')).toEqual({ repoURL: OCI_REPO, path: '.' });
  });
});

describe('sync options', () => {
  it('sets, replaces and removes key=value options', () => {
    let opts = setSyncOption(undefined, 'CreateNamespace', 'true');
    opts = setSyncOption(opts, 'PrunePropagationPolicy', 'background');
    opts = setSyncOption(opts, 'PrunePropagationPolicy', 'orphan');
    expect(opts).toEqual(['CreateNamespace=true', 'PrunePropagationPolicy=orphan']);
    expect(syncOptionValue(opts, 'PrunePropagationPolicy')).toBe('orphan');
    expect(setSyncOption(opts, 'CreateNamespace', undefined)).toEqual(['PrunePropagationPolicy=orphan']);
  });
});

describe('YAML', () => {
  it('round-trips the cleaned application', () => {
    const app = valid();
    const parsed = parseApplicationYAML(toYAML(app));
    expect('app' in parsed && parsed.app).toEqual(cleanApplication(app));
  });

  it('reports syntax errors with a line number and structural errors plainly', () => {
    expect(parseApplicationYAML('metadata:\n  name: [x\n')).toEqual({ error: expect.stringMatching(/^Line \d+: /) as string });
    expect(parseApplicationYAML('- a')).toEqual({ error: 'The document must be a YAML mapping describing an Application.' });
    expect(parseApplicationYAML('metadata: {name: a}\nspec: {}')).toEqual({ error: 'spec.destination is required.' });
    expect(parseApplicationYAML('kind: Deployment\nmetadata: {}\nspec: {destination: {}}')).toEqual({ error: 'kind must be Application, not Deployment.' });
  });

  // Documents that parse but are half-edited or mistyped (reviewer finding: these crashed the
  // wizard during render). Every case must yield a readable draft or a line-specific error.
  const doc = (spec: string) => `metadata:\n  name: app\nspec:\n  project: default\n  destination:\n    server: https://kubernetes.default.svc\n${spec}`;

  it('fills in required fields that a partial edit removed, so validation names them', () => {
    const noRepo = parseApplicationYAML(doc('  source:\n    path: guestbook\n'));
    expect('app' in noRepo && noRepo.app.spec.source).toEqual({ repoURL: '', path: 'guestbook' });
    if ('app' in noRepo) expect(validateApplication(noRepo.app)).toEqual({ 'sources.0.repoURL': 'Enter a repository URL.' });

    const hydrator = parseApplicationYAML(doc('  sourceHydrator: {}\n'));
    expect('app' in hydrator && hydrator.app.spec.sourceHydrator).toEqual({
      drySource: { repoURL: '', targetRevision: '', path: '' },
      syncSource: { targetBranch: '', path: '' },
    });
    if ('app' in hydrator)
      expect(Object.keys(validateApplication(hydrator.app))).toEqual(['hydrator.repoURL', 'hydrator.syncBranch', 'hydrator.syncPath']);

    // Empty values ("path:" while typing) read as absent.
    const nulls = parseApplicationYAML(doc('  source:\n    repoURL:\n    path:\n  syncPolicy:\n    automated:\n'));
    expect('app' in nulls && nulls.app.spec).toMatchObject({ source: { repoURL: '' }, syncPolicy: {} });
  });

  it.each([
    ['sources as a string', '  sources: abc\n', 'Line 7: spec.sources must be a list.'],
    ['a source that is not a mapping', '  sources:\n    - abc\n', 'Line 8: spec.sources[0] must be a mapping (key: value pairs).'],
    ['syncOptions as a string', '  syncPolicy:\n    syncOptions: CreateNamespace=true\n', 'Line 8: spec.syncPolicy.syncOptions must be a list.'],
    ['a numeric revision', '  source:\n    repoURL: https://x.example.test/r.git\n    path: .\n    targetRevision: 1.10\n', 'Line 10: spec.source.targetRevision must be a string; put the value in quotes, e.g. "1.10".'],
    ['a numeric Helm parameter value', '  source:\n    repoURL: r\n    chart: c\n    helm:\n      parameters:\n        - name: replicas\n          value: 3\n', 'Line 13: spec.source.helm.parameters[0].value must be a string; put the value in quotes, e.g. "3".'],
    ['a dry source that is not a mapping', '  sourceHydrator:\n    drySource: main\n', 'Line 8: spec.sourceHydrator.drySource must be a mapping (key: value pairs).'],
    ['automated as a boolean', '  syncPolicy:\n    automated: true\n', 'Line 8: spec.syncPolicy.automated must be a mapping (key: value pairs).'],
    ['a retry limit in quotes', '  syncPolicy:\n    retry:\n      limit: "5"\n', 'Line 9: spec.syncPolicy.retry.limit must be a number.'],
  ])('reports %s with its line', (_, spec, error) => {
    expect(parseApplicationYAML(doc(spec))).toEqual({ error });
  });

  it('reports mistyped metadata with its line', () => {
    expect(parseApplicationYAML('metadata:\n  name: app\n  finalizers: x\nspec:\n  destination: {}\n')).toEqual({ error: 'Line 3: metadata.finalizers must be a list.' });
    expect(parseApplicationYAML('metadata:\n  name: 123\nspec:\n  destination: {}\n')).toEqual({
      error: 'Line 2: metadata.name must be a string; put the value in quotes, e.g. "123".',
    });
    expect(parseApplicationYAML('metadata: app\nspec:\n  destination: {}\n')).toEqual({ error: 'Line 1: metadata must be a mapping (key: value pairs).' });
  });

  it('keeps fields the form does not edit as written', () => {
    const parsed = parseApplicationYAML(doc('  source:\n    repoURL: r\n    path: p\n    helm:\n      valuesObject: {a: 1}\n  ignoreDifferences:\n    - kind: Deployment\n'));
    expect('app' in parsed && parsed.app.spec).toMatchObject({ source: { helm: { valuesObject: { a: 1 } } }, ignoreDifferences: [{ kind: 'Deployment' }] });
  });

  it('drops empty values before sending', () => {
    const app = valid();
    app.spec.syncPolicy = { syncOptions: [], automated: {} };
    app.spec.source = { repoURL: GIT_REPO, path: 'p', targetRevision: '', helm: { releaseName: '', parameters: [{ name: '', value: '' }] } };
    expect(cleanApplication(app).spec).toEqual({
      project: 'default',
      source: { repoURL: GIT_REPO, path: 'p' },
      destination: { server: 'https://kubernetes.default.svc', namespace: 'guestbook' },
      syncPolicy: { automated: {} },
    });
  });
});

describe('validateApplication', () => {
  it('accepts a complete application', () => {
    expect(validateApplication(valid(), { repos })).toEqual({});
  });

  it('names every missing or invalid field', () => {
    const app = defaultApplication();
    app.metadata.name = 'Bad_Name';
    app.metadata.namespace = 'team';
    expect(validateApplication(app, { repos })).toEqual({
      name: 'Use lowercase letters, digits, "-" and "."; start and end with a letter or digit.',
      appNamespace: 'Applications in other namespaces are disabled on this instance.',
      'sources.0.repoURL': 'Enter a repository URL.',
      'sources.0.path': 'Enter a path in the repository ("." for its root).',
      'destination.target': 'Choose a destination cluster.',
    });
  });

  it('requires chart and version for Helm sources', () => {
    const app = valid();
    app.spec.source = { repoURL: HELM_REPO, chart: '', targetRevision: '' };
    expect(validateApplication(app, { repos })).toEqual({
      'sources.0.chart': 'Choose a chart.',
      'sources.0.targetRevision': 'Enter a chart version (or a semver range such as 1.*).',
    });
  });

  it('accepts values-only ref sources and rejects duplicate refs and escaping paths', () => {
    const app = valid();
    app.spec = setMultiSource(app.spec, true);
    app.spec.sources = [
      { repoURL: HELM_REPO, chart: 'nginx', targetRevision: '15.4.0' },
      { repoURL: GIT_REPO, ref: 'values' },
      { repoURL: GIT_REPO, ref: 'values', path: '../x' },
    ];
    expect(validateApplication(app, { repos })).toEqual({
      'sources.2.ref': 'Ref "values" is already used by source 2.',
      'sources.2.path': 'Use a relative path inside the repository, without "..".',
    });
  });

  it('checks the hydrator and destination exclusivity', () => {
    const app = valid();
    delete app.spec.source;
    app.spec.sourceHydrator = { drySource: { repoURL: '', targetRevision: 'HEAD', path: '.' }, syncSource: { targetBranch: '', path: '' } };
    app.spec.destination = { server: 'https://a', name: 'b' };
    expect(validateApplication(app)).toEqual({
      'hydrator.repoURL': 'Enter the dry source repository URL.',
      'hydrator.syncBranch': 'Enter the branch the hydrated manifests are written to.',
      'hydrator.syncPath': 'Enter the path the hydrated manifests are written to.',
      'destination.target': 'Set the cluster URL or the cluster name, not both.',
    });
  });
});

describe('malformed drafts', () => {
  it('never throws on shapes that bypass the YAML parser', () => {
    const shapes: unknown[] = [
      { metadata: {}, spec: { sources: 'abc', destination: {} } },
      { metadata: { name: 7 }, spec: { source: {}, destination: {} } },
      { metadata: {}, spec: { sourceHydrator: {}, destination: {} } },
      { metadata: {}, spec: { sources: ['x', null, { repoURL: 5, path: ['a'] }], destination: { server: 1 } } },
    ];
    for (const shape of shapes) expect(() => validateApplication(shape as Application, { repos })).not.toThrow();
    expect(validateApplication(shapes[0] as Application)).toMatchObject({ sources: 'Add at least one source.' });
    expect(syncOptionValue('CreateNamespace=true' as unknown as string[], 'CreateNamespace')).toBeUndefined();
    expect(setSyncOption('x' as unknown as string[], 'CreateNamespace', 'true')).toEqual(['CreateNamespace=true']);
    expect(sourceKind({} as ApplicationSource)).toBe('git');
  });
});

describe('createdApplicationPath', () => {
  it('uses the namespaced route outside the control plane', () => {
    const app = valid();
    expect(createdApplicationPath({ ...app, metadata: { name: 'a', namespace: 'patrol' } }, 'patrol')).toBe('/applications/a');
    expect(createdApplicationPath({ ...app, metadata: { name: 'a', namespace: 'team' } }, 'patrol')).toBe('/applications/team/a');
  });
});

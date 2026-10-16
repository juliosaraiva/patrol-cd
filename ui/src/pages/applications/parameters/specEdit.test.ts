import { describe, expect, it } from 'vitest';
import { guestbookApp, paymentsApp, searchApp } from '../../../mocks/fixtures/apps-core';
import {
  applySources,
  applySummary,
  hasErrors,
  helmParamRows,
  manifestYAML,
  newSourceDraft,
  parseAnnouncement,
  parseManifest,
  setHelmParam,
  setPluginMap,
  setPluginParam,
  setTool,
  sourceDrafts,
  summaryFormFrom,
  toolOf,
  validateKey,
  validateSources,
  validateSummary,
} from './specEdit';

describe('summary form', () => {
  it('round-trips an application without changes', () => {
    const app = guestbookApp();
    const next = applySummary(app, summaryFormFrom(app));
    expect(next.spec).toEqual(app.spec);
    expect(next.metadata).toEqual(app.metadata);
  });

  it('applies labels, destination, sync policy, retry, info and history limit', () => {
    const app = searchApp();
    const form = summaryFormFrom(app);
    const next = applySummary(app, {
      ...form,
      labels: [{ key: 'tier', value: 'backend' }],
      destinationMode: 'name',
      destinationName: 'prod-eu',
      destinationNamespace: 'search',
      automated: true,
      selfHeal: true,
      syncOptions: ['ServerSideApply=true'],
      retry: { ...form.retry, enabled: false },
      info: [{ name: 'Owner', value: 'search-team' }],
      revisionHistoryLimit: '5',
    });
    expect(next.metadata.labels).toEqual({ tier: 'backend' });
    expect(next.spec.destination).toEqual({ name: 'prod-eu', namespace: 'search' });
    expect(next.spec.syncPolicy).toEqual({ automated: { selfHeal: true }, syncOptions: ['ServerSideApply=true'] });
    expect(next.spec.info).toEqual([{ name: 'Owner', value: 'search-team' }]);
    expect(next.spec.revisionHistoryLimit).toBe(5);
    expect(next.metadata.resourceVersion).toBe(app.metadata.resourceVersion);
  });

  it('disables automated sync without losing its settings', () => {
    const app = guestbookApp();
    const next = applySummary(app, { ...summaryFormFrom(app), automated: false });
    expect(next.spec.syncPolicy?.automated).toEqual({ enabled: false, prune: true, selfHeal: true });
  });

  it('validates required fields, keys and values', () => {
    const form = summaryFormFrom(guestbookApp());
    const errors = validateSummary({
      ...form,
      project: '',
      destinationServer: '',
      revisionHistoryLimit: '-1',
      labels: [
        { key: 'ok', value: 'fine' },
        { key: 'ok', value: 'dup' },
        { key: 'bad key', value: '' },
        { key: 'v', value: 'not valid!' },
      ],
      info: [{ name: 'a', value: '' }],
    });
    expect(errors.project).toBeDefined();
    expect(errors.destination).toBeDefined();
    expect(errors.revisionHistoryLimit).toBeDefined();
    expect(errors.info).toBeDefined();
    expect(Object.keys(errors.labels ?? {})).toEqual(['1', '2', '3']);
    expect(hasErrors(errors)).toBe(true);
    expect(hasErrors(validateSummary(form))).toBe(false);
    expect(validateKey('example.com/name')).toBeUndefined();
    expect(validateKey('Bad_Prefix/name')).toBeDefined();
  });
});

describe('sources', () => {
  it('switches tools and drops the other tool settings', () => {
    const source = searchApp().spec.source ?? { repoURL: '' };
    expect(toolOf(source)).toBe('kustomize');
    const helm = setTool(source, 'helm');
    expect(helm.kustomize).toBeUndefined();
    expect(helm.helm).toEqual({});
    expect(setTool(helm, 'auto').helm).toBeUndefined();
  });

  it('validates repository, location, refs and YAML fields', () => {
    const drafts = sourceDrafts(paymentsApp());
    expect(validateSources(drafts, true).every((e) => Object.keys(e).length === 0)).toBe(true);
    const broken = [
      { ...drafts[0], source: { repoURL: '' }, valuesObjectText: 'a: [', patchesText: '' },
      { source: { repoURL: 'x', ref: 'values' }, valuesObjectText: '', patchesText: 'a: b' },
      { source: { repoURL: 'y', ref: 'values' }, valuesObjectText: '- list', patchesText: '' },
    ] as typeof drafts;
    const errors = validateSources(broken, true);
    expect(errors[0]?.repoURL).toBeDefined();
    expect(errors[0]?.location).toBeDefined();
    expect(errors[0]?.valuesObject).toMatch(/Line 1/);
    expect(errors[1]?.patches).toBe('Patches must be a YAML list.');
    expect(errors[2]?.ref).toMatch(/already used/);
    expect(errors[2]?.valuesObject).toBe('Values object must be a YAML mapping.');
  });

  it('applies drafts with parsed YAML and switches between single and multi source', () => {
    const app = searchApp();
    const drafts = sourceDrafts(app);
    const first = drafts[0];
    if (!first) throw new Error('draft expected');
    first.patchesText = '- patch: |-\n    - op: replace\n      path: /spec/replicas\n      value: 2\n  target:\n    kind: StatefulSet\n';
    const single = applySources(app, drafts, false);
    expect(single.spec.source?.kustomize?.patches?.[0]?.target).toEqual({ kind: 'StatefulSet' });
    const multi = applySources(app, [...drafts, newSourceDraft({ repoURL: 'https://git.example.test/values.git', ref: 'values', path: '' })], true);
    expect(multi.spec.source).toBeUndefined();
    expect(multi.spec.sources).toHaveLength(2);
    expect(multi.spec.sources?.[1]).toEqual({ repoURL: 'https://git.example.test/values.git', ref: 'values' });
  });

  it('merges Helm defaults with overrides', () => {
    const rows = helmParamRows([{ name: 'replicaCount', value: '3' }, { name: 'extra', value: 'x' }], [
      { name: 'replicaCount', value: '1' },
      { name: 'image.tag', value: '1.0' },
    ]);
    expect(rows).toEqual([
      { name: 'extra', value: 'x', defaultValue: undefined, overridden: true, forceString: false },
      { name: 'image.tag', value: '1.0', defaultValue: '1.0', overridden: false, forceString: false },
      { name: 'replicaCount', value: '3', defaultValue: '1', overridden: true, forceString: false },
    ]);
    expect(setHelmParam([{ name: 'a', value: '1' }], 'a', undefined)).toEqual([]);
    expect(setHelmParam([], 'b', '2', true)).toEqual([{ name: 'b', value: '2', forceString: true }]);
  });

  it('reads plugin announcements defensively', () => {
    expect(parseAnnouncement(undefined)).toEqual([]);
    const params = parseAnnouncement({
      parametersAnnouncement: [{ name: 'env', title: 'Environment', required: true, string: 'dev' }, { name: 'list', collectionType: 'array', array: ['a', 1] }, { title: 'no name' }, 'junk'],
    });
    expect(params.map((p) => [p.name, p.collectionType])).toEqual([
      ['env', 'string'],
      ['list', 'array'],
    ]);
    expect(params[1]?.array).toEqual(['a']);
    expect(setPluginParam([{ name: 'env', string: 'dev' }], { name: 'env' })).toEqual([]);
  });
});

describe('manifest editor', () => {
  it('round-trips the editable manifest', () => {
    const app = paymentsApp();
    const result = parseManifest(manifestYAML(app), app);
    expect(result.ok).toBe(true);
    if (result.ok) {
      expect(result.app.spec).toEqual(app.spec);
      expect(result.app.status).toEqual(app.status);
      expect(result.app.metadata.resourceVersion).toBe(app.metadata.resourceVersion);
    }
  });

  it('reports YAML syntax errors with a position and validates the shape', () => {
    const app = guestbookApp();
    const syntax = parseManifest('spec: [', app);
    expect(syntax.ok).toBe(false);
    if (!syntax.ok) expect(syntax.errors[0]).toMatch(/^Line 1/);
    const shape = parseManifest('kind: Deployment\nmetadata:\n  name: other\nspec:\n  destination: {}\n', app);
    expect(shape.ok).toBe(false);
    if (!shape.ok) {
      expect(shape.errors).toEqual([
        'kind must be Application.',
        'metadata.name cannot change; create a new application to rename it.',
        'spec.project is required.',
        'spec.destination needs a server or a name.',
        'spec.source or spec.sources is required.',
      ]);
    }
  });
});

describe('row drafts of Kustomize common labels and plugin maps', () => {
  it('keeps rows without a key in the draft and writes only complete rows', () => {
    const app = searchApp();
    const [draft] = sourceDrafts(app);
    if (!draft) throw new Error('draft expected');
    expect(draft.commonLabels).toEqual([]);
    // A freshly added row has no key yet: it stays in the draft but is not written.
    draft.commonLabels = [{ key: '', value: '' }];
    expect(applySources(app, [draft], false).spec.source?.kustomize?.commonLabels).toBeUndefined();
    draft.commonLabels = [{ key: 'tier', value: 'backend' }, { key: '', value: '' }];
    draft.commonAnnotations = [{ key: 'example.test/owner', value: 'search-team' }];
    draft.source.kustomize = { ...draft.source.kustomize, replicas: [{ name: '', count: 1 }, { name: 'search', count: 2 }] };
    const kustomize = applySources(app, [draft], false).spec.source?.kustomize;
    expect(kustomize?.commonLabels).toEqual({ tier: 'backend' });
    expect(kustomize?.commonAnnotations).toEqual({ 'example.test/owner': 'search-team' });
    expect(kustomize?.replicas).toEqual([{ name: 'search', count: 2 }]);
  });

  it('validates common label keys and plugin map keys per row', () => {
    const app = searchApp();
    const [draft] = sourceDrafts(app);
    if (!draft) throw new Error('draft expected');
    draft.commonLabels = [{ key: 'bad key', value: 'x' }, { key: '', value: 'orphan' }];
    const errors = validateSources([draft], false)[0];
    expect(Object.keys(errors?.commonLabels ?? {})).toEqual(['0', '1']);
    const plugin = { ...draft, source: { ...draft.source, kustomize: undefined, plugin: {} }, pluginMaps: { values: [{ key: 'a', value: '1' }, { key: 'a', value: '2' }] } };
    expect(validateSources([plugin], false)[0]?.pluginMaps).toEqual({ values: { 1: 'Duplicate key a.' } });
  });

  it('round-trips plugin map parameters in place and drops emptied ones', () => {
    const app = searchApp();
    app.spec.source = {
      repoURL: 'https://git.example.test/platform/apps.git',
      path: 'search',
      plugin: { name: 'cue', parameters: [{ name: 'values', map: { replicas: '1' } }, { name: 'env', string: 'prod' }] },
    };
    const [draft] = sourceDrafts(app);
    if (!draft) throw new Error('draft expected');
    expect(draft.pluginMaps).toEqual({ values: [{ key: 'replicas', value: '1' }] });
    expect(applySources(app, [draft], false).spec.source).toEqual(app.spec.source);
    draft.pluginMaps = { values: [{ key: 'replicas', value: '1' }, { key: 'image', value: 'v2' }], extra: [{ key: '', value: '' }] };
    expect(applySources(app, [draft], false).spec.source?.plugin?.parameters).toEqual([
      { name: 'values', map: { replicas: '1', image: 'v2' } },
      { name: 'env', string: 'prod' },
    ]);
    draft.pluginMaps = { values: [] };
    expect(applySources(app, [draft], false).spec.source?.plugin?.parameters).toEqual([{ name: 'env', string: 'prod' }]);
    expect(setPluginMap(undefined, 'values', { a: '1' })).toEqual([{ name: 'values', map: { a: '1' } }]);
  });
});

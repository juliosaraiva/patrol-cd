import { describe, expect, it } from 'vitest';
import type { Application } from '../../../api/types';
import { guestbookApp, paymentsApp } from '../../../mocks/fixtures/apps-core';
import { cloneJSON } from '../shared/app';
import { deepEqual, diffPaths, editableChanges, rebaseApplication, rebaseManifest, rebaseSources, rebaseSummary } from './rebase';
import { applySummary, manifestYAML, sourceDrafts, summaryFormFrom } from './specEdit';

function edit(app: Application, change: (a: Application) => void): Application {
  const next = cloneJSON(app);
  change(next);
  return next;
}

describe('deepEqual and diffPaths', () => {
  it('ignores key order and undefined-valued keys', () => {
    expect(deepEqual({ a: 1, b: [1, { c: 2 }] }, { b: [1, { c: 2 }], a: 1, d: undefined })).toBe(true);
    expect(deepEqual([1, 2], [2, 1])).toBe(false);
    expect(deepEqual({ a: {} }, { a: undefined })).toBe(false);
  });

  it('names changed leaves, keys with dots and named list items', () => {
    expect(diffPaths({ 'example.test/x': 'a', team: 'web' }, { team: 'ops' }, 'metadata.annotations')).toEqual([
      'metadata.annotations["example.test/x"]',
      'metadata.annotations.team',
    ]);
    expect(diffPaths([{ name: 'a', value: '1' }], [{ name: 'a', value: '2' }, { name: 'b', value: '3' }], 'p')).toEqual(['p[name=a].value', 'p[name=b]']);
  });
});

describe('editableChanges', () => {
  it('ignores status and resourceVersion updates from the controller', () => {
    const base = guestbookApp();
    const latest = edit(base, (a) => {
      a.metadata.resourceVersion = '99';
      a.metadata.generation = 7;
      if (a.status) a.status.health = { status: 'Degraded' };
    });
    expect(editableChanges(base, latest)).toEqual([]);
  });

  it('lists label, annotation, finalizer and spec changes', () => {
    const base = guestbookApp();
    const latest = edit(base, (a) => {
      a.metadata.labels = { ...a.metadata.labels, owner: 'platform' };
      a.metadata.finalizers = [];
      a.spec.destination.namespace = 'guestbook-prod';
    });
    expect(editableChanges(base, latest)).toEqual(['metadata.labels.owner', 'metadata.finalizers', 'spec.destination.namespace']);
  });
});

describe('rebaseApplication', () => {
  it("applies the user's edits onto the latest version and keeps concurrent changes", () => {
    const base = edit(guestbookApp(), (a) => {
      a.metadata.annotations = { ...a.metadata.annotations, 'cd.patrol.example/refresh': 'normal' };
    });
    const mine = edit(base, (a) => {
      a.spec.destination.namespace = 'guestbook-v2';
      a.metadata.labels = { ...a.metadata.labels, tier: 'frontend' };
    });
    const latest = edit(base, (a) => {
      a.metadata.resourceVersion = '7';
      // Another user annotates; the controller removes the refresh annotation after refreshing.
      a.metadata.annotations = { 'link.cd.patrol.example/dashboard': base.metadata.annotations?.['link.cd.patrol.example/dashboard'] ?? '', 'example.test/changed-by': 'someone-else' };
      if (a.status) a.status.health = { status: 'Progressing' };
    });
    const result = rebaseApplication(base, mine, latest);
    expect(result.merged.metadata.resourceVersion).toBe('7');
    expect(result.merged.status?.health?.status).toBe('Progressing');
    expect(result.merged.metadata.annotations).toEqual(latest.metadata.annotations);
    expect(result.merged.metadata.annotations?.['cd.patrol.example/refresh']).toBeUndefined();
    expect(result.merged.metadata.labels).toEqual({ ...base.metadata.labels, tier: 'frontend' });
    expect(result.merged.spec.destination.namespace).toBe('guestbook-v2');
    expect(result.serverChanges).toEqual(['metadata.annotations["cd.patrol.example/refresh"]', 'metadata.annotations["example.test/changed-by"]']);
    expect(result.conflicts).toEqual([]);
  });

  it("keeps the user's value when both sides changed a field and reports the conflict", () => {
    const base = guestbookApp();
    const mine = edit(base, (a) => {
      a.spec.destination.namespace = 'mine';
    });
    const latest = edit(base, (a) => {
      a.spec.destination.namespace = 'theirs';
      a.spec.revisionHistoryLimit = 3;
    });
    const result = rebaseApplication(base, mine, latest);
    expect(result.merged.spec.destination.namespace).toBe('mine');
    expect(result.merged.spec.revisionHistoryLimit).toBe(3);
    expect(result.conflicts).toEqual(['spec.destination.namespace']);
    expect(result.serverChanges).toEqual(['spec.revisionHistoryLimit']);
  });

  it('merges lists of named items per name and removes what either side deleted', () => {
    const base = edit(paymentsApp(), (a) => {
      a.metadata.labels = { keep: '1', drop: '2', theirsDrop: '3' };
    });
    const helm = (a: Application) => {
      const source = a.spec.sources?.[0];
      if (!source) throw new Error('fixture has no first source');
      source.helm = { ...source.helm };
      return source.helm;
    };
    helm(base).parameters = [
      { name: 'image.tag', value: '1.4.2' },
      { name: 'replicaCount', value: '1' },
    ];
    const mine = edit(base, (a) => {
      helm(a).parameters = [
        { name: 'image.tag', value: '1.5.0' },
        { name: 'replicaCount', value: '1' },
        { name: 'debug', value: 'true' },
      ];
      a.metadata.labels = { keep: '1', theirsDrop: '3' };
    });
    const latest = edit(base, (a) => {
      helm(a).parameters = [
        { name: 'image.tag', value: '1.4.2' },
        { name: 'replicaCount', value: '3' },
      ];
      a.metadata.labels = { keep: '1', drop: '2' };
      const second = a.spec.sources?.[1];
      if (second) second.targetRevision = 'release-2';
    });
    const result = rebaseApplication(base, mine, latest);
    expect(result.merged.spec.sources?.[0]?.helm?.parameters).toEqual([
      { name: 'image.tag', value: '1.5.0' },
      { name: 'replicaCount', value: '3' },
      { name: 'debug', value: 'true' },
    ]);
    expect(result.merged.spec.sources?.[1]?.targetRevision).toBe('release-2');
    expect(result.merged.metadata.labels).toEqual({ keep: '1' });
    expect(result.conflicts).toEqual([]);
  });
});

describe('editor rebases', () => {
  it('rebases the summary form, keeping the edit and taking the new label', () => {
    const base = guestbookApp();
    const form = { ...summaryFormFrom(base), destinationNamespace: 'guestbook-v2' };
    const latest = edit(base, (a) => {
      a.metadata.resourceVersion = '8';
      a.metadata.labels = { ...a.metadata.labels, owner: 'platform' };
    });
    const { draft, report } = rebaseSummary(base, form, latest);
    expect(draft.destinationNamespace).toBe('guestbook-v2');
    expect(draft.labels).toContainEqual({ key: 'owner', value: 'platform' });
    expect(report.serverChanges).toEqual(['metadata.labels.owner']);
    expect(applySummary(latest, draft).metadata.labels?.owner).toBe('platform');
  });

  it('rebases source drafts of a multi-source application', () => {
    const base = paymentsApp();
    const drafts = sourceDrafts(base);
    const first = drafts[0];
    if (!first) throw new Error('draft expected');
    first.source = { ...first.source, targetRevision: '2.0.0' };
    const latest = edit(base, (a) => {
      const second = a.spec.sources?.[1];
      if (second) second.targetRevision = 'release-2';
    });
    const { draft, report } = rebaseSources(base, { drafts, multi: true }, latest);
    expect(draft.multi).toBe(true);
    expect(draft.drafts.map((d) => d.source.targetRevision)).toEqual(['2.0.0', 'release-2']);
    expect(report.serverChanges).toEqual(['spec.sources[1].targetRevision']);
  });

  it('rebases a valid manifest and refuses an invalid one', () => {
    const base = guestbookApp();
    const latest = edit(base, (a) => {
      a.metadata.labels = { ...a.metadata.labels, owner: 'platform' };
    });
    const text = manifestYAML(base).replace('namespace: guestbook\n', 'namespace: guestbook-v2\n');
    const rebased = rebaseManifest(base, text, latest);
    expect(rebased?.draft).toContain('owner: platform');
    expect(rebased?.draft).toContain('namespace: guestbook-v2');
    expect(rebaseManifest(base, 'spec: [', latest)).toBeNull();
  });
});

import { describe, expect, it } from 'vitest';
import type { Application } from '../../api/types';
import { canonicalJSON, changeLines, changeYAML, comparableApplication, countChanges, diffApplications, hunks, lineDiff, lineStats } from './diff';

function app(name: string, path: string, extra: Partial<Application['metadata']> = {}): Application {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Application',
    metadata: { name, ...extra },
    spec: { project: 'default', source: { repoURL: 'https://git.example.test/a.git', path }, destination: { server: 'https://kubernetes.default.svc', namespace: name } },
  };
}

describe('lineDiff', () => {
  it('produces a minimal line diff with line numbers', () => {
    expect(lineDiff('a\nb\nc', 'a\nx\nc')).toEqual([
      { kind: 'same', text: 'a', oldNo: 1, newNo: 1 },
      { kind: 'del', text: 'b', oldNo: 2 },
      { kind: 'add', text: 'x', newNo: 2 },
      { kind: 'same', text: 'c', oldNo: 3, newNo: 3 },
    ]);
    expect(lineDiff('', 'a')).toEqual([{ kind: 'add', text: 'a', newNo: 1 }]);
  });

  it('collapses unchanged runs outside the context', () => {
    const a = Array.from({ length: 20 }, (_, i) => `l${i}`).join('\n');
    const b = a.replace('l10', 'changed');
    const segments = hunks(lineDiff(a, b), 2);
    expect(segments.map((s) => (s.type === 'skip' ? `skip ${s.count}` : `lines ${s.lines.length}`))).toEqual(['skip 8', 'lines 6', 'skip 7']);
  });
});

describe('diffApplications', () => {
  it('classifies added, modified, removed and unchanged Applications', () => {
    const existing = [app('a', 'one'), app('b', 'two'), app('c', 'three', { namespace: 'patrol' })];
    const desired = [app('a', 'one'), app('b', 'TWO'), app('d', 'four')];
    const changes = diffApplications(existing, desired, 'patrol');
    expect(changes.map((c) => [c.name, c.change])).toEqual([
      ['d', 'added'],
      ['b', 'modified'],
      ['c', 'removed'],
      ['a', 'unchanged'],
    ]);
    expect(countChanges(changes)).toEqual({ added: 1, modified: 1, removed: 1, unchanged: 1 });
    const modified = changes.find((c) => c.name === 'b');
    expect(modified && changeLines(modified).filter((l) => l.kind !== 'same').map((l) => `${l.kind} ${l.text.trim()}`)).toEqual(['del path: two', 'add path: TWO']);
    expect(modified && lineStats(changeLines(modified))).toEqual({ added: 1, removed: 1 });
  });

  it('classifies without rendering YAML and computes each diff once, on demand', () => {
    // Thousands of generated Applications: classification is cheap and no diff is computed yet.
    const existing = Array.from({ length: 3000 }, (_, i) => app(`app-${i}`, 'v1'));
    const desired = existing.map((a, i) => (i % 2 === 0 ? app(a.metadata.name ?? '', 'v2') : a));
    const changes = diffApplications(existing, desired, 'patrol');
    expect(countChanges(changes)).toEqual({ added: 0, modified: 1500, removed: 0, unchanged: 1500 });
    expect(changes[0]).not.toHaveProperty('lines');
    const first = changes[0]!;
    const lines = changeLines(first);
    expect(changeLines(first)).toBe(lines);
    expect(changeYAML(first).after).toContain('path: v2');
    expect(lineStats(lines)).toEqual({ added: 1, removed: 1 });
  });

  it('compares regardless of key order', () => {
    expect(canonicalJSON({ b: 1, a: { d: [{ z: 1, y: 2 }], c: undefined } })).toBe(canonicalJSON({ a: { d: [{ y: 2, z: 1 }] }, b: 1 }));
    expect(lineStats(changeLines({ key: 'x/a', name: 'a', namespace: 'x', change: 'added', before: null, after: { a: 1, b: 2 } }))).toEqual({ added: 2, removed: 0 });
  });

  it('ignores server-managed metadata and status', () => {
    const live = app('a', 'one', {
      namespace: 'patrol',
      uid: 'u',
      resourceVersion: '9',
      annotations: { 'notified.notifications.cd.patrol.example': '{}', 'cd.patrol.example/refresh': 'normal' },
    });
    live.status = { health: { status: 'Healthy' } };
    expect(comparableApplication(live)).toEqual(comparableApplication(app('a', 'one', { namespace: 'patrol' })));
    expect(diffApplications([live], [app('a', 'one')], 'patrol')[0]?.change).toBe('unchanged');
  });
});

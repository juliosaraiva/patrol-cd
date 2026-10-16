import { describe, expect, it } from 'vitest';
import { applicationFixture } from '../../mocks/fixtures';
import { mergeWatchEvents } from './watch';

describe('mergeWatchEvents', () => {
  const a = applicationFixture('a');
  const b = applicationFixture('b');

  it('upserts in place, appends new objects and removes deleted ones', () => {
    const a2 = applicationFixture('a', { health: 'Degraded' });
    const c = applicationFixture('c');
    const result = mergeWatchEvents(
      [a, b],
      [
        { type: 'MODIFIED', object: a2 },
        { type: 'ADDED', object: c },
        { type: 'DELETED', object: b },
      ],
    );
    expect(result.map((x) => x.metadata.name)).toEqual(['a', 'c']);
    expect(result[0]?.status?.health?.status).toBe('Degraded');
  });

  it('distinguishes objects by namespace', () => {
    const other = applicationFixture('a', { namespace: 'team-b' });
    expect(mergeWatchEvents([a], [{ type: 'ADDED', object: other }])).toHaveLength(2);
  });

  it('applies events in order (delete then re-add)', () => {
    const result = mergeWatchEvents([a], [
      { type: 'DELETED', object: a },
      { type: 'ADDED', object: a },
    ]);
    expect(result).toHaveLength(1);
  });

  it('returns the same array when there are no events', () => {
    const list = [a];
    expect(mergeWatchEvents(list, [])).toBe(list);
  });
});

import { describe, expect, it } from 'vitest';
import { globMatch } from './glob';

describe('globMatch (minimatch-compatible subset used by resource tabs)', () => {
  it.each([
    // [value, pattern, expected]
    ['', '**', true],
    ['apps', '**', true],
    ['', '*', false],
    ['', '', true],
    ['apps', '*', true],
    ['apps', 'apps', true],
    ['apps', 'app', false],
    ['apps', 'app?', true],
    ['argoproj.io', '*.io', true],
    ['argoproj.io', 'argoproj.*', true],
    ['cert-manager.io', '{apps,cert-manager.io}', true],
    ['batch', '{apps,cert-manager.io}', false],
    ['Deployment', 'Deploy*', true],
    ['deployment', 'Deployment', false],
    ['Pod', '[PQ]od', true],
    ['Rod', '[!PQ]od', true],
    ['Pod', '[!PQ]od', false],
    ['.hidden', '*', false],
    ['a.b', 'a\\.b', true],
    ['identify.provider.crossplane.io', '*.crossplane.io', true],
    ['crossplane.io', '*.crossplane.io', false],
    ['a{b', 'a{b', true],
  ])('%j against %j is %s', (value, pattern, expected) => {
    expect(globMatch(value, pattern)).toBe(expected);
  });

  it('treats ** inside a segment like *', () => {
    expect(globMatch('apps', 'a**')).toBe(true);
    expect(globMatch('', 'a**')).toBe(false);
  });

  it('supports globstar across path segments', () => {
    expect(globMatch('a/b/c', 'a/**')).toBe(true);
    expect(globMatch('a/b/c', '**/c')).toBe(true);
    expect(globMatch('a/b/c', 'a/*')).toBe(false);
  });
});

/**
 * Glob matching for resource-tab registrations (API-121). Argo CD uses minimatch with default
 * options; this implements the subset that matters for API groups and kinds with the same
 * results:
 *
 * - `*` matches any run of characters except "/" but not an empty string on its own, `?` one
 *   character, `[abc]`/`[a-z]`/`[!a]` character classes, `{a,b}` alternatives, `\x` a literal.
 * - A path segment that is exactly `**` (globstar) matches any number of segments, including
 *   none: `**` is the only pattern that matches the empty core group "".
 * - A wildcard at the start of a segment does not match a leading "." (minimatch `dot: false`).
 */

const cache = new Map<string, RegExp | null>();

function segmentRegExp(segment: string): RegExp | null {
  const cached = cache.get(segment);
  if (cached !== undefined) return cached;
  let out = '';
  let i = 0;
  let braceDepth = 0;
  while (i < segment.length) {
    const c = segment[i] ?? '';
    if (c === '\\' && i + 1 < segment.length) {
      out += escapeRegExp(segment[i + 1] ?? '');
      i += 2;
      continue;
    }
    if (c === '*') {
      while (segment[i + 1] === '*') i += 1;
      out += '[^/]*';
    } else if (c === '?') {
      out += '[^/]';
    } else if (c === '[') {
      const end = segment.indexOf(']', i + 2);
      if (end === -1) {
        out += '\\[';
      } else {
        let body = segment.slice(i + 1, end);
        let negate = false;
        if (body.startsWith('!') || body.startsWith('^')) {
          negate = true;
          body = body.slice(1);
        }
        out += `[${negate ? '^' : ''}${body.replace(/\\/g, '\\\\').replace(/\]/g, '\\]')}]`;
        i = end;
      }
    } else if (c === '{') {
      braceDepth += 1;
      out += '(?:';
    } else if (c === '}' && braceDepth > 0) {
      braceDepth -= 1;
      out += ')';
    } else if (c === ',' && braceDepth > 0) {
      out += '|';
    } else {
      out += escapeRegExp(c);
    }
    i += 1;
  }
  let re: RegExp | null;
  if (braceDepth > 0) {
    // Unbalanced braces: minimatch treats the pattern literally.
    re = new RegExp(`^${escapeRegExp(segment)}$`);
  } else {
    try {
      re = new RegExp(`^${out}$`);
    } catch {
      re = null;
    }
  }
  cache.set(segment, re);
  return re;
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&');
}

function matchSegment(value: string, pattern: string): boolean {
  if (value === '') return pattern === '';
  if (value.startsWith('.') && /^[*?[]/.test(pattern)) return false;
  return segmentRegExp(pattern)?.test(value) ?? false;
}

function matchSegments(values: readonly string[], patterns: readonly string[]): boolean {
  if (patterns.length === 0) return values.length === 0;
  const [head, ...rest] = patterns;
  if (head === '**') {
    for (let skip = 0; skip <= values.length; skip += 1) {
      const remaining = values.slice(skip);
      // A globstar never swallows dot segments (minimatch dot: false).
      if (values.slice(0, skip).some((v) => v.startsWith('.'))) return false;
      if (matchSegments(remaining, rest)) return true;
    }
    return false;
  }
  if (values.length === 0) return false;
  return matchSegment(values[0] ?? '', head ?? '') && matchSegments(values.slice(1), rest);
}

/** True when `value` matches the glob `pattern` (see the module comment for the syntax). */
export function globMatch(value: string, pattern: string): boolean {
  if (pattern === '**') return !value.split('/').some((v) => v.startsWith('.'));
  const values = value === '' ? [] : value.split('/');
  const patterns = pattern === '' ? [] : pattern.split('/');
  if (values.length === 0) return patterns.length === 0 || patterns.every((p) => p === '**');
  return matchSegments(values, patterns);
}

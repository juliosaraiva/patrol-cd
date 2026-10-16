/*
 * Line diff for manifests (YAML renderings of live and desired state).
 *
 * Common prefix and suffix are trimmed first (most manifest changes touch a few fields), then
 * the middle runs Myers' O((N+M)·D) algorithm. When the edit distance exceeds `maxEdits` the
 * middle is reported as one replaced block instead: still a correct diff, just not minimal,
 * and it keeps very different large documents from costing seconds and memory.
 */

export type DiffOpType = 'equal' | 'delete' | 'insert';

export interface DiffOp {
  type: DiffOpType;
  /** 1-based line number in the old text (equal/delete). */
  oldLine?: number | undefined;
  /** 1-based line number in the new text (equal/insert). */
  newLine?: number | undefined;
  text: string;
}

export function splitLines(text: string): string[] {
  if (text === '') return [];
  const lines = text.split('\n');
  if (lines[lines.length - 1] === '') lines.pop();
  return lines;
}

function myers(a: readonly string[], b: readonly string[], maxEdits: number): DiffOpType[] | null {
  const n = a.length;
  const m = b.length;
  const max = n + m;
  const offset = max + 1;
  const v = new Int32Array(2 * max + 3);
  const trace: Int32Array[] = [];
  let found = -1;
  for (let d = 0; d <= Math.min(max, maxEdits); d += 1) {
    // Keep only the diagonals this round can touch.
    trace.push(v.slice(offset - d - 1, offset + d + 2));
    for (let k = -d; k <= d; k += 2) {
      let x: number;
      if (k === -d || (k !== d && (v[offset + k - 1] ?? 0) < (v[offset + k + 1] ?? 0))) x = v[offset + k + 1] ?? 0;
      else x = (v[offset + k - 1] ?? 0) + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x += 1;
        y += 1;
      }
      v[offset + k] = x;
      if (x >= n && y >= m) {
        found = d;
        break;
      }
    }
    if (found >= 0) break;
  }
  if (found < 0) return null;

  // Backtrack through the saved V snapshots.
  const ops: DiffOpType[] = [];
  let x = n;
  let y = m;
  for (let d = found; d > 0; d -= 1) {
    const snap = trace[d]!; // V before round d, diagonals -d-1..d+1
    const at = (k: number) => snap[k + d + 1] ?? 0;
    const k = x - y;
    const prevK = k === -d || (k !== d && at(k - 1) < at(k + 1)) ? k + 1 : k - 1;
    const prevX = at(prevK);
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      ops.push('equal');
      x -= 1;
      y -= 1;
    }
    ops.push(x === prevX ? 'insert' : 'delete');
    x = prevX;
    y = prevY;
  }
  while (x > 0 && y > 0) {
    ops.push('equal');
    x -= 1;
    y -= 1;
  }
  return ops.reverse();
}

export function diffLines(oldLines: readonly string[], newLines: readonly string[], maxEdits = 1000): DiffOp[] {
  let start = 0;
  while (start < oldLines.length && start < newLines.length && oldLines[start] === newLines[start]) start += 1;
  let endOld = oldLines.length;
  let endNew = newLines.length;
  while (endOld > start && endNew > start && oldLines[endOld - 1] === newLines[endNew - 1]) {
    endOld -= 1;
    endNew -= 1;
  }
  const a = oldLines.slice(start, endOld);
  const b = newLines.slice(start, endNew);
  const middle = myers(a, b, maxEdits) ?? [...a.map((): DiffOpType => 'delete'), ...b.map((): DiffOpType => 'insert')];

  const result: DiffOp[] = [];
  for (let i = 0; i < start; i += 1) result.push({ type: 'equal', oldLine: i + 1, newLine: i + 1, text: oldLines[i] ?? '' });
  let i = start;
  let j = start;
  for (const op of middle) {
    if (op === 'equal') {
      result.push({ type: 'equal', oldLine: i + 1, newLine: j + 1, text: oldLines[i] ?? '' });
      i += 1;
      j += 1;
    } else if (op === 'delete') {
      result.push({ type: 'delete', oldLine: i + 1, text: oldLines[i] ?? '' });
      i += 1;
    } else {
      result.push({ type: 'insert', newLine: j + 1, text: newLines[j] ?? '' });
      j += 1;
    }
  }
  for (; i < oldLines.length; i += 1, j += 1) result.push({ type: 'equal', oldLine: i + 1, newLine: j + 1, text: oldLines[i] ?? '' });
  return result;
}

export function diffStats(ops: readonly DiffOp[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const op of ops) {
    if (op.type === 'insert') added += 1;
    else if (op.type === 'delete') removed += 1;
  }
  return { added, removed };
}

/** A run of unchanged lines hidden in compact mode. */
export interface SkipRow {
  type: 'skip';
  id: string;
  count: number;
  /** Ops hidden by this row (shown when expanded). */
  ops: DiffOp[];
}

export type UnifiedRow = DiffOp | SkipRow;

/**
 * Unified rows. With `context` set, unchanged runs longer than 2×context (or at the edges,
 * longer than context) collapse into skip rows unless their id is in `expanded`.
 */
export function unifiedRows(ops: readonly DiffOp[], context: number | null, expanded: ReadonlySet<string> = new Set()): UnifiedRow[] {
  if (context === null) return [...ops];
  const rows: UnifiedRow[] = [];
  let i = 0;
  while (i < ops.length) {
    const op = ops[i]!;
    if (op.type !== 'equal') {
      rows.push(op);
      i += 1;
      continue;
    }
    let j = i;
    while (j < ops.length && (ops[j]!).type === 'equal') j += 1;
    const run = ops.slice(i, j);
    const leading = i === 0 ? 0 : context;
    const trailing = j === ops.length ? 0 : context;
    if (run.length > leading + trailing) {
      const hidden = run.slice(leading, run.length - trailing);
      const id = `skip-${hidden[0]?.oldLine ?? 0}-${hidden[0]?.newLine ?? 0}`;
      if (expanded.has(id)) rows.push(...run);
      else {
        rows.push(...run.slice(0, leading));
        rows.push({ type: 'skip', id, count: hidden.length, ops: hidden });
        rows.push(...run.slice(run.length - trailing));
      }
    } else {
      rows.push(...run);
    }
    i = j;
  }
  return rows;
}

export interface SplitCell {
  line: number;
  text: string;
  type: DiffOpType;
}

export type SplitRow = { type: 'pair'; left?: SplitCell | undefined; right?: SplitCell | undefined } | SkipRow;

/** Side-by-side rows: deletions and insertions of one block are paired line by line. */
export function splitRows(rows: readonly UnifiedRow[]): SplitRow[] {
  const result: SplitRow[] = [];
  let i = 0;
  while (i < rows.length) {
    const row = rows[i]!;
    if (row.type === 'skip') {
      result.push(row);
      i += 1;
      continue;
    }
    if (row.type === 'equal') {
      result.push({
        type: 'pair',
        left: { line: row.oldLine ?? 0, text: row.text, type: 'equal' },
        right: { line: row.newLine ?? 0, text: row.text, type: 'equal' },
      });
      i += 1;
      continue;
    }
    const dels: DiffOp[] = [];
    const ins: DiffOp[] = [];
    while (i < rows.length && (rows[i]!).type === 'delete') dels.push(rows[i++] as DiffOp);
    while (i < rows.length && (rows[i]!).type === 'insert') ins.push(rows[i++] as DiffOp);
    const count = Math.max(dels.length, ins.length);
    for (let k = 0; k < count; k += 1) {
      const d = dels[k];
      const n = ins[k];
      result.push({
        type: 'pair',
        left: d ? { line: d.oldLine ?? 0, text: d.text, type: 'delete' } : undefined,
        right: n ? { line: n.newLine ?? 0, text: n.text, type: 'insert' } : undefined,
      });
    }
  }
  return result;
}

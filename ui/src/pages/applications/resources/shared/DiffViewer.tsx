import { useMemo, useState, type ReactNode } from 'react';
import { diffLines, diffStats, splitLines, splitRows, unifiedRows, type DiffOp, type SkipRow, type SplitCell } from '../model/diff';
import styles from './DiffViewer.module.css';

export interface DiffViewerProps {
  /** Accessible caption, e.g. "Diff of Deployment guestbook-ui". */
  caption: string;
  oldText: string;
  newText: string;
  oldLabel?: string;
  newLabel?: string;
  /** 2 lines of context around changes instead of the whole document. */
  compact: boolean;
  /** Unified (one column) instead of side by side. */
  inline: boolean;
}

const ROW_HEIGHT = 20;
const WINDOW_THRESHOLD = 600;
const OVERSCAN = 20;
const VIEWPORT = 560;

function Marker({ type }: { type: DiffOp['type'] }) {
  if (type === 'insert')
    return (
      <>
        <span aria-hidden="true">+</span>
        <span className="sr-only">Added</span>
      </>
    );
  if (type === 'delete')
    return (
      <>
        <span aria-hidden="true">−</span>
        <span className="sr-only">Removed</span>
      </>
    );
  return null;
}

const typeClass = (type: DiffOp['type'] | undefined) => (type === 'insert' ? styles.insert : type === 'delete' ? styles.delete : undefined);

/**
 * Line diff of two manifests (old = live, new = desired by default). Changes carry a +/−
 * marker and a spoken "Added"/"Removed", never color alone. Compact mode collapses unchanged
 * runs into expandable rows; very long diffs render only the rows near the scroll position.
 */
export function DiffViewer({ caption, oldText, newText, oldLabel = 'Live', newLabel = 'Desired', compact, inline }: DiffViewerProps) {
  const ops = useMemo(() => diffLines(splitLines(oldText), splitLines(newText)), [oldText, newText]);
  const stats = useMemo(() => diffStats(ops), [ops]);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());
  const unified = useMemo(() => unifiedRows(ops, compact ? 2 : null, expanded), [ops, compact, expanded]);
  const split = useMemo(() => (inline ? [] : splitRows(unified)), [unified, inline]);
  const count = inline ? unified.length : split.length;
  const [scrollTop, setScrollTop] = useState(0);
  const windowed = count > WINDOW_THRESHOLD;
  const start = windowed ? Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN) : 0;
  const end = windowed ? Math.min(count, Math.ceil((scrollTop + VIEWPORT) / ROW_HEIGHT) + OVERSCAN) : count;
  const columns = 4;

  const expand = (skip: SkipRow) => setExpanded((s) => new Set(s).add(skip.id));
  const skipRow = (skip: SkipRow, key: string) => (
    <tr key={key} className={styles.skipRow}>
      <td colSpan={columns}>
        <button type="button" className={styles.skipButton} onClick={() => expand(skip)}>
          Show {skip.count} unchanged {skip.count === 1 ? 'line' : 'lines'}
        </button>
      </td>
    </tr>
  );

  const body: ReactNode[] = [];
  if (windowed && start > 0) body.push(<tr key="top" aria-hidden="true" style={{ height: start * ROW_HEIGHT }} />);
  for (let i = start; i < end; i += 1) {
    if (inline) {
      const row = unified[i];
      if (!row) continue;
      if (row.type === 'skip') {
        body.push(skipRow(row, `u${i}`));
        continue;
      }
      body.push(
        <tr key={`u${i}`} className={typeClass(row.type)}>
          <td className={styles.num}>{row.oldLine ?? ''}</td>
          <td className={styles.num}>{row.newLine ?? ''}</td>
          <td className={styles.marker}>
            <Marker type={row.type} />
          </td>
          <td className={styles.code}>{row.text}</td>
        </tr>,
      );
    } else {
      const row = split[i];
      if (!row) continue;
      if (row.type === 'skip') {
        body.push(skipRow(row, `s${i}`));
        continue;
      }
      const cell = (c: SplitCell | undefined) => (
        <>
          <td className={[styles.num, typeClass(c?.type)].filter(Boolean).join(' ')}>{c?.line ?? ''}</td>
          <td className={[styles.code, typeClass(c?.type), c ? undefined : styles.empty].filter(Boolean).join(' ')}>
            {c && c.type !== 'equal' ? <Marker type={c.type} /> : null}
            {c ? <span className={styles.text}>{c.text}</span> : null}
          </td>
        </>
      );
      body.push(
        <tr key={`s${i}`}>
          {cell(row.left)}
          {cell(row.right)}
        </tr>,
      );
    }
  }
  if (windowed && end < count) body.push(<tr key="bottom" aria-hidden="true" style={{ height: (count - end) * ROW_HEIGHT }} />);

  return (
    <div className={styles.root}>
      <p className={styles.stats}>
        <span className={styles.added}>+{stats.added}</span> <span className={styles.removed}>−{stats.removed}</span>
        <span className="sr-only">
          : {stats.added} lines added, {stats.removed} lines removed ({oldLabel} to {newLabel})
        </span>
      </p>
      {/* Scrollable region: focusable so keyboard users can scroll wide or long diffs. */}
      <div
        className={styles.scroll}
        style={windowed ? { maxHeight: VIEWPORT } : undefined}
        // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex -- scrollable region must be keyboard scrollable
        tabIndex={0}
        role="region"
        aria-label={caption}
        onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
      >
        <table className={[styles.table, inline ? styles.inline : styles.split].join(' ')}>
          <caption className="sr-only">{caption}</caption>
          <thead>
            {inline ? (
              <tr>
                <th scope="col" className={styles.num}>
                  <span className="sr-only">{oldLabel} line</span>
                </th>
                <th scope="col" className={styles.num}>
                  <span className="sr-only">{newLabel} line</span>
                </th>
                <th scope="col" className={styles.marker}>
                  <span className="sr-only">Change</span>
                </th>
                <th scope="col">
                  {oldLabel} → {newLabel}
                </th>
              </tr>
            ) : (
              <tr>
                <th scope="col" className={styles.num}>
                  <span className="sr-only">{oldLabel} line</span>
                </th>
                <th scope="col">{oldLabel}</th>
                <th scope="col" className={styles.num}>
                  <span className="sr-only">{newLabel} line</span>
                </th>
                <th scope="col">{newLabel}</th>
              </tr>
            )}
          </thead>
          <tbody>{body}</tbody>
        </table>
      </div>
    </div>
  );
}

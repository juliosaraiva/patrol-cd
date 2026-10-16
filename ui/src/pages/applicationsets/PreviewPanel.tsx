import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { parseDocument } from 'yaml';
import { useGenerateApplicationSet } from '../../api/hooks/applicationsets';
import { isApiError } from '../../api/errors';
import type { Application, ApplicationSet } from '../../api/types';
import { Badge, Banner, Button, CodeBlock, FormField, Mono, Tabs, TextArea } from '../../design';
import { ErrorBanner } from '../../shell/errors';
import { IconChevronDown, IconChevronRight } from '../../design/icons';
import { changeLines, countChanges, diffApplications, hunks, lineStats, toYAML, type ApplicationChange, type DiffLine } from './diff';
import { appSetProjects } from './model';
import styles from './ApplicationSets.module.css';

type Spec = ApplicationSet['spec'];

/** Parses the edited spec; returns a message with the line number on failure. */
export function parseSpec(text: string): { spec: Spec } | { error: string } {
  const doc = parseDocument(text);
  const first = doc.errors[0];
  if (first) {
    const line = first.linePos?.[0]?.line;
    const message = (first.message.split('\n')[0] ?? 'invalid YAML').replace(/ at line \d+, column \d+:?$/, '');
    return { error: `${line ? `Line ${line}: ` : ''}${message}` };
  }
  const value: unknown = doc.toJS();
  if (!value || typeof value !== 'object' || Array.isArray(value)) return { error: 'The spec must be a YAML mapping with generators and template.' };
  const spec = value as Partial<Spec>;
  if (!Array.isArray(spec.generators)) return { error: 'spec.generators must be a list.' };
  if (!spec.template || typeof spec.template !== 'object') return { error: 'spec.template must be a mapping with metadata and spec.' };
  return { spec: spec as Spec };
}

/** Object sent to POST /applicationsets/generate: identity plus the (edited) spec, no status. */
function generateBody(set: ApplicationSet, spec: Spec): ApplicationSet {
  return {
    apiVersion: set.apiVersion,
    kind: set.kind,
    metadata: { name: set.metadata.name, namespace: set.metadata.namespace, labels: set.metadata.labels, annotations: set.metadata.annotations },
    spec,
  };
}

const CHANGE_TONE = { added: 'success', modified: 'warning', removed: 'danger', unchanged: 'neutral' } as const;
const CHANGE_LABEL = { added: 'Added', modified: 'Modified', removed: 'Removed', unchanged: 'Unchanged' } as const;
const MARKER: Record<DiffLine['kind'], string> = { add: '+', del: '-', same: ' ' };
const LINE_SR: Record<DiffLine['kind'], string> = { add: 'added', del: 'removed', same: '' };

/** Changed Applications rendered per page; more on request (large ApplicationSets). */
export const CHANGES_PAGE = 50;
/** Diffs start expanded only when this few Applications changed; otherwise rows start collapsed. */
export const AUTO_EXPAND = 5;
/** Diff rows rendered per Application before "Show all lines". */
export const DIFF_ROWS = 400;
/** Applications per page in the Existing/Desired manifests. */
export const MANIFEST_PAGE = 25;
/** Unchanged Applications named before "and N more". */
const UNCHANGED_NAMES = 20;

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

type DiffRow = { type: 'line'; line: DiffLine } | { type: 'skip'; count: number };

export function DiffView({ change }: { change: ApplicationChange }) {
  const [showAll, setShowAll] = useState(false);
  // The line diff is computed here, when the row is expanded, and cached per change.
  const rows = useMemo(
    () => hunks(changeLines(change)).flatMap((seg): DiffRow[] => (seg.type === 'skip' ? [seg] : seg.lines.map((line) => ({ type: 'line', line })))),
    [change],
  );
  const shown = showAll ? rows : rows.slice(0, DIFF_ROWS);
  return (
    <>
      {/* Focusable so keyboard users can scroll long diffs. */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex */}
      <div className={styles.diff} tabIndex={0} role="region" aria-label={`Changes to ${change.name}`}>
        <table className={styles.diffTable}>
          <tbody>
            {shown.map((row, i) =>
              row.type === 'skip' ? (
                <tr key={i} className={styles.skip}>
                  <td colSpan={4}>{`${row.count} unchanged line${row.count === 1 ? '' : 's'}`}</td>
                </tr>
              ) : (
                <tr key={i} className={row.line.kind === 'add' ? styles.lineAdd : row.line.kind === 'del' ? styles.lineDel : undefined}>
                  <td className={styles.lineNo}>{row.line.oldNo ?? ''}</td>
                  <td className={styles.lineNo}>{row.line.newNo ?? ''}</td>
                  <td className={styles.marker} aria-hidden="true">
                    {MARKER[row.line.kind]}
                  </td>
                  <td>
                    {LINE_SR[row.line.kind] ? <span className="sr-only">{`${LINE_SR[row.line.kind]}: `}</span> : null}
                    {row.line.text}
                  </td>
                </tr>
              ),
            )}
          </tbody>
        </table>
      </div>
      {rows.length > shown.length ? (
        <div>
          <Button size="sm" onClick={() => setShowAll(true)}>{`Show all ${rows.length} lines of ${change.name}`}</Button>
        </div>
      ) : null}
    </>
  );
}

/** One changed Application: a disclosure row (name, change, +/- line counts) over its diff. */
function ChangeItem({ change, expanded, onToggle }: { change: ApplicationChange; expanded: boolean; onToggle: () => void }) {
  const panelId = useId();
  // Only rows on the current page are rendered, so at most CHANGES_PAGE diffs are computed.
  const stats = lineStats(changeLines(change));
  return (
    <li className={styles.change}>
      <h4 className={styles.changeTitle}>
        <button type="button" className={styles.changeToggle} aria-expanded={expanded} aria-controls={panelId} onClick={onToggle}>
          {expanded ? <IconChevronDown /> : <IconChevronRight />}
          <Mono>{change.name}</Mono>
        </button>
        <Badge tone={CHANGE_TONE[change.change]}>{CHANGE_LABEL[change.change]}</Badge>
        <span className={styles.secondary}>{`namespace ${change.namespace}`}</span>
        <span className={styles.secondary}>
          <span aria-hidden="true">{`+${stats.added} \u2212${stats.removed}`}</span>
          <span className="sr-only">{`${plural(stats.added, 'line')} added, ${plural(stats.removed, 'line')} removed`}</span>
        </span>
      </h4>
      <div id={panelId} hidden={!expanded} className={expanded ? styles.change : undefined}>
        {expanded ? <DiffView change={change} /> : null}
      </div>
    </li>
  );
}

function ChangesList({ changed, unchanged, desiredCount }: { changed: ApplicationChange[]; unchanged: ApplicationChange[]; desiredCount: number }) {
  const [limit, setLimit] = useState(CHANGES_PAGE);
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set(changed.length <= AUTO_EXPAND ? changed.map((c) => c.key) : []));
  if (changed.length === 0) {
    return (
      <p className={styles.muted}>
        {desiredCount === 0
          ? 'The spec generates no Applications. Check that the generators return parameters (for example that a list has elements or a cluster selector matches).'
          : 'No changes: the spec generates the same Applications that exist now.'}
      </p>
    );
  }
  const shown = changed.slice(0, limit);
  const remaining = changed.length - shown.length;
  const toggle = (key: string) =>
    setExpanded((current) => {
      const next = new Set(current);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  const unchangedNames = unchanged.slice(0, UNCHANGED_NAMES).map((c) => c.name);
  return (
    <div className={styles.stack}>
      {changed.length > AUTO_EXPAND ? <p className={styles.muted}>{`${changed.length} Applications change. Expand one to see its diff.`}</p> : null}
      <ul className={styles.changes} aria-label="Changed applications">
        {shown.map((c) => (
          <ChangeItem key={c.key} change={c} expanded={expanded.has(c.key)} onToggle={() => toggle(c.key)} />
        ))}
      </ul>
      {remaining > 0 ? (
        <div>
          <Button size="sm" onClick={() => setLimit((n) => n + CHANGES_PAGE)}>{`Show ${Math.min(CHANGES_PAGE, remaining)} more of ${remaining} remaining`}</Button>
        </div>
      ) : null}
      {unchanged.length > 0 ? (
        <p className={styles.muted}>
          {`Unchanged (${unchanged.length}): ${unchangedNames.join(', ')}${unchanged.length > unchangedNames.length ? `, and ${unchanged.length - unchangedNames.length} more` : ''}`}
        </p>
      ) : null}
    </div>
  );
}

/** Manifests of many Applications, rendered a page at a time. */
function ManifestPages({ title, apps }: { title: string; apps: Application[] }) {
  const [limit, setLimit] = useState(MANIFEST_PAGE);
  const code = useMemo(
    () =>
      apps
        .slice(0, limit)
        .map((a) => toYAML(a))
        .join('\n---\n'),
    [apps, limit],
  );
  const remaining = Math.max(0, apps.length - limit);
  return (
    <div className={styles.stack}>
      <CodeBlock title={title} code={code} maxHeight={520} />
      {remaining > 0 ? (
        <div className={styles.actions}>
          <span className={styles.muted}>{`Showing ${apps.length - remaining} of ${apps.length} Applications.`}</span>
          <Button size="sm" onClick={() => setLimit((n) => n + MANIFEST_PAGE)}>{`Show ${Math.min(MANIFEST_PAGE, remaining)} more`}</Button>
        </div>
      ) : null}
    </div>
  );
}

interface PreviewResult {
  /** Distinguishes runs, so paging and expansion reset for each new result. */
  id: number;
  changes: ApplicationChange[];
  existing: Application[];
  desired: Application[];
  base: 'live' | 'current';
  baseWarning: string | null;
}

export interface PreviewPanelProps {
  set: ApplicationSet;
  /** Live Applications owned by the set: null when they could not be listed, undefined while loading. */
  existing: Application[] | null | undefined;
  /** Incremented by the page's "Generate preview" action to run a preview of the current spec. */
  runToken?: number;
}

function describePreviewError(error: unknown, projects: string[]): { title: string; message: string } | null {
  if (!isApiError(error)) return null;
  if (error.code === 'permission_denied') {
    const scope = projects.length > 0 ? ` on project ${projects.join(', ')}` : '';
    return {
      title: 'Preview needs permission to create ApplicationSets',
      message: `Rendering a preview requires applicationsets, create${scope}. Ask an administrator for that permission. (${error.detail})`,
    };
  }
  if (error.code === 'invalid_argument') {
    return { title: 'The server rejected the spec', message: `${error.detail}. Fix the spec and generate again.` };
  }
  return null;
}

/**
 * Sandbox for the ApplicationSet spec (ASET-126): renders the Applications it would generate
 * with POST /applicationsets/generate (dry run) and compares them with the existing ones.
 * Nothing is saved.
 */
export function PreviewPanel({ set, existing, runToken = 0 }: PreviewPanelProps) {
  const original = toYAML(set.spec);
  const [text, setText] = useState(original);
  const [parseError, setParseError] = useState<string | null>(null);
  const [result, setResult] = useState<PreviewResult | null>(null);
  const [view, setView] = useState('changes');
  const generate = useGenerateApplicationSet();
  const baseGenerate = useGenerateApplicationSet();
  const resultsRef = useRef<HTMLHeadingElement>(null);
  const lastRun = useRef(0);
  const runs = useRef(0);
  const focusResult = useRef(false);
  const namespace = set.metadata.namespace ?? '';

  const run = async (specText: string) => {
    const parsed = parseSpec(specText);
    if ('error' in parsed) {
      setParseError(parsed.error);
      return;
    }
    setParseError(null);
    setResult(null);
    let desired: Application[];
    try {
      desired = (await generate.mutateAsync(generateBody(set, parsed.spec))).applications ?? [];
    } catch {
      return; // shown from generate.error
    }
    let base: PreviewResult['base'] = 'live';
    let baseApps = existing ?? null;
    let baseWarning: string | null = null;
    if (baseApps === null) {
      base = 'current';
      try {
        baseApps = (await baseGenerate.mutateAsync(generateBody(set, set.spec))).applications ?? [];
        baseWarning = 'The existing Applications could not be listed, so the preview is compared with what the saved spec generates.';
      } catch (err) {
        baseApps = [];
        baseWarning = `Neither the existing Applications nor the saved spec's output could be loaded (${isApiError(err) ? err.detail : 'unknown error'}); every Application shows as added.`;
      }
    }
    runs.current += 1;
    setResult({ id: runs.current, changes: diffApplications(baseApps, desired, namespace), existing: baseApps, desired, base, baseWarning });
    setView('changes');
    focusResult.current = true;
  };

  useEffect(() => {
    if (focusResult.current && result) {
      resultsRef.current?.focus();
      focusResult.current = false;
    }
  }, [result]);

  const existingLoaded = existing !== undefined;
  useEffect(() => {
    if (!existingLoaded) return;
    if (runToken > 0 && runToken !== lastRun.current) {
      lastRun.current = runToken;
      setText(original);
      void run(original);
    }
    // run/original are recreated each render; only a new token (once existing apps loaded) triggers a run.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runToken, existingLoaded]);

  const counts = result ? countChanges(result.changes) : null;
  const friendly = generate.isError ? describePreviewError(generate.error, appSetProjects(set)) : null;
  const changed = useMemo(() => result?.changes.filter((c) => c.change !== 'unchanged') ?? [], [result]);
  const unchanged = useMemo(() => result?.changes.filter((c) => c.change === 'unchanged') ?? [], [result]);

  return (
    <div className={styles.stack}>
      <form
        className={styles.previewForm}
        onSubmit={(e) => {
          e.preventDefault();
          void run(text);
        }}
      >
        <p className={styles.muted}>
          Edit the spec to try generator or template changes, then generate. The server renders the Applications without creating anything;
          your edits are not saved.
        </p>
        <FormField label="ApplicationSet spec (YAML)" error={parseError ?? undefined} help={text !== original ? 'Edited: differs from the saved spec.' : undefined}>
          <TextArea mono rows={18} className={styles.editor} spellCheck={false} value={text} onChange={(e) => setText(e.target.value)} />
        </FormField>
        <div className={styles.actions}>
          <Button type="submit" variant="primary" loading={generate.isPending || baseGenerate.isPending} disabled={existing === undefined}>
            Preview changes
          </Button>
          {existing === undefined ? <span className={styles.muted}>Loading the existing Applications…</span> : null}
          <Button
            disabled={text === original}
            onClick={() => {
              setText(original);
              setParseError(null);
            }}
          >
            Reset to saved spec
          </Button>
        </div>
      </form>

      {generate.isError ? (
        friendly ? (
          <Banner tone="error" live title={friendly.title}>
            {friendly.message}
          </Banner>
        ) : (
          <ErrorBanner error={generate.error} onRetry={() => void run(text)} title="Preview failed" />
        )
      ) : null}

      <p role="status" className="sr-only">
        {counts ? `Preview generated: ${counts.added} added, ${counts.modified} modified, ${counts.removed} removed, ${counts.unchanged} unchanged.` : ''}
      </p>
      <div className={styles.stack}>
        {result && counts ? (
          <>
            {/* Receives focus after a run so keyboard and screen-reader users land on the result. */}
            <h3 ref={resultsRef} tabIndex={-1} className={styles.sectionTitle}>
              Preview result
            </h3>
            <ul className={styles.counts} aria-label="Summary of changes">
              {(['added', 'modified', 'removed', 'unchanged'] as const).map((k) => (
                <li key={k}>
                  <Badge tone={CHANGE_TONE[k]}>{`${counts[k]} ${k}`}</Badge>
                </li>
              ))}
            </ul>
            {result.baseWarning ? (
              <Banner tone="warning" title="Compared with the saved spec">
                {result.baseWarning}
              </Banner>
            ) : null}
            <Tabs
              label="Preview views"
              value={view}
              onValueChange={setView}
              items={[
                {
                  id: 'changes',
                  label: 'Changes',
                  content: <ChangesList key={result.id} changed={changed} unchanged={unchanged} desiredCount={result.desired.length} />,
                },
                {
                  id: 'existing',
                  label: result.base === 'live' ? `Existing (${result.existing.length})` : `Saved spec output (${result.existing.length})`,
                  content:
                    result.existing.length === 0 ? (
                      <p className={styles.muted}>No Applications exist for this ApplicationSet yet.</p>
                    ) : (
                      <ManifestPages key={result.id} title="Existing applications" apps={result.existing} />
                    ),
                },
                {
                  id: 'desired',
                  label: `Desired (${result.desired.length})`,
                  content:
                    result.desired.length === 0 ? (
                      <p className={styles.muted}>The spec generates no Applications.</p>
                    ) : (
                      <ManifestPages key={result.id} title="Desired applications" apps={result.desired} />
                    ),
                },
              ]}
            />
          </>
        ) : null}
      </div>
    </div>
  );
}

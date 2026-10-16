import { useEffect, useRef, type ReactNode } from 'react';
import { useSearchParams } from 'react-router';
import { Button, IconButton, IconClose, IconPlus, Input } from '../../../design';
import styles from './common.module.css';

export type Row = Record<string, string>;

export interface RowField {
  key: string;
  label: string;
  placeholder?: string;
  mono?: boolean;
  /** id of a <datalist> with suggestions. */
  list?: string;
}

export interface RowListEditorProps {
  legend: ReactNode;
  help?: ReactNode;
  rows: readonly Row[];
  onChange: (rows: Row[]) => void;
  fields: readonly RowField[];
  /** Singular noun used in button and control names, e.g. "destination". */
  noun: string;
  /** Prefix for control ids (`<prefix>-<index>-<field>`), used by error summaries. */
  idPrefix: string;
  /** Error message per row index. */
  errors?: readonly (string | null | undefined)[];
  disabled?: boolean;
  /** Text shown when there are no rows. */
  emptyText?: ReactNode;
}

export function rowFieldId(prefix: string, index: number, key: string): string {
  return `${prefix}-${index}-${key}`;
}

/**
 * Editable list of rows with one input per field, "Add <noun>" and per-row "Remove" buttons.
 * Adding a row focuses its first input; removing one keeps focus inside the list.
 *
 * Rows with several fields show visible field labels: one header row above the columns on wide
 * screens, and a label above each input when the fields stack on narrow screens. Both are
 * visual only; each input's accessible name ("<label>, <noun> <n>") starts with the same text.
 */
export function RowListEditor({ legend, help, rows, onChange, fields, noun, idPrefix, errors, disabled = false, emptyText }: RowListEditorProps) {
  const pendingFocus = useRef<string | null>(null);
  const addRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!pendingFocus.current) return;
    const el = document.getElementById(pendingFocus.current);
    pendingFocus.current = null;
    (el ?? addRef.current)?.focus();
  });
  const firstKey = fields[0]?.key ?? 'value';
  const labelled = fields.length > 1;
  const update = (index: number, key: string, value: string) => onChange(rows.map((r, i) => (i === index ? { ...r, [key]: value } : r)));
  const remove = (index: number) => {
    const next = rows.filter((_, i) => i !== index);
    pendingFocus.current = next.length === 0 ? null : rowFieldId(idPrefix, Math.min(index, next.length - 1), firstKey);
    if (next.length === 0) addRef.current?.focus();
    onChange(next);
  };
  const add = () => {
    pendingFocus.current = rowFieldId(idPrefix, rows.length, firstKey);
    onChange([...rows, Object.fromEntries(fields.map((f) => [f.key, '']))]);
  };
  return (
    <fieldset className={styles.fieldset} disabled={disabled}>
      <legend className={styles.legend}>{legend}</legend>
      {help ? <p className={styles.small}>{help}</p> : null}
      {rows.length === 0 ? (
        emptyText ? <p className={styles.muted}>{emptyText}</p> : null
      ) : (
        <ol className={styles.listRows}>
          {labelled ? (
            <li className={styles.listHead} aria-hidden="true">
              <div className={styles.listRowFields} data-columns={fields.length}>
                {fields.map((field) => (
                  <span key={field.key} className={styles.columnLabel}>
                    {field.label}
                  </span>
                ))}
              </div>
              <span className={styles.listHeadSpacer} />
            </li>
          ) : null}
          {rows.map((row, index) => {
            const error = errors?.[index];
            const errorId = error ? `${idPrefix}-${index}-error` : undefined;
            return (
              <li key={index}>
                <div className={styles.listRow}>
                  <div className={styles.listRowFields} data-columns={fields.length}>
                    {fields.map((field) => (
                      <div key={field.key} className={styles.listCell}>
                        {labelled ? (
                          <span className={styles.cellLabel} aria-hidden="true">
                            {field.label}
                          </span>
                        ) : null}
                        <Input
                          id={rowFieldId(idPrefix, index, field.key)}
                          aria-label={fields.length === 1 ? `${field.label} ${index + 1}` : `${field.label}, ${noun} ${index + 1}`}
                          aria-describedby={errorId}
                          aria-invalid={error ? true : undefined}
                          placeholder={field.placeholder}
                          mono={field.mono}
                          list={field.list}
                          value={row[field.key] ?? ''}
                          onChange={(e) => update(index, field.key, e.target.value)}
                          size="sm"
                          autoComplete="off"
                          spellCheck={false}
                        />
                      </div>
                    ))}
                  </div>
                  <IconButton aria-label={`Remove ${noun} ${index + 1}`} icon={<IconClose />} size="sm" onClick={() => remove(index)} />
                </div>
                {error ? (
                  <p id={errorId} className={styles.rowError}>
                    {error}
                  </p>
                ) : null}
              </li>
            );
          })}
        </ol>
      )}
      <div>
        <Button ref={addRef} id={`${idPrefix}-add`} size="sm" icon={<IconPlus />} onClick={add}>
          {`Add ${noun}`}
        </Button>
      </div>
    </fieldset>
  );
}

/** One-column variant for lists of strings (repositories, namespaces, groups, policies). */
export function StringListEditor({
  values,
  onChange,
  label,
  placeholder,
  mono = true,
  list,
  ...rest
}: Omit<RowListEditorProps, 'rows' | 'onChange' | 'fields'> & {
  values: readonly string[];
  onChange: (values: string[]) => void;
  label: string;
  placeholder?: string;
  mono?: boolean;
  list?: string;
}) {
  return (
    <RowListEditor
      {...rest}
      rows={values.map((value) => ({ value }))}
      onChange={(rows) => onChange(rows.map((r) => r.value ?? ''))}
      fields={[{ key: 'value', label, placeholder, mono, list }]}
    />
  );
}

/** Suggestions for an input (`list` attribute); native, so keyboard and AT support is built in. */
export function Suggestions({ id, values }: { id: string; values: readonly string[] }) {
  return (
    <datalist id={id}>
      {values.map((v) => (
        <option key={v} value={v} />
      ))}
    </datalist>
  );
}

/** A tab id kept in the URL (`?<param>=`) so views are shareable; invalid values fall back. */
export function useTabParam<T extends string>(ids: readonly T[], fallback: T, param = 'tab'): [T, (next: string) => void] {
  const [params, setParams] = useSearchParams();
  const raw = params.get(param);
  const value = ids.find((id) => id === raw) ?? fallback;
  const set = (next: string) => {
    setParams(
      (prev) => {
        const copy = new URLSearchParams(prev);
        if (next === fallback) copy.delete(param);
        else copy.set(param, next);
        return copy;
      },
      { replace: true },
    );
  };
  return [value, set];
}

/** Map → rows for the label/annotation editors, and back (empty keys dropped). */
export function mapToRows(map: Record<string, string> | undefined): Row[] {
  return Object.entries(map ?? {}).map(([key, value]) => ({ key, value }));
}

export function rowsToMap(rows: readonly Row[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows) {
    const key = (r.key ?? '').trim();
    if (key !== '') out[key] = r.value ?? '';
  }
  return out;
}

/** `value`, or `fallback` when it is undefined or empty. */
export function orDefault(value: string | undefined, fallback: string): string {
  return value === undefined || value === '' ? fallback : value;
}

export function nonEmpty(values: readonly string[]): string[] {
  return values.map((v) => v.trim()).filter((v) => v !== '');
}

import { useId, useState } from 'react';
import { Button, IconButton, Input, TextArea } from '../../../design';
import { IconClose, IconPlus } from '../../../design/icons';
import styles from './Parameters.module.css';
import { linesToList, listToLines, type KV } from './specEdit';

/** Editable key/value rows (labels, annotations, common labels, env…). */
export function KeyValueEditor({
  legend,
  rows,
  onChange,
  errors = {},
  keyLabel = 'Key',
  valueLabel = 'Value',
  itemLabel = 'entry',
  mono = true,
}: {
  legend: string;
  rows: readonly KV[];
  onChange: (rows: KV[]) => void;
  errors?: Record<number, string>;
  keyLabel?: string;
  valueLabel?: string;
  itemLabel?: string;
  mono?: boolean;
}) {
  const id = useId();
  const update = (i: number, patch: Partial<KV>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <fieldset className={styles.fieldset}>
      <legend>{legend}</legend>
      {rows.length === 0 ? <p className={styles.muted}>None.</p> : null}
      <ul className={styles.rows}>
        {rows.map((row, i) => {
          const errorId = errors[i] ? `${id}-err-${i}` : undefined;
          return (
            <li key={i} className={styles.row}>
              <Input
                mono={mono}
                size="sm"
                aria-label={`${keyLabel} of ${itemLabel} ${i + 1}`}
                value={row.key}
                aria-invalid={errors[i] ? true : undefined}
                aria-describedby={errorId}
                onChange={(e) => update(i, { key: e.target.value })}
              />
              <Input mono={mono} size="sm" aria-label={`${valueLabel} of ${itemLabel} ${i + 1}`} value={row.value} onChange={(e) => update(i, { value: e.target.value })} />
              <IconButton size="sm" aria-label={`Remove ${itemLabel} ${i + 1}`} icon={<IconClose />} onClick={() => onChange(rows.filter((_r, j) => j !== i))} />
              {errors[i] ? (
                <p id={errorId} className={styles.rowError}>
                  {errors[i]}
                </p>
              ) : null}
            </li>
          );
        })}
      </ul>
      <Button size="sm" icon={<IconPlus />} onClick={() => onChange([...rows, { key: '', value: '' }])}>
        Add {itemLabel}
      </Button>
    </fieldset>
  );
}

/** One value per line (value files, images, components, libraries). Remount (key) to reset. */
export function LinesField({
  id,
  value,
  onChange,
  rows = 3,
  'aria-label': ariaLabel,
}: {
  id?: string;
  value: readonly string[] | undefined;
  onChange: (next: string[] | undefined) => void;
  rows?: number;
  'aria-label'?: string;
}) {
  const [text, setText] = useState(() => listToLines(value));
  return (
    <TextArea
      id={id}
      mono
      rows={rows}
      aria-label={ariaLabel}
      value={text}
      onChange={(e) => {
        setText(e.target.value);
        const list = linesToList(e.target.value);
        onChange(list.length > 0 ? list : undefined);
      }}
    />
  );
}

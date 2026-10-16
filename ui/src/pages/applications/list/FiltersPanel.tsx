import { useState } from 'react';
import type { Application } from '../../../api/types';
import { Button, Checkbox, FormField, HEALTH_STATES, Input, SYNC_STATES } from '../../../design';
import { IconPlus } from '../../../design/icons';
import styles from './List.module.css';
import { dimensionCounts, type AppFilters, type Dimension } from './filters';

const SYNC_LABEL: Record<string, string> = { Synced: 'Synced', OutOfSync: 'Out of sync', Unknown: 'Unknown' };

interface GroupDef {
  dimension: Dimension;
  legend: string;
  /** Always-listed values (in order); other values present in the data are appended. */
  fixed?: readonly string[];
  label?: (value: string) => string;
  /** Accepts glob patterns typed by the user. */
  patterns?: boolean;
}

const GROUPS: readonly GroupDef[] = [
  { dimension: 'sync', legend: 'Sync status', fixed: SYNC_STATES, label: (v) => SYNC_LABEL[v] ?? v },
  { dimension: 'health', legend: 'Health status', fixed: HEALTH_STATES },
  { dimension: 'operation', legend: 'Operation', fixed: ['Running', 'Succeeded', 'Failed', 'Error', 'Terminating', 'Deleting'] },
  { dimension: 'autoSync', legend: 'Auto-sync', fixed: ['Enabled', 'Disabled'] },
  { dimension: 'projects', legend: 'Projects' },
  { dimension: 'clusters', legend: 'Clusters', patterns: true },
  { dimension: 'namespaces', legend: 'Namespaces', patterns: true },
  { dimension: 'repos', legend: 'Repositories', patterns: true },
  { dimension: 'targetRevisions', legend: 'Target revisions', patterns: true },
];

function SelectorInput({ label, help, onAdd }: { label: string; help: string; onAdd: (selector: string) => void }) {
  const [value, setValue] = useState('');
  const add = () => {
    const selector = value.trim();
    if (!selector) return;
    onAdd(selector);
    setValue('');
  };
  return (
    <>
      <FormField label={label} help={help}>
        <Input
          size="sm"
          mono
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              add();
            }
          }}
        />
      </FormField>
      <Button size="sm" icon={<IconPlus />} onClick={add} disabled={!value.trim()}>
        Add {label.toLowerCase().replace(' selector', '')} filter
      </Button>
    </>
  );
}

function PatternInput({ legend, onAdd }: { legend: string; onAdd: (pattern: string) => void }) {
  const [value, setValue] = useState('');
  const add = () => {
    const pattern = value.trim();
    if (!pattern) return;
    onAdd(pattern);
    setValue('');
  };
  return (
    <div className={styles.inlineLabel}>
      <Input
        size="sm"
        mono
        aria-label={`${legend} pattern`}
        placeholder="pattern, e.g. prod-*"
        value={value}
        onChange={(e) => setValue(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            add();
          }
        }}
      />
      <Button size="sm" aria-label={`Add ${legend.toLowerCase()} pattern`} icon={<IconPlus />} onClick={add} disabled={!value.trim()}>
        Add
      </Button>
    </div>
  );
}

/** Filter groups with counts computed against the other active filters (parity API-089). */
export function FiltersPanel({
  id,
  apps,
  filters,
  favorites,
  onChange,
}: {
  id: string;
  apps: readonly Application[];
  filters: AppFilters;
  favorites: ReadonlySet<string>;
  onChange: (filters: AppFilters) => void;
}) {
  const toggle = (dimension: Dimension, value: string, on: boolean) => {
    const current = filters[dimension] as string[];
    const next = on ? [...current, value] : current.filter((v) => v !== value);
    onChange({ ...filters, [dimension]: next });
  };
  const addSelector = (key: 'labels' | 'annotations', selector: string) => {
    if (!filters[key].includes(selector)) onChange({ ...filters, [key]: [...filters[key], selector] });
  };
  return (
    <div id={id} className={styles.filtersPanel} role="region" aria-label="Filter options">
      <fieldset className={styles.filterGroup}>
        <legend>Favorites</legend>
        <Checkbox label="Favorites only" checked={filters.favorites} onChange={(e) => onChange({ ...filters, favorites: e.target.checked })} />
      </fieldset>
      {GROUPS.map((group) => {
        const counts = dimensionCounts(apps, filters, favorites, group.dimension);
        const selected = filters[group.dimension] as string[];
        const values = [...new Set([...(group.fixed ?? []), ...[...counts.keys()].sort((a, b) => a.localeCompare(b)), ...selected])].filter(Boolean);
        return (
          <fieldset key={group.dimension} className={styles.filterGroup}>
            <legend>{group.legend}</legend>
            <div className={styles.filterOptions}>
              {values.length === 0 ? <span className={styles.filterNone}>No values</span> : null}
              {values.map((value) => (
                <Checkbox
                  key={value}
                  label={
                    <>
                      {group.label?.(value) ?? value} <span className={styles.count}>({counts.get(value) ?? 0})</span>
                    </>
                  }
                  checked={selected.includes(value)}
                  onChange={(e) => toggle(group.dimension, value, e.target.checked)}
                />
              ))}
            </div>
            {group.patterns ? <PatternInput legend={group.legend} onAdd={(p) => !selected.includes(p) && toggle(group.dimension, p, true)} /> : null}
          </fieldset>
        );
      })}
      <fieldset className={styles.filterGroup}>
        <legend>Labels</legend>
        <SelectorInput
          label="Label selector"
          help="key, key=value, key!=value, !key, key in (a,b). Several selectors must all match."
          onAdd={(selector) => addSelector('labels', selector)}
        />
      </fieldset>
      <fieldset className={styles.filterGroup}>
        <legend>Annotations</legend>
        <SelectorInput label="Annotation selector" help="Same syntax as labels, matched against annotations." onAdd={(selector) => addSelector('annotations', selector)} />
      </fieldset>
    </div>
  );
}

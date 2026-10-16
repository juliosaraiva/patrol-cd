import { FilterBar, FilterChips, HealthStatus, SearchInput, Switch, SyncStatus, type FilterChip } from '../../../../design';
import { EMPTY_FILTERS, type ResourceFilters } from '../model/filters';
import { MultiSelectFilter } from '../shared/MultiSelectFilter';
import styles from './Views.module.css';

export interface ResourceFiltersBarProps {
  filters: ResourceFilters;
  onChange: (next: ResourceFilters) => void;
  options: { kinds: string[]; namespaces: string[]; health: string[] };
  /** The project monitors orphaned resources, so the toggle is offered. */
  orphanedAvailable: boolean;
  orphanedCount: number;
}

const SYNC_OPTIONS = ['Synced', 'OutOfSync'];
const SYNC_TEXT: Record<string, string> = { Synced: 'Synced', OutOfSync: 'Out of sync' };

/** Name search, kind/sync/health/namespace menus, orphaned and hook toggles, active chips. */
export function ResourceFiltersBar({ filters, onChange, options, orphanedAvailable, orphanedCount }: ResourceFiltersBarProps) {
  const set = (patch: Partial<ResourceFilters>) => onChange({ ...filters, ...patch });
  const chips: FilterChip[] = [
    ...(filters.name.trim() ? [{ id: 'name', label: `Name: ${filters.name.trim()}` }] : []),
    ...filters.kinds.map((k) => ({ id: `kind:${k}`, label: `Kind: ${k}` })),
    ...filters.sync.map((s) => ({ id: `sync:${s}`, label: `Sync: ${SYNC_TEXT[s] ?? s}` })),
    ...filters.health.map((h) => ({ id: `health:${h}`, label: `Health: ${h}` })),
    ...filters.namespaces.map((n) => ({ id: `namespace:${n}`, label: `Namespace: ${n}` })),
  ];
  const removeChip = (id: string) => {
    const [field, value = ''] = id.split(/:(.*)/s);
    switch (field) {
      case 'name':
        set({ name: '' });
        break;
      case 'kind':
        set({ kinds: filters.kinds.filter((k) => k !== value) });
        break;
      case 'sync':
        set({ sync: filters.sync.filter((k) => k !== value) });
        break;
      case 'health':
        set({ health: filters.health.filter((k) => k !== value) });
        break;
      case 'namespace':
        set({ namespaces: filters.namespaces.filter((k) => k !== value) });
        break;
      default:
        break;
    }
  };
  const healthValues = [...new Set([...options.health, ...filters.health])];

  return (
    <FilterBar
      label="Resource filters"
      chips={
        <FilterChips
          chips={chips}
          onRemove={removeChip}
          onClearAll={() => onChange({ ...EMPTY_FILTERS, showOrphaned: filters.showOrphaned, showHooks: filters.showHooks })}
          label="Active resource filters"
        />
      }
    >
      <SearchInput
        className={styles.search}
        size="md"
        value={filters.name}
        onValueChange={(name) => set({ name })}
        placeholder="Filter by name (* wildcards)"
        aria-label="Filter resources by name"
      />
      <MultiSelectFilter
        label="Kind"
        options={[...new Set([...options.kinds, ...filters.kinds])].map((k) => ({ value: k, label: k }))}
        selected={filters.kinds}
        onChange={(kinds) => set({ kinds })}
      />
      <MultiSelectFilter
        label="Sync status"
        options={SYNC_OPTIONS.map((s) => ({ value: s, label: SYNC_TEXT[s] ?? s, adornment: <SyncStatus status={s} hideLabel /> }))}
        selected={filters.sync}
        onChange={(sync) => set({ sync })}
      />
      <MultiSelectFilter
        label="Health"
        options={healthValues.map((h) => ({ value: h, label: h, adornment: <HealthStatus status={h} hideLabel /> }))}
        selected={filters.health}
        onChange={(health) => set({ health })}
      />
      <MultiSelectFilter
        label="Namespace"
        options={[...new Set([...options.namespaces, ...filters.namespaces])].map((n) => ({ value: n, label: n }))}
        selected={filters.namespaces}
        onChange={(namespaces) => set({ namespaces })}
      />
      {orphanedAvailable ? (
        <Switch
          checked={filters.showOrphaned}
          onChange={(showOrphaned) => set({ showOrphaned })}
          label={`Orphaned resources (${orphanedCount})`}
        />
      ) : null}
      <Switch checked={filters.showHooks} onChange={(showHooks) => set({ showHooks })} label="Hooks" />
    </FilterBar>
  );
}

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { useApplications, useRefreshApplication } from '../../api/queries/applications';
import type { Application } from '../../api/types';
import { Button, ButtonLink, EmptyState, FilterBar, FilterChips, PageHeader, Pagination, SearchInput, Select, Skeleton, useToast } from '../../design';
import { IconBook, IconFilter, IconPlus } from '../../design/icons';
import { readStorage, writeStorage } from '../../lib/storage';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import { ErrorBanner } from '../../shell/errors';
import { CreateApplicationPanel } from '../create';
import { DeleteDialog } from './delete/DeleteDialog';
import { ApplicationsSummary } from './list/ApplicationsSummary';
import { ApplicationsTable } from './list/ApplicationsTable';
import { ApplicationTiles, type ItemActions } from './list/ApplicationTiles';
import { BulkActionDialog, type BulkKind } from './list/BulkActions';
import { FiltersPanel } from './list/FiltersPanel';
import { HealthBar } from './list/HealthBar';
import {
  applyFilters,
  filterChips,
  hasActiveFilters,
  parseFilters,
  parseSort,
  parseView,
  removeChip,
  sortApps,
  writeFilters,
  EMPTY_FILTERS,
  type AppFilters,
  type Dimension,
  type ViewMode,
} from './list/filters';
import { digitOf, isEditableTarget, isModalOpen, measureColumns, nextTileCursor } from './list/keyboard';
import styles from './list/List.module.css';
import { appDetailsPath, appKey } from './shared/app';
import { useFavorites } from './shared/favorites';

const PAGE_SIZES = [10, 25, 50, 100, 500] as const;
export const PAGE_SIZE_KEY = 'patrol.apps.pageSize';
export const FILTERS_OPEN_KEY = 'patrol.apps.filtersOpen';
export const VIEW_KEY = 'patrol.apps.view';
export const HEALTH_BAR_KEY = 'patrol.apps.healthBar';

function storedPageSize(): number {
  const n = Number(readStorage(PAGE_SIZE_KEY));
  return (PAGE_SIZES as readonly number[]).includes(n) ? n : 25;
}

const VIEWS: { id: ViewMode; label: string }[] = [
  { id: 'tiles', label: 'Tiles' },
  { id: 'list', label: 'List' },
  { id: 'summary', label: 'Summary' },
];

/** /applications — list, search, filters, bulk sync/refresh/delete (parity API-088..090, API-133..135). */
export function Component() {
  useDocumentTitle('Applications');
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const creating = params.has('new');
  const filters = useMemo(() => parseFilters(params), [params]);
  const view = parseView(params.get('view') ?? readStorage(VIEW_KEY));
  const sort = parseSort(params.get('sort'));
  const [pageSize, setPageSize] = useState(storedPageSize);
  const [filtersOpen, setFiltersOpen] = useState(() => readStorage(FILTERS_OPEN_KEY) === 'true');
  const [healthBar, setHealthBar] = useState(() => readStorage(HEALTH_BAR_KEY) !== 'false');
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [bulk, setBulk] = useState<{ kind: BulkKind; apps: Application[] } | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Application | null>(null);
  const [refreshing, setRefreshing] = useState<string | null>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const tilesRef = useRef<HTMLUListElement>(null);
  const tableRef = useRef<HTMLDivElement>(null);
  const { favorites, toggle: toggleFavorite } = useFavorites();
  const refresh = useRefreshApplication();
  const { toast } = useToast();

  const query = useApplications();
  const all = useMemo(() => query.data?.items ?? [], [query.data]);
  const filtered = useMemo(() => sortApps(applyFilters(all, filters, favorites), sort), [all, filters, favorites, sort]);
  const pageCount = Math.max(1, Math.ceil(filtered.length / pageSize));
  const page = Math.min(Math.max(1, Number(params.get('page')) || 1), pageCount);
  const pageApps = useMemo(() => filtered.slice((page - 1) * pageSize, page * pageSize), [filtered, page, pageSize]);
  const selectedApps = useMemo(() => all.filter((a) => selected.has(appKey(a))), [all, selected]);

  // Keyboard cursor, scoped to what is shown: changing page, view or filters clears it.
  const cursorScope = `${view}|${page}|${pageSize}|${params.toString()}`;
  const [cursorState, setCursorState] = useState({ scope: cursorScope, index: -1 });
  const cursor = cursorState.scope === cursorScope && cursorState.index < pageApps.length ? cursorState.index : -1;
  const setCursor = useCallback((index: number) => setCursorState({ scope: cursorScope, index }), [cursorScope]);

  const updateParams = (mutate: (p: URLSearchParams) => URLSearchParams | void, replace = false) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        return mutate(next) ?? next;
      },
      { replace },
    );
  const setFilters = (next: AppFilters, replace = false) =>
    updateParams((p) => {
      const written = writeFilters(p, next);
      written.delete('page');
      return written;
    }, replace);

  const open = useCallback((app: Application) => void navigate(appDetailsPath(app)), [navigate]);

  // "/" focuses this page's search (before the shell's global search); disabled while creating.
  useEffect(() => {
    if (creating) return undefined;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey || isEditableTarget(e.target) || isModalOpen()) return;
      e.preventDefault();
      searchRef.current?.focus();
      searchRef.current?.select();
    };
    document.addEventListener('keydown', onKeyDown, true);
    return () => document.removeEventListener('keydown', onKeyDown, true);
  }, [creating]);

  // Tiles: arrows move the cursor, Enter opens, Esc clears, digits jump. List: ↓ enters the table.
  useEffect(() => {
    if (creating || view === 'summary') return undefined;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || isEditableTarget(e.target) || isModalOpen()) return;
      const target = e.target instanceof HTMLElement ? e.target : null;
      if (view === 'list') {
        const inTable = target?.closest('tr[data-row-index]');
        if (e.key === 'ArrowDown' && !inTable) {
          const first = tableRef.current?.querySelector<HTMLElement>('tr[data-row-index="0"]');
          if (first) {
            e.preventDefault();
            first.focus();
          }
        } else if (e.key === 'Escape' && inTable) {
          e.preventDefault();
          searchRef.current?.focus();
        }
        return;
      }
      const digit = digitOf(e);
      if (digit !== null && !target?.closest('[role="menu"]')) {
        if (digit >= 1 && digit <= pageApps.length) {
          e.preventDefault();
          setCursor(digit - 1);
          focusTile(digit - 1);
        }
        return;
      }
      if (e.key === 'Enter') {
        if (cursor < 0 || target?.closest('a, button, input, select, textarea, [role="menuitem"]')) return;
        const app = pageApps[cursor];
        if (app) {
          e.preventDefault();
          open(app);
        }
        return;
      }
      if (e.key === 'Escape') {
        if (cursor >= 0) {
          e.preventDefault();
          setCursor(-1);
        }
        return;
      }
      if (target?.closest('[role="menu"]')) return;
      const next = nextTileCursor(e.key, cursor, pageApps.length, measureColumns(tilesRef.current));
      if (next === null) return;
      e.preventDefault();
      if (next !== cursor) {
        setCursor(next);
        focusTile(next);
      }
    };
    const focusTile = (index: number) => {
      tilesRef.current?.querySelector<HTMLElement>(`[data-tile-index="${index}"] [data-tile-link]`)?.focus();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [creating, view, cursor, pageApps, open, setCursor]);

  const actions: ItemActions = useMemo(
    () => ({
      refreshing,
      onSync: (app) => void navigate(appDetailsPath(app, 'panel=sync')),
      onDelete: setDeleteTarget,
      onRefresh: (app) => {
        setRefreshing(appKey(app));
        refresh.mutate(
          { name: app.metadata.name ?? '', scope: { appNamespace: app.metadata.namespace, project: app.spec.project }, type: 'normal' },
          {
            onSuccess: () => toast({ tone: 'success', title: `Refreshed ${app.metadata.name ?? ''}` }),
            onError: (err) => toast({ tone: 'error', title: `Could not refresh ${app.metadata.name ?? ''}`, description: err.message }),
            onSettled: () => setRefreshing(null),
          },
        );
      },
    }),
    [refreshing, navigate, refresh, toast],
  );

  const toggleSelect = (key: string, on: boolean) => {
    const next = new Set(selected);
    if (on) next.add(key);
    else next.delete(key);
    setSelected(next);
  };

  const applyDimension = (dimension: Dimension, value: string) => {
    const next = { ...filters, [dimension]: [value] };
    updateParams((p) => {
      const written = writeFilters(p, next);
      written.delete('page');
      written.set('view', 'tiles');
      return written;
    });
  };

  const chips = filterChips(filters);
  const loading = query.isPending;
  const noApps = !loading && !query.isError && all.length === 0;
  const filteredEmpty = !loading && all.length > 0 && filtered.length === 0;
  const filtersId = 'applications-filters';

  return (
    <>
      <PageHeader
        title="Applications"
        description="Applications deployed from Git, Helm and OCI sources, with live sync and health status."
        actions={
          <Button variant="primary" icon={<IconPlus />} onClick={() => updateParams((p) => void p.set('new', '1'))}>
            New application
          </Button>
        }
      />
      <FilterBar
        label="Search and filters"
        chips={
          <FilterChips
            chips={chips}
            onRemove={(id) => setFilters(removeChip(filters, id))}
            onClearAll={() => setFilters({ ...EMPTY_FILTERS })}
          />
        }
      >
        <div className={styles.toolbar} style={{ flex: 1 }}>
          <SearchInput
            ref={searchRef}
            className={styles.search}
            aria-label="Filter applications by name or namespace"
            placeholder="Filter by name or namespace"
            shortcut="/"
            value={filters.search}
            onValueChange={(search) => setFilters({ ...filters, search }, true)}
          />
          <Button
            icon={<IconFilter />}
            aria-expanded={filtersOpen}
            aria-controls={filtersId}
            onClick={() => {
              writeStorage(FILTERS_OPEN_KEY, String(!filtersOpen));
              setFiltersOpen(!filtersOpen);
            }}
          >
            Filters{chips.length > 0 ? ` (${chips.length})` : ''}
          </Button>
          <span className={styles.inlineLabel}>
            <label htmlFor="applications-sort">Sort</label>
            <Select
              id="applications-sort"
              size="sm"
              value={sort}
              onChange={(e) => updateParams((p) => void p.set('sort', e.target.value))}
              options={[
                { value: 'name', label: 'Name' },
                { value: 'created', label: 'Created (newest)' },
                { value: 'synced', label: 'Last synced (newest)' },
              ]}
            />
          </span>
          <div role="group" aria-label="View" className={styles.segmented}>
            {VIEWS.map((v) => (
              <button
                key={v.id}
                type="button"
                aria-pressed={view === v.id}
                onClick={() => {
                  writeStorage(VIEW_KEY, v.id);
                  updateParams((p) => void p.set('view', v.id));
                }}
              >
                {v.label}
              </button>
            ))}
          </div>
        </div>
      </FilterBar>
      {filtersOpen ? <FiltersPanel id={filtersId} apps={all} filters={filters} favorites={favorites} onChange={(f) => setFilters(f)} /> : null}

      {selectedApps.length > 0 ? (
        <div className={styles.bulkBar} role="region" aria-label="Bulk actions">
          <span>
            {selectedApps.length} selected
          </span>
          <Button size="sm" onClick={() => setBulk({ kind: 'sync', apps: selectedApps })}>
            Sync selected
          </Button>
          <Button size="sm" onClick={() => setBulk({ kind: 'refresh', apps: selectedApps })}>
            Refresh selected
          </Button>
          <Button size="sm" variant="danger" onClick={() => setBulk({ kind: 'delete', apps: selectedApps })}>
            Delete selected
          </Button>
          <span className={styles.bulkSpacer} />
          <Button size="sm" variant="ghost" onClick={() => setSelected(new Set())}>
            Clear selection
          </Button>
        </div>
      ) : null}

      {query.isError ? <ErrorBanner error={query.error} onRetry={() => void query.refetch()} title="Applications could not be loaded" /> : null}

      {loading ? (
        <div aria-busy="true" aria-label="Loading applications" className={styles.results}>
          <ul className={styles.tiles}>
            {Array.from({ length: 6 }, (_, i) => (
              <li key={i} className={styles.tile}>
                <Skeleton height={16} width="50%" />
                <Skeleton height={12} />
                <Skeleton height={12} width="70%" />
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {noApps ? (
        <EmptyState
          icon={<IconBook size={24} />}
          title="No applications yet"
          action={
            <Button variant="primary" icon={<IconPlus />} onClick={() => updateParams((p) => void p.set('new', '1'))}>
              New application
            </Button>
          }
          secondaryAction={<ButtonLink to="/help">Docs and help</ButtonLink>}
        >
          <p>
            An application deploys manifests from a Git, Helm or OCI source to a cluster and namespace, and keeps them in sync. Create one to
            start managing resources; the docs explain sources, projects and sync policies.
          </p>
        </EmptyState>
      ) : null}

      {filteredEmpty ? (
        <EmptyState title="No applications match the filters" action={<Button onClick={() => setFilters({ ...EMPTY_FILTERS })}>Clear filters</Button>}>
          <p>{all.length} applications are hidden by the current search and filters.</p>
        </EmptyState>
      ) : null}

      {!loading && filtered.length > 0 ? (
        <div className={styles.results}>
          <div className={styles.meta}>
            <span role="status">
              {filtered.length === all.length ? `${all.length} applications` : `${filtered.length} of ${all.length} applications`}
              {hasActiveFilters(filters) ? ' match the filters' : ''}
            </span>
            <span className={styles.toolbarGroup}>
              <Button
                size="sm"
                variant="ghost"
                aria-pressed={healthBar}
                onClick={() => {
                  writeStorage(HEALTH_BAR_KEY, String(!healthBar));
                  setHealthBar(!healthBar);
                }}
              >
                Health bar
              </Button>
              {view !== 'summary' ? (
                <Button size="sm" variant="ghost" onClick={() => setSelected(new Set(filtered.map(appKey)))}>
                  Select all {filtered.length}
                </Button>
              ) : null}
            </span>
          </div>
          {healthBar && view !== 'summary' ? <HealthBar apps={filtered} /> : null}
          {view === 'summary' ? (
            <ApplicationsSummary apps={filtered} onFilter={applyDimension} />
          ) : view === 'list' ? (
            <ApplicationsTable
              apps={pageApps}
              selected={selected}
              onSelectionChange={(keys) => setSelected(new Set([...[...selected].filter((k) => !pageApps.some((a) => appKey(a) === k)), ...keys]))}
              favorites={favorites}
              onToggleFavorite={toggleFavorite}
              onOpen={open}
              actions={actions}
              containerRef={tableRef}
            />
          ) : (
            <ApplicationTiles
              apps={pageApps}
              selected={selected}
              onToggleSelect={toggleSelect}
              favorites={favorites}
              onToggleFavorite={toggleFavorite}
              cursor={cursor}
              onCursorChange={setCursor}
              actions={actions}
              containerRef={tilesRef}
            />
          )}
          {view !== 'summary' ? (
            <Pagination
              page={page}
              pageCount={pageCount}
              totalItems={filtered.length}
              onPageChange={(p) => updateParams((next) => void next.set('page', String(p)))}
              pageSize={pageSize}
              pageSizeOptions={PAGE_SIZES}
              onPageSizeChange={(size) => {
                writeStorage(PAGE_SIZE_KEY, String(size));
                setPageSize(size);
                updateParams((next) => void next.delete('page'));
              }}
            />
          ) : null}
        </div>
      ) : null}

      {bulk ? <BulkActionDialog kind={bulk.kind} apps={bulk.apps} onClose={() => setBulk(null)} /> : null}
      {deleteTarget ? <DeleteDialog app={deleteTarget} open onClose={() => setDeleteTarget(null)} /> : null}
      <CreateApplicationPanel open={creating} onClose={() => updateParams((p) => void p.delete('new'))} />
    </>
  );
}

/**
 * Mount points for UI extensions. Pages owned by other areas render these where upstream shows
 * extensions (see docs/architecture/components/ui.md, "UI extensions"):
 *
 * - resource details panel: useResourceExtensionTabs() (tab items) or <ResourceExtensionTabs>
 * - application details view switcher: useAppViewExtensions() + <AppViewExtensions view=...>
 * - application status panel: <StatusPanelExtensions>
 * - application details action bar: <TopBarExtensionActions>
 * - shell navigation: useSystemExtensionNavItems(); the catch-all route: <SystemExtensionRoutes>
 *
 * Every extension component renders inside ExtensionBoundary. Slots re-render when an
 * extension registers after the first render.
 */
import { useEffect, useMemo, useSyncExternalStore, type ReactNode } from 'react';
import { useLocation, useSearchParams } from 'react-router';
import type { Application, ApplicationTree } from '../api/types';
import { Button, Drawer, PageHeader, Spinner, Tabs, type TabItem } from '../design';
import { useDocumentTitle } from '../lib/useDocumentTitle';
import { ExtensionBoundary } from './ExtensionBoundary';
import { IconExtension } from './icon';
import { extensionsLoadState, loadExtensions, subscribeExtensionsLoadState } from './loader';
import {
  extensionsVersion,
  getActionMenuExtensions,
  getAppViewExtensions,
  getResourceTabs,
  getStatusPanelExtensions,
  getSystemExtensions,
  subscribeExtensions,
} from './registry';
import type { AppViewExtension, KubernetesObject, StatusPanelExtension, SystemLevelExtension, TopBarActionMenuExt } from './types';
import styles from './slots.module.css';

const EMPTY_TREE: ApplicationTree = { nodes: [] };

/** Re-renders the caller on every registration. */
function useRegistryVersion(): number {
  return useSyncExternalStore(subscribeExtensions, extensionsVersion, extensionsVersion);
}

/**
 * Safety net when the bootstrap did not call initExtensions(): the first mounted slot starts
 * loading. Skipped under vitest (jsdom does not execute external scripts).
 */
function useEnsureExtensionsLoaded(): void {
  useEffect(() => {
    if (import.meta.env.MODE !== 'test') void loadExtensions();
  }, []);
}

/** URL search parameter holding the open extension flyout (upstream `extension=<id>`). */
export const EXTENSION_FLYOUT_PARAM = 'extension';

function useFlyout(): [string, (id: string) => void] {
  const [params, setParams] = useSearchParams();
  const open = params.get(EXTENSION_FLYOUT_PARAM) ?? '';
  const set = (id: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (id) next.set(EXTENSION_FLYOUT_PARAM, id);
        else next.delete(EXTENSION_FLYOUT_PARAM);
        return next;
      },
      { replace: true },
    );
  return [open, set];
}

// ---------------------------------------------------------------------------------------------
// Resource tabs (registerResourceExtension)

export interface ResourceExtensionContext {
  /** API group of the selected resource ("" for the core group, "cd.patrol.example" for Applications). */
  group: string;
  kind: string;
  application: Application;
  /** Live object (GET /applications/{name}/resource); for the Application itself, the Application. */
  resource: KubernetesObject;
  tree: ApplicationTree | undefined;
}

/**
 * Application-level tabs: extensions written for Argo CD register group "argoproj.io" and kind
 * "Application"; Patrol matches both that and its own group for Application resources.
 */
const APPLICATION_GROUPS = ['argoproj.io', 'cd.patrol.example'];

function resourceTabs(group: string, kind: string) {
  if (kind === 'Application' && APPLICATION_GROUPS.includes(group)) {
    const seen = new Set<unknown>();
    return APPLICATION_GROUPS.flatMap((g) => getResourceTabs(g, kind))
      .filter((ext) => (seen.has(ext) ? false : (seen.add(ext), true)))
      .sort((a, b) => a.title.localeCompare(b.title));
  }
  return getResourceTabs(group, kind);
}

/** Tab items for the resource details panel; ids are `extension-<index>-<title>`. */
export function useResourceExtensionTabs({ group, kind, application, resource, tree }: ResourceExtensionContext): TabItem[] {
  useEnsureExtensionsLoaded();
  const version = useRegistryVersion();
  return useMemo(
    () =>
      resourceTabs(group, kind).map((ext, index) => {
        const Ext = ext.component;
        return {
          id: `extension-${index}-${ext.title}`,
          label: (
            <span className={styles.tabLabel}>
              <IconExtension />
              {ext.title}
            </span>
          ),
          content: (
            <ExtensionBoundary message={`Something went wrong with Extension for ${kind}`}>
              <Ext application={application} resource={resource} tree={tree ?? EMPTY_TREE} />
            </ExtensionBoundary>
          ),
        };
      }),
    // version: registrations change the result without changing the inputs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [group, kind, application, resource, tree, version],
  );
}

/** Standalone tab list of the matching resource extensions; renders nothing when none match. */
export function ResourceExtensionTabs(props: ResourceExtensionContext & { label?: string }) {
  const items = useResourceExtensionTabs(props);
  if (items.length === 0) return null;
  return <Tabs label={props.label ?? `Extensions for ${props.kind}`} items={items} />;
}

// ---------------------------------------------------------------------------------------------
// Application views (registerAppViewExtension)

/** App view extensions available for `application` (shouldDisplay applied, errors = hidden). */
export function useAppViewExtensions(application: Application | undefined): AppViewExtension[] {
  useEnsureExtensionsLoaded();
  const version = useRegistryVersion();
  return useMemo(
    () =>
      application
        ? getAppViewExtensions().filter((ext) => {
            try {
              return ext.shouldDisplay(application);
            } catch (err) {
              console.error(`Extension "${ext.title}" shouldDisplay failed:`, err);
              return false;
            }
          })
        : [],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [application, version],
  );
}

/**
 * Renders the app view extension whose title equals `view` (the `view` URL parameter of the
 * application details page), or nothing when `view` names a built-in view.
 */
export function AppViewExtensions({ application, tree, view }: { application: Application; tree: ApplicationTree | undefined; view: string }) {
  const extensions = useAppViewExtensions(application);
  const ext = extensions.find((e) => e.title === view);
  if (!ext) return null;
  const Ext = ext.component;
  return (
    <section aria-label={ext.title} className={styles.view}>
      <ExtensionBoundary message={`Something went wrong with Extension ${ext.title}`}>
        <Ext application={application} tree={tree ?? EMPTY_TREE} />
      </ExtensionBoundary>
    </section>
  );
}

// ---------------------------------------------------------------------------------------------
// Flyouts shared by status panel and top-bar extensions

function FlyoutDrawer({
  ext,
  open,
  onClose,
  application,
  tree,
}: {
  ext: StatusPanelExtension | TopBarActionMenuExt;
  open: boolean;
  onClose: () => void;
  application: Application;
  tree: ApplicationTree | undefined;
}) {
  const Flyout = ext.flyout;
  if (!Flyout) return null;
  return (
    <Drawer open={open} onClose={onClose} title={ext.title} width="min(720px, 100vw)">
      <ExtensionBoundary message={`Something went wrong with Extension ${ext.title}`}>
        <Flyout application={application} tree={tree ?? EMPTY_TREE} />
      </ExtensionBoundary>
    </Drawer>
  );
}

// ---------------------------------------------------------------------------------------------
// Status panel (registerStatusPanelExtension)

/** Status panel items; `openFlyout()` opens the extension's flyout (URL `extension=<id>`). */
export function StatusPanelExtensions({ application, tree, className }: { application: Application; tree: ApplicationTree | undefined; className?: string }) {
  useEnsureExtensionsLoaded();
  useRegistryVersion();
  const [openId, setOpenId] = useFlyout();
  const extensions = getStatusPanelExtensions();
  if (extensions.length === 0) return null;
  return (
    <>
      {extensions.map((ext) => {
        const Ext = ext.component;
        return (
          <div key={ext.id} role="group" aria-label={ext.title} className={[styles.statusItem, className].filter(Boolean).join(' ')}>
            <ExtensionBoundary message={`Something went wrong with Extension ${ext.title}`}>
              <Ext application={application} openFlyout={() => setOpenId(ext.id)} />
            </ExtensionBoundary>
          </div>
        );
      })}
      {extensions.map((ext) => (
        <FlyoutDrawer
          key={`flyout-${ext.id}`}
          ext={ext}
          open={openId === ext.id}
          onClose={() => setOpenId('')}
          application={application}
          tree={tree}
        />
      ))}
    </>
  );
}

// ---------------------------------------------------------------------------------------------
// Top bar actions (registerTopBarActionMenuExt)

export type TopBarPlacement = 'middle' | 'end' | 'all';

function useTopBarExtensions(application: Application, placement: TopBarPlacement): TopBarActionMenuExt[] {
  useEnsureExtensionsLoaded();
  const version = useRegistryVersion();
  return useMemo(
    () =>
      getActionMenuExtensions().filter((ext) => {
        if (placement === 'middle' && !ext.isMiddle) return false;
        if (placement === 'end' && ext.isMiddle) return false;
        try {
          return ext.shouldDisplay(application);
        } catch (err) {
          console.error(`Extension "${ext.title}" shouldDisplay failed:`, err);
          return false;
        }
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [application, placement, version],
  );
}

/**
 * Action-bar buttons for top-bar extensions. The button content is the extension component
 * (as upstream); the accessible name is the registered title. Clicking opens the flyout.
 * `placement` selects the middle group (isMiddle) or the end group.
 */
export function TopBarExtensionActions({
  application,
  tree,
  placement = 'all',
}: {
  application: Application;
  tree: ApplicationTree | undefined;
  placement?: TopBarPlacement;
}) {
  const extensions = useTopBarExtensions(application, placement);
  const [openId, setOpenId] = useFlyout();
  if (extensions.length === 0) return null;
  return (
    <>
      {extensions.map((ext) => {
        const Ext = ext.component;
        return (
          <Button
            key={ext.id}
            variant="ghost"
            icon={<IconExtension />}
            aria-label={ext.title}
            aria-haspopup={ext.flyout ? 'dialog' : undefined}
            onClick={() => {
              if (ext.flyout) setOpenId(ext.id);
            }}
          >
            <ExtensionBoundary message={`Something went wrong with Extension ${ext.title}`}>
              <Ext application={application} tree={tree ?? EMPTY_TREE} openFlyout={() => setOpenId(ext.id)} />
            </ExtensionBoundary>
          </Button>
        );
      })}
      {extensions.map((ext) => (
        <FlyoutDrawer
          key={`flyout-${ext.id}`}
          ext={ext}
          open={openId === ext.id}
          onClose={() => setOpenId('')}
          application={application}
          tree={tree}
        />
      ))}
    </>
  );
}

// ---------------------------------------------------------------------------------------------
// System-level pages (registerSystemLevelExtension)

export interface ExtensionNavItem {
  to: string;
  label: string;
  icon: ReactNode;
}

/** Sidebar entries for system-level extensions, in registration order. */
export function useSystemExtensionNavItems(): ExtensionNavItem[] {
  useEnsureExtensionsLoaded();
  const version = useRegistryVersion();
  return useMemo(
    () => getSystemExtensions().map((ext) => ({ to: ext.path ?? '/', label: ext.title, icon: <IconExtension /> })),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [version],
  );
}

/** The system extension whose path equals `pathname` (or is its parent path). */
export function findSystemExtension(pathname: string): SystemLevelExtension | undefined {
  const path = pathname.replace(/\/+$/, '') || '/';
  return getSystemExtensions().find((ext) => ext.path !== undefined && (path === ext.path || path.startsWith(`${ext.path}/`)));
}

function SystemExtensionPage({ ext }: { ext: SystemLevelExtension }) {
  useDocumentTitle(ext.title);
  const Ext = ext.component;
  return (
    <>
      <PageHeader title={ext.title} />
      <ExtensionBoundary message={`Something went wrong with Extension ${ext.title}`}>
        <Ext />
      </ExtensionBoundary>
    </>
  );
}

/**
 * Catch-all route element: renders the system-level extension registered for the current path,
 * a loading state while extension bundles load, else `fallback` (the 404 page). Built-in routes
 * always win because they match first.
 */
export function SystemExtensionRoutes({ fallback }: { fallback: ReactNode }) {
  useEnsureExtensionsLoaded();
  useRegistryVersion();
  const loadState = useSyncExternalStore(subscribeExtensionsLoadState, extensionsLoadState, extensionsLoadState);
  const { pathname } = useLocation();
  const ext = findSystemExtension(pathname);
  if (ext) return <SystemExtensionPage key={ext.path} ext={ext} />;
  if (loadState === 'loading') {
    return (
      <div className={styles.loading} aria-busy="true">
        <Spinner label="Loading extensions" showLabel />
      </div>
    );
  }
  return <>{fallback}</>;
}

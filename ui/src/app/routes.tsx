import { Navigate, type RouteObject } from 'react-router';
import { findSystemExtension, SystemExtensionRoutes } from '../extensions';
import { AppShell } from '../shell/AppShell';
import type { RouteHandle } from '../shell/breadcrumbs';
import { AppLoading, NotFoundPage, RootError, RouteError } from '../shell/errors';
import { SETTINGS_SECTIONS } from '../pages/settings/sections';
import { RootLayout } from './RootLayout';

/*
 * Route table (paths are relative to the router basename, i.e. the server root path). Pages are
 * code-split: each module under src/pages exports `Component` and is loaded through `lazy`.
 * Breadcrumbs come from `handle.crumb`. Paths mirror upstream so deep links keep working.
 */

const handle = (crumb: RouteHandle['crumb']): RouteHandle => ({ crumb });

const settingsTitle = (path: string) => SETTINGS_SECTIONS.find((s) => s.path === path)?.title ?? path;

function settingsRoute(path: string, load: RouteObject['lazy'], children?: RouteObject[]): RouteObject {
  return {
    path,
    handle: handle(() => ({ label: settingsTitle(path) })),
    children: [{ index: true, lazy: load }, ...(children ?? [])],
  };
}

/** Auth pages (outside the shell) and the shell with every page. */
const childRoutes: RouteObject[] = [
  { path: 'login', lazy: () => import('../pages/auth/LoginPage'), errorElement: <RootError />, hydrateFallbackElement: <AppLoading /> },
  { path: 'logout', lazy: () => import('../pages/auth/LogoutPage'), errorElement: <RootError />, hydrateFallbackElement: <AppLoading /> },
  {
    element: <AppShell />,
    errorElement: <RootError />,
    hydrateFallbackElement: <AppLoading />,
    children: [
      {
        errorElement: <RouteError />,
        children: [
          { index: true, element: <Navigate to="/applications" replace /> },
          {
            path: 'applications',
            handle: handle(() => ({ label: 'Applications' })),
            children: [
              { index: true, lazy: () => import('../pages/applications/ApplicationsListPage') },
              {
                path: ':name',
                handle: handle((p) => ({ label: p.name ?? '' })),
                lazy: () => import('../pages/applications/ApplicationDetailsPage'),
              },
              {
                path: ':appNamespace/:name',
                handle: handle((p) => ({ label: `${p.appNamespace ?? ''}/${p.name ?? ''}` })),
                lazy: () => import('../pages/applications/ApplicationDetailsPage'),
              },
              {
                path: ':name/:namespace/:container/logs',
                handle: handle((p) => [{ label: p.name ?? '', to: `/applications/${p.name ?? ''}` }, { label: 'Logs' }]),
                lazy: () => import('../pages/applications/ApplicationLogsPage'),
              },
              {
                path: ':appNamespace/:name/:namespace/:container/logs',
                handle: handle((p) => [
                  { label: `${p.appNamespace ?? ''}/${p.name ?? ''}`, to: `/applications/${p.appNamespace ?? ''}/${p.name ?? ''}` },
                  { label: 'Logs' },
                ]),
                lazy: () => import('../pages/applications/ApplicationLogsPage'),
              },
            ],
          },
          {
            path: 'applicationsets',
            handle: handle(() => ({ label: 'ApplicationSets' })),
            children: [
              { index: true, lazy: () => import('../pages/applicationsets/ApplicationSetsListPage') },
              {
                path: ':name',
                handle: handle((p) => ({ label: p.name ?? '' })),
                lazy: () => import('../pages/applicationsets/ApplicationSetDetailsPage'),
              },
              {
                path: ':appsetNamespace/:name',
                handle: handle((p) => ({ label: `${p.appsetNamespace ?? ''}/${p.name ?? ''}` })),
                lazy: () => import('../pages/applicationsets/ApplicationSetDetailsPage'),
              },
            ],
          },
          {
            path: 'settings',
            handle: handle(() => ({ label: 'Settings' })),
            children: [
              { index: true, lazy: () => import('../pages/settings/SettingsIndexPage') },
              settingsRoute('repos', () => import('../pages/settings/repositories/RepositoriesPage')),
              settingsRoute('repocreds', () => import('../pages/settings/repocreds/RepoCredsPage')),
              settingsRoute('certs', () => import('../pages/settings/certificates/CertificatesPage')),
              settingsRoute('gpgkeys', () => import('../pages/settings/gpgkeys/GPGKeysPage')),
              settingsRoute('clusters', () => import('../pages/settings/clusters/ClustersPage'), [
                {
                  path: ':server',
                  handle: handle((p) => ({ label: p.server ?? '' })),
                  lazy: () => import('../pages/settings/clusters/ClusterDetailsPage'),
                },
              ]),
              settingsRoute('projects', () => import('../pages/settings/projects/ProjectsPage'), [
                {
                  path: ':name',
                  handle: handle((p) => ({ label: p.name ?? '' })),
                  lazy: () => import('../pages/settings/projects/ProjectDetailsPage'),
                },
              ]),
              settingsRoute('accounts', () => import('../pages/settings/accounts/AccountsPage'), [
                {
                  path: ':name',
                  handle: handle((p) => ({ label: p.name ?? '' })),
                  lazy: () => import('../pages/settings/accounts/AccountDetailsPage'),
                },
              ]),
              settingsRoute('scmproviders', () => import('../pages/settings/scmproviders/SCMProvidersPage')),
              settingsRoute('notifications', () => import('../pages/settings/notifications/NotificationsPage')),
              settingsRoute('appearance', () => import('../pages/settings/appearance/AppearancePage')),
            ],
          },
          { path: 'user-info', handle: handle(() => ({ label: 'User info' })), lazy: () => import('../pages/auth/UserInfoPage') },
          { path: 'help', handle: handle(() => ({ label: 'Docs & help' })), lazy: () => import('../pages/help/HelpPage') },
          // Development-only component gallery; statically removed from production builds.
          ...(import.meta.env.DEV
            ? [{ path: 'dev/design', handle: handle(() => ({ label: 'Design system' })), lazy: () => import('../pages/dev/DesignGalleryPage') }]
            : []),
          // System-level UI extension pages (API-122), else the 404 page. Built-in routes match first.
          {
            path: '*',
            handle: handle((p) => ({ label: findSystemExtension(`/${p['*'] ?? ''}`)?.title ?? 'Not found' })),
            element: <SystemExtensionRoutes fallback={<NotFoundPage />} />,
          },
        ],
      },
    ],
  },
];

export const appRoutes: RouteObject[] = [
  {
    // Pathless root shared by every route: providers that render router links (toasts) live
    // inside the router (see RootLayout).
    element: <RootLayout />,
    errorElement: <RootError />,
    hydrateFallbackElement: <AppLoading />,
    children: childRoutes,
  },
];

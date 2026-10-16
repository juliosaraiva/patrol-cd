import type { Application, ApplicationList, Settings, UserInfo, Version } from '../../api/types';

/** Fixture data shared by tests and the dev mock mode (VITE_MOCK_API=1). Not real accounts. */

export const settingsFixture: Settings = {
  url: 'https://patrol.example.test',
  userLoginsDisabled: false,
  statusBadgeEnabled: true,
  execEnabled: false,
  appsInAnyNamespaceEnabled: true,
  controllerNamespace: 'patrol',
  hydratorEnabled: false,
  syncWithReplaceAllowed: false,
  uiBannerContent: '',
  uiBannerPosition: '',
  uiBannerPermanent: false,
  uiCssURL: '',
  kustomizeVersions: [],
  help: { chatUrl: '', chatText: '', binaryUrls: {} },
};

export const loggedInUser: UserInfo = {
  loggedIn: true,
  username: 'mock-admin',
  iss: 'patrol',
  groups: ['platform-team', 'oncall'],
};

export const loggedOutUser: UserInfo = { loggedIn: false };

export const versionFixture: Version = {
  version: 'v0.0.0-mock',
  buildDate: '2026-10-13T00:00:00Z',
  gitCommit: '0000000000000000000000000000000000000000',
  gitTreeState: 'clean',
  goVersion: 'go1.25',
  compiler: 'gc',
  platform: 'linux/amd64',
};

export function applicationFixture(
  name: string,
  overrides: { namespace?: string; health?: string; sync?: string; project?: string } = {},
): Application {
  return {
    apiVersion: 'cd.patrol.example/v1alpha1',
    kind: 'Application',
    metadata: {
      name,
      namespace: overrides.namespace ?? 'patrol',
      resourceVersion: '1',
      creationTimestamp: '2026-10-01T12:00:00Z',
      uid: `uid-${name}`,
    },
    spec: {
      project: overrides.project ?? 'default',
      source: { repoURL: 'https://git.example.test/platform/apps.git', path: name, targetRevision: 'main' },
      destination: { server: 'https://kubernetes.default.svc', namespace: name },
    },
    status: {
      health: { status: overrides.health ?? 'Healthy' },
      sync: { status: overrides.sync ?? 'Synced', revision: '4f2c1a9d3e7b' },
    },
  };
}

export const applicationsFixture: ApplicationList = {
  items: [
    applicationFixture('guestbook'),
    applicationFixture('payments', { health: 'Degraded', sync: 'OutOfSync', project: 'finance' }),
    applicationFixture('search', { health: 'Progressing', sync: 'Synced' }),
  ],
  metadata: { resourceVersion: '100' },
};

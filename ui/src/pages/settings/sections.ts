/** Settings areas, in display order. Used by the settings index and breadcrumbs. */
export interface SettingsSection {
  path: string;
  title: string;
  description: string;
}

export const SETTINGS_SECTIONS: readonly SettingsSection[] = [
  { path: 'repos', title: 'Repositories', description: 'Git, Helm and OCI repositories and their connection status.' },
  { path: 'repocreds', title: 'Repository credentials', description: 'Credential templates matched by URL prefix.' },
  { path: 'certs', title: 'Certificates and known hosts', description: 'TLS certificates and SSH known-hosts entries for repositories.' },
  { path: 'gpgkeys', title: 'GnuPG keys', description: 'Public keys used to verify commit signatures.' },
  { path: 'clusters', title: 'Clusters', description: 'Destination clusters, credentials and cache state.' },
  { path: 'projects', title: 'Projects', description: 'Tenancy boundaries: sources, destinations, roles and sync windows.' },
  { path: 'accounts', title: 'Accounts', description: 'Local accounts and API tokens.' },
  { path: 'scmproviders', title: 'SCM providers', description: 'GitHub, GitLab and other providers used by generators, webhooks and status reporting.' },
  { path: 'notifications', title: 'Notifications', description: 'Configured notification services, triggers and templates.' },
  { path: 'appearance', title: 'Appearance', description: 'Theme for this browser.' },
];

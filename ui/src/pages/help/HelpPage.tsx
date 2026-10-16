import { useSettings, useVersion } from '../../api/queries/resources';
import type { Version, VersionStringField } from '../../api/types';
import { buttonClassName, CodeBlock, CopyButton, KeyValueList, Mono, PageHeader, SkeletonText } from '../../design';
import { IconExternal } from '../../design/icons';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import { ErrorBanner } from '../../shell/errors';
import { ChatButton } from './ChatButton';
import { chatLink, cliDownloads, documentationURL } from './help';
import styles from './Help.module.css';

const SERVER_FIELDS: [VersionStringField, string][] = [
  ['version', 'Version'],
  ['buildDate', 'Build date'],
  ['gitCommit', 'Git commit'],
  ['gitTreeState', 'Git tree state'],
  ['goVersion', 'Go version'],
  ['compiler', 'Compiler'],
  ['platform', 'Platform'],
];

const TOOL_FIELDS: [VersionStringField, string][] = [
  ['kustomizeVersion', 'Kustomize'],
  ['helmVersion', 'Helm'],
  ['kubectlVersion', 'kubectl'],
  ['jsonnetVersion', 'Jsonnet'],
];

/** Documentation topics, relative to the documentation site of the distribution. */
const DOC_TOPICS: { path: string; label: string }[] = [
  { path: 'user-guide/', label: 'User guide' },
  { path: 'operator-manual/', label: 'Operator manual' },
  { path: 'developer-guide/extensions/', label: 'UI and proxy extensions' },
];

function versionText(version: Version): string {
  return [...SERVER_FIELDS, ...TOOL_FIELDS]
    .filter(([key]) => version[key])
    .map(([key, label]) => `${label}: ${version[key] ?? ''}`)
    .join('\n');
}

function ExternalLink({ href, children }: { href: string; children: string }) {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer">
      {children}
      <IconExternal />
      <span className="sr-only"> (opens in a new tab)</span>
    </a>
  );
}

function serverHost(url: string | undefined): string {
  if (url) {
    try {
      return new URL(url).host;
    } catch {
      // fall through to the current host
    }
  }
  return window.location.host;
}

/** /help — CLI downloads, API reference, documentation, chat and version information (API-109, API-111). */
export function Component() {
  useDocumentTitle('Docs & help');
  const settings = useSettings();
  const version = useVersion();
  const downloads = cliDownloads(settings.data, version.data);
  const chat = chatLink(settings.data);
  const docs = documentationURL();
  const host = serverHost(settings.data?.url);

  return (
    <>
      <PageHeader title="Docs & help" description="Command-line tools, API reference, documentation and version information for this Patrol CD instance." />
      <div className={styles.sections}>
        <section className={styles.section} aria-labelledby="help-cli">
          <h2 id="help-cli">Command-line tool</h2>
          <p>Download the patrol CLI for your platform, make it executable, then sign in to this instance.</p>
          <ul className={styles.downloads} aria-label="CLI downloads">
            {downloads.map((d) => (
              <li key={d.id}>
                {d.local ? (
                  <a className={buttonClassName('secondary')} href={d.href} download>
                    {d.label}
                  </a>
                ) : (
                  <a className={buttonClassName('secondary')} href={d.href} target="_blank" rel="noopener noreferrer">
                    {d.label}
                    <IconExternal />
                    <span className="sr-only"> (opens in a new tab)</span>
                  </a>
                )}
              </li>
            ))}
          </ul>
          <CodeBlock title="Sign in with the CLI" label="CLI sign-in command" code={`patrol login ${host}${settings.data?.oidcConfig?.name || settings.data?.dexConfig?.connectors?.length ? ' --sso' : ''}`} />
        </section>

        <section className={styles.section} aria-labelledby="help-api">
          <h2 id="help-api">API reference</h2>
          <p>
            The REST API under <Mono>/api/v1</Mono> is described by an OpenAPI document. Requests authenticate with a bearer token from{' '}
            <Mono>patrol account generate-token</Mono> or a project role token.
          </p>
          <ul className={styles.links}>
            <li>
              <ExternalLink href="swagger-ui">Interactive API reference</ExternalLink>
            </li>
          </ul>
        </section>

        <section className={styles.section} aria-labelledby="help-docs">
          <h2 id="help-docs">Documentation</h2>
          {docs ? (
            <ul className={styles.links}>
              {DOC_TOPICS.map((t) => (
                <li key={t.path}>
                  <ExternalLink href={new URL(t.path, docs).href}>{t.label}</ExternalLink>
                </li>
              ))}
            </ul>
          ) : (
            <p>
              The user guide, operator manual and extension guides ship with your Patrol CD release in its <Mono>docs/</Mono> directory. Ask
              the operators of this instance where they publish them.
            </p>
          )}
        </section>

        {chat ? (
          <section className={styles.section} aria-labelledby="help-chat">
            <h2 id="help-chat">Get help</h2>
            <p>Questions about this instance go to the team that runs it.</p>
            <div>
              <ChatButton />
            </div>
          </section>
        ) : null}

        <section className={styles.section} aria-labelledby="help-version" aria-busy={version.isPending || undefined}>
          <div className={styles.versionHeader}>
            <h2 id="help-version">Version</h2>
            {version.data ? <CopyButton value={versionText(version.data)} label="Copy version information" showLabel /> : null}
          </div>
          {version.isPending ? <SkeletonText lines={4} /> : null}
          {version.isError ? <ErrorBanner error={version.error} onRetry={() => void version.refetch()} title="Version information is unavailable" /> : null}
          {version.data ? (
            <>
              <h3 className="sr-only">Server</h3>
              <KeyValueList
                items={SERVER_FIELDS.filter(([key]) => version.data[key]).map(([key, label]) => ({ key, label, value: <Mono>{version.data[key]}</Mono> }))}
              />
              <h3 className={styles.subheading}>Bundled tools</h3>
              {TOOL_FIELDS.some(([key]) => version.data[key]) ? (
                <KeyValueList
                  items={TOOL_FIELDS.filter(([key]) => version.data[key]).map(([key, label]) => ({ key, label, value: <Mono>{version.data[key]}</Mono> }))}
                />
              ) : (
                <p>The server did not report tool versions.</p>
              )}
            </>
          ) : null}
        </section>
      </div>
    </>
  );
}

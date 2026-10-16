import { useState } from 'react';
import { badgeURL } from '../../../api/endpoints/misc';
import type { Application, Settings } from '../../../api/types';
import { CodeBlock, FormField, Select } from '../../../design';
import { appName } from '../shared/app';
import styles from './Details.module.css';

export type BadgeFormat = 'URL' | 'Markdown' | 'Textile' | 'Rdoc' | 'AsciiDoc';
export const BADGE_FORMATS: readonly BadgeFormat[] = ['URL', 'Markdown', 'Textile', 'Rdoc', 'AsciiDoc'];

/** Badge image URL and the page it links to (statusbadge.url overrides the origin). */
export function badgeLinks(app: Application, settings: Settings): { image: string; target: string } {
  const name = appName(app);
  const ns = app.metadata.namespace;
  const namespaced = Boolean(settings.appsInAnyNamespaceEnabled && ns && ns !== settings.controllerNamespace);
  const root = settings.statusBadgeRootUrl ? (settings.statusBadgeRootUrl.endsWith('/') ? settings.statusBadgeRootUrl : `${settings.statusBadgeRootUrl}/`) : document.baseURI;
  const image = badgeURL({ name, revision: true, showAppName: true, ...(namespaced && ns ? { appNamespace: ns } : {}) }, settings.statusBadgeRootUrl === '' ? undefined : settings.statusBadgeRootUrl);
  const path = namespaced && ns ? `applications/${encodeURIComponent(ns)}/${encodeURIComponent(name)}` : `applications/${encodeURIComponent(name)}`;
  return { image, target: new URL(path, root).href };
}

/** Snippet text per format (same shapes as upstream's badge panel). */
export function badgeSnippet(format: BadgeFormat, image: string, target: string, alt = 'App Status'): string {
  switch (format) {
    case 'URL':
      return image;
    case 'Markdown':
      return `[![${alt}](${image})](${target})`;
    case 'Textile':
      return `!${image}!:${target}`;
    case 'Rdoc':
      return `{<img src="${image}" alt="${alt}" />}[${target}]`;
    case 'AsciiDoc':
      return `image:${image}["${alt}", link="${target}"]`;
  }
}

/** Status badge preview and copyable snippet (parity API-114); hidden when badges are disabled. */
export function StatusBadge({ app, settings }: { app: Application; settings: Settings | undefined }) {
  const [format, setFormat] = useState<BadgeFormat>('URL');
  if (!settings?.statusBadgeEnabled) return null;
  const { image, target } = badgeLinks(app, settings);
  return (
    <div className={styles.stack}>
      <img src={image} alt={`Status badge of ${appName(app)}`} className={styles.badgeImage} />
      <FormField label="Snippet format">
        <Select value={format} onChange={(e) => setFormat(e.target.value as BadgeFormat)} options={BADGE_FORMATS.map((f) => ({ value: f, label: f }))} />
      </FormField>
      <CodeBlock code={badgeSnippet(format, image, target)} title={`Badge ${format}`} wrap maxHeight={160} />
    </div>
  );
}

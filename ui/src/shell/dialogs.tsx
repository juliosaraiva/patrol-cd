import type { Version, VersionStringField } from '../api/types';
import { CopyButton, Dialog, KeyValueList, Mono, Skeleton } from '../design';
import { SHORTCUTS } from './shortcuts';
import styles from './Shell.module.css';

export function ShortcutsDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    <Dialog open={open} onClose={onClose} title="Keyboard shortcuts">
      <dl className={styles.shortcuts}>
        {SHORTCUTS.map((s) => (
          <div key={s.description} className={styles.shortcutRow}>
            <dt>
              {s.keys.map((k) => (
                <kbd key={k} className={styles.kbd}>
                  {k}
                </kbd>
              ))}
            </dt>
            <dd>{s.description}</dd>
          </div>
        ))}
      </dl>
    </Dialog>
  );
}

const VERSION_FIELDS: [VersionStringField, string][] = [
  ['version', 'Version'],
  ['buildDate', 'Build date'],
  ['gitCommit', 'Git commit'],
  ['gitTreeState', 'Git tree state'],
  ['goVersion', 'Go version'],
  ['compiler', 'Compiler'],
  ['platform', 'Platform'],
  ['kustomizeVersion', 'Kustomize'],
  ['helmVersion', 'Helm'],
  ['kubectlVersion', 'kubectl'],
  ['jsonnetVersion', 'Jsonnet'],
];

export function versionText(version: Version): string {
  return VERSION_FIELDS.filter(([key]) => version[key])
    .map(([key, label]) => `${label}: ${version[key] ?? ''}`)
    .join('\n');
}

export function VersionDialog({ open, onClose, version }: { open: boolean; onClose: () => void; version: Version | undefined }) {
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Version information"
      footer={version ? <CopyButton value={versionText(version)} label="Copy version information" showLabel /> : undefined}
    >
      {version ? (
        <KeyValueList
          items={VERSION_FIELDS.filter(([key]) => version[key]).map(([key, label]) => ({
            key,
            label,
            value: <Mono>{version[key]}</Mono>,
          }))}
        />
      ) : (
        <Skeleton height={80} />
      )}
    </Dialog>
  );
}

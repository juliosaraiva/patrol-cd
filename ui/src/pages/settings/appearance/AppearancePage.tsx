import { PageHeader, RadioGroup, useTheme, type ThemePreference } from '../../../design';
import { useDocumentTitle } from '../../../lib/useDocumentTitle';
import styles from '../Settings.module.css';

const OPTIONS: { value: ThemePreference; label: string; description: string }[] = [
  { value: 'system', label: 'System', description: 'Follow the operating system light or dark setting.' },
  { value: 'light', label: 'Light', description: 'Always use the light theme.' },
  { value: 'dark', label: 'Dark', description: 'Always use the dark theme.' },
];

/** /settings/appearance — theme preference for this browser (API-107). */
export function Component() {
  useDocumentTitle('Appearance', 'Settings');
  const { preference, setPreference } = useTheme();
  return (
    <>
      <PageHeader title="Appearance" description="Stored in this browser only." />
      <div className={styles.section}>
        <RadioGroup legend="Theme" value={preference} onChange={setPreference} options={OPTIONS} />
      </div>
    </>
  );
}

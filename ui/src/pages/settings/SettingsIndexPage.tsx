import { Link } from 'react-router';
import { PageHeader } from '../../design';
import { useDocumentTitle } from '../../lib/useDocumentTitle';
import styles from './Settings.module.css';
import { SETTINGS_SECTIONS } from './sections';

/** /settings — index of settings areas. */
export function Component() {
  useDocumentTitle('Settings');
  return (
    <>
      <PageHeader title="Settings" description="Configure repositories, clusters, projects, accounts and this browser." />
      <ul className={styles.grid}>
        {SETTINGS_SECTIONS.map((section) => (
          <li key={section.path}>
            <Link to={section.path} className={styles.card}>
              <span className={styles.cardTitle}>{section.title}</span>
              <span className={styles.cardText}>{section.description}</span>
            </Link>
          </li>
        ))}
      </ul>
    </>
  );
}

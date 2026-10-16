import type { ReactNode } from 'react';
import { Badge, EmptyState, PageHeader } from '../design';
import { IconBook } from '../design/icons';
import { useDocumentTitle } from '../lib/useDocumentTitle';
import styles from './Placeholder.module.css';

export interface PlannedCapability {
  /** Short name of the capability, e.g. "Applications list". */
  name: string;
  /** What the finished page will do. */
  summary: ReactNode;
  /** Parity inventory items covered (docs/parity/areas/*.md). */
  parity: readonly string[];
}

/**
 * Scaffold for a page that is not implemented yet. Replace the whole component when the page
 * is built; keep the route, handle and file location.
 */
export function PlaceholderPage({
  title,
  description,
  planned,
  actions,
  children,
}: {
  title: string;
  description?: ReactNode;
  planned: PlannedCapability;
  actions?: ReactNode;
  children?: ReactNode;
}) {
  useDocumentTitle(title);
  return (
    <>
      <PageHeader title={title} description={description} actions={actions} />
      {children}
      <PlannedEmptyState planned={planned} />
    </>
  );
}

export function PlannedEmptyState({ planned, headingLevel = 2 }: { planned: PlannedCapability; headingLevel?: 2 | 3 }) {
  return (
    <EmptyState icon={<IconBook size={24} />} title={`${planned.name} is planned`} headingLevel={headingLevel}>
      <p>{planned.summary}</p>
      {planned.parity.length > 0 ? (
        <p className={styles.parity}>
          <span>Parity items:</span>
          {planned.parity.map((id) => (
            <Badge key={id}>{id}</Badge>
          ))}
        </p>
      ) : null}
    </EmptyState>
  );
}

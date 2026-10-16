import type { ReactNode } from 'react';
import { NavLink } from 'react-router';
import { IconApplications, IconApplicationSets, IconHelp, IconSettings, IconUser } from '../design/icons';
import styles from './Shell.module.css';

export interface NavItem {
  to: string;
  label: string;
  icon: ReactNode;
}

export const NAV_ITEMS: readonly NavItem[] = [
  { to: '/applications', label: 'Applications', icon: <IconApplications /> },
  { to: '/applicationsets', label: 'ApplicationSets', icon: <IconApplicationSets /> },
  { to: '/settings', label: 'Settings', icon: <IconSettings /> },
  { to: '/user-info', label: 'User info', icon: <IconUser /> },
  { to: '/help', label: 'Docs & help', icon: <IconHelp /> },
];

function NavEntry({ item, onNavigate }: { item: NavItem; onNavigate: (() => void) | undefined }) {
  return (
    <NavLink to={item.to} className={styles.navLink} onClick={onNavigate}>
      <span className={styles.navIcon}>{item.icon}</span>
      {item.label}
    </NavLink>
  );
}

/**
 * Primary navigation list; NavLink sets aria-current="page" on the active section.
 * `extensionItems` (system-level UI extensions, API-122) follow the built-in sections.
 */
export function NavList({ onNavigate, extensionItems = [] }: { onNavigate?: () => void; extensionItems?: readonly NavItem[] }) {
  return (
    <ul className={styles.navList}>
      {NAV_ITEMS.map((item) => (
        <li key={item.to}>
          <NavEntry item={item} onNavigate={onNavigate} />
        </li>
      ))}
      {extensionItems.map((item, index) => (
        // Extensions may register the same path or title more than once.
        <li key={`extension-${index}`}>
          <NavEntry item={item} onNavigate={onNavigate} />
        </li>
      ))}
    </ul>
  );
}

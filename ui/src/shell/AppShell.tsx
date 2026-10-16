import { useEffect, useRef, useState, type ReactNode, type RefObject } from 'react';
import { Link, Outlet, useLocation, useMatch, useNavigate } from 'react-router';
import { useVersion } from '../api/queries/resources';
import { RequireSession, useSession } from '../app/session';
import {
  Breadcrumbs,
  Button,
  Drawer,
  IconButton,
  Menu,
  SearchInput,
  useTheme,
  type ThemePreference,
} from '../design';
import {
  IconCheck,
  IconKeyboard,
  IconLogout,
  IconMenu,
  IconMonitor,
  IconMoon,
  IconSun,
  IconUser,
  PatrolMark,
} from '../design/icons';
import { useSystemExtensionNavItems } from '../extensions';
import { ChatButton } from '../pages/help/ChatButton';
import { useBreadcrumbs } from './breadcrumbs';
import { ShortcutsDialog, VersionDialog } from './dialogs';
import { AppLoading } from './errors';
import { NavList } from './nav';
import { useGlobalShortcuts } from './shortcuts';
import styles from './Shell.module.css';
import { SiteBanner, useSiteBanner } from './SiteBanner';

const THEME_OPTIONS: { value: ThemePreference; label: string; icon: ReactNode }[] = [
  { value: 'system', label: 'System theme', icon: <IconMonitor /> },
  { value: 'light', label: 'Light theme', icon: <IconSun /> },
  { value: 'dark', label: 'Dark theme', icon: <IconMoon /> },
];

function ThemeMenu() {
  const { preference, resolved, setPreference } = useTheme();
  const current = THEME_OPTIONS.find((o) => o.value === preference) ?? THEME_OPTIONS[0];
  return (
    <Menu
      label="Theme"
      align="end"
      trigger={(props) => (
        <IconButton
          {...props}
          aria-label={`Theme: ${current?.label ?? 'System theme'}`}
          icon={resolved === 'dark' ? <IconMoon /> : <IconSun />}
        />
      )}
      items={THEME_OPTIONS.map((o) => ({
        id: o.value,
        label: o.label,
        icon: o.value === preference ? <IconCheck /> : o.icon,
        onSelect: () => setPreference(o.value),
      }))}
    />
  );
}

function UserMenu() {
  const navigate = useNavigate();
  const location = useLocation();
  const { user, anonymous } = useSession();
  const name = user?.loggedIn ? (user.username ?? 'Signed in') : 'Anonymous';
  return (
    <Menu
      align="end"
      label="Account"
      header={user?.loggedIn ? `Signed in as ${name}` : 'Not signed in'}
      trigger={(props) => (
        <Button {...props} variant="ghost" icon={<IconUser />} aria-label={`Account: ${name}`}>
          <span className={styles.userName}>{name}</span>
        </Button>
      )}
      items={[
        { id: 'user-info', label: 'User info', icon: <IconUser />, onSelect: () => void navigate('/user-info') },
        { type: 'separator', id: 'sep' },
        anonymous
          ? {
              id: 'login',
              label: 'Sign in',
              icon: <IconLogout />,
              onSelect: () => void navigate(`/login?return_url=${encodeURIComponent(window.location.href)}`, { state: { from: location } }),
            }
          : { id: 'logout', label: 'Log out', icon: <IconLogout />, onSelect: () => void navigate('/logout') },
      ]}
    />
  );
}

function GlobalSearch({ inputRef }: { inputRef: RefObject<HTMLInputElement | null> }) {
  const navigate = useNavigate();
  const [value, setValue] = useState('');
  return (
    <form
      role="search"
      className={styles.search}
      onSubmit={(e) => {
        e.preventDefault();
      }}
    >
      <SearchInput
        ref={inputRef}
        aria-label="Search applications"
        placeholder="Search applications"
        size="sm"
        shortcut="/"
        value={value}
        onValueChange={setValue}
        onSubmit={(q) => {
          const query = q.trim();
          void navigate(query ? `/applications?search=${encodeURIComponent(query)}` : '/applications');
        }}
      />
    </form>
  );
}

/**
 * Client-side navigation does not move focus or announce anything by itself. When the path
 * changes (not just the search params), focus moves to <main> unless the new page already put
 * it inside main (e.g. an autofocused field) or it is in a modal dialog that stayed open (a route
 * can commit after the user opened one; moving focus out would break the dialog's focus trap and
 * Esc), and the new document title is announced politely.
 */
function useRouteChangeFocus(mainRef: RefObject<HTMLElement | null>, announcerRef: RefObject<HTMLElement | null>) {
  const { pathname } = useLocation();
  const previous = useRef(pathname);
  useEffect(() => {
    if (previous.current === pathname) return;
    previous.current = pathname;
    const main = mainRef.current;
    const active = document.activeElement;
    const focusInPage = main !== null && active !== null && active !== main && main.contains(active);
    const focusInModal = active?.closest('[aria-modal="true"]') != null;
    if (main && !focusInPage && !focusInModal) main.focus();
    // Page effects (useDocumentTitle) ran before this parent effect, so the title is current.
    if (announcerRef.current) announcerRef.current.textContent = document.title;
  }, [pathname, mainRef, announcerRef]);
}

function Shell() {
  const { settings } = useSession();
  const version = useVersion();
  // Subscribes the shell to extension registrations, so the nav and the breadcrumb of an
  // extension page (catch-all route handle) update when a bundle registers after the first render.
  const extensionNav = useSystemExtensionNavItems();
  const crumbs = useBreadcrumbs();
  // The help page shows the chat link in its own "Get help" section.
  const onHelpPage = useMatch('/help') !== null;
  const banner = useSiteBanner(settings);
  const searchRef = useRef<HTMLInputElement>(null);
  const mainRef = useRef<HTMLElement>(null);
  const announcerRef = useRef<HTMLDivElement>(null);
  const [navOpen, setNavOpen] = useState(false);
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  const [versionOpen, setVersionOpen] = useState(false);

  useRouteChangeFocus(mainRef, announcerRef);
  useGlobalShortcuts({
    onSearch: () => searchRef.current?.focus(),
    onHelp: () => setShortcutsOpen(true),
  });

  return (
    <div className={styles.shell}>
      <a
        className={styles.skipLink}
        href="#main"
        onClick={(e) => {
          e.preventDefault();
          mainRef.current?.focus();
        }}
      >
        Skip to main content
      </a>
      <SiteBanner banner={banner} placement="top" />
      <div className={styles.frame}>
        <aside className={styles.sidebar} aria-label="Sidebar">
          <Link to="/applications" className={styles.brand}>
            <PatrolMark />
            Patrol CD
          </Link>
          <nav aria-label="Main" className={styles.nav}>
            <NavList extensionItems={extensionNav} />
          </nav>
          <div className={styles.sidebarFooter}>
            <button type="button" className={styles.versionButton} onClick={() => setVersionOpen(true)}>
              {version.data?.version ? `Version ${version.data.version}` : 'Version information'}
            </button>
          </div>
        </aside>
        <div className={styles.column}>
          <header className={styles.topbar}>
            <IconButton
              className={styles.menuButton}
              aria-label="Open navigation"
              aria-expanded={navOpen}
              icon={<IconMenu />}
              onClick={() => setNavOpen(true)}
            />
            <div className={styles.crumbs}>
              <Breadcrumbs items={crumbs} />
            </div>
            <GlobalSearch inputRef={searchRef} />
            <div className={styles.topActions}>
              <IconButton aria-label="Keyboard shortcuts" icon={<IconKeyboard />} onClick={() => setShortcutsOpen(true)} />
              <ThemeMenu />
              <UserMenu />
            </div>
          </header>
          <main id="main" ref={mainRef} tabIndex={-1} className={styles.main}>
            <Outlet />
          </main>
        </div>
      </div>
      <SiteBanner banner={banner} placement="bottom" />
      {/* help.chatUrl (API-111): fixed bottom-right link on every page; renders nothing when unset. */}
      {onHelpPage ? null : <ChatButton floating />}
      <div ref={announcerRef} className="sr-only" role="status" aria-live="polite" aria-atomic="true" data-route-announcer="" />
      <Drawer open={navOpen} onClose={() => setNavOpen(false)} title="Patrol CD" side="start" width="280px">
        <nav aria-label="Main" className={styles.mobileNav}>
          <NavList onNavigate={() => setNavOpen(false)} extensionItems={extensionNav} />
        </nav>
      </Drawer>
      <ShortcutsDialog open={shortcutsOpen} onClose={() => setShortcutsOpen(false)} />
      <VersionDialog open={versionOpen} onClose={() => setVersionOpen(false)} version={version.data} />
    </div>
  );
}

/** Authenticated application frame: navigation, top bar, banners, shortcuts. */
export function AppShell() {
  return (
    <RequireSession fallback={<AppLoading />}>
      <Shell />
    </RequireSession>
  );
}

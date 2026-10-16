/** Pieces shared by the tile and table renderings of an application. */
import type { Application } from '../../../api/types';
import { Badge, IconButton, Menu, Mono } from '../../../design';
import { IconError, IconInfo, IconWarning } from '../../../design/icons';
import { appSources, sourceLocation } from '../shared/app';
import { IconLink, IconStar, IconStarFilled } from '../shared/icons';
import { applicationExternalLinks, openExternal } from '../shared/links';
import { noticePreview, parseNotice } from '../shared/notice';
import styles from './List.module.css';

export function FavoriteButton({ app, favorite, onToggle }: { app: Application; favorite: boolean; onToggle: () => void }) {
  const name = app.metadata.name ?? '';
  return (
    <IconButton
      size="sm"
      aria-label={favorite ? `Remove ${name} from favorites` : `Add ${name} to favorites`}
      aria-pressed={favorite}
      icon={favorite ? <IconStarFilled /> : <IconStar />}
      onClick={onToggle}
    />
  );
}

/** Notice icon (scope icon or both) with a 200-character preview (parity API-117). */
export function NoticeIcon({ app }: { app: Application }) {
  const notice = parseNotice(app);
  if (!notice || notice.scope === 'banner') return null;
  const preview = noticePreview(notice);
  const Icon = notice.icon === 'error' ? IconError : notice.icon === 'warning' ? IconWarning : IconInfo;
  const content = (
    <>
      <Icon size={14} />
      <span className="sr-only">
        Notice ({notice.severity}): {preview}
      </span>
    </>
  );
  return notice.url ? (
    <a className={styles.noticeIcon} data-severity={notice.severity} href={notice.url} target="_blank" rel="noopener noreferrer" title={preview}>
      {content}
    </a>
  ) : (
    <span className={styles.noticeIcon} data-severity={notice.severity} title={preview}>
      {content}
    </span>
  );
}

export function LabelBadges({ app, max = 4 }: { app: Application; max?: number }) {
  const entries = Object.entries(app.metadata.labels ?? {});
  if (entries.length === 0) return null;
  return (
    <span className={styles.labels}>
      {entries.slice(0, max).map(([k, v]) => (
        <Badge key={k}>
          <Mono>{v ? `${k}=${v}` : k}</Mono>
        </Badge>
      ))}
      {entries.length > max ? <Badge>+{entries.length - max}</Badge> : null}
    </span>
  );
}

export function SourceSummary({ app }: { app: Application }) {
  const sources = appSources(app);
  const first = sources[0];
  if (!first) return <span>—</span>;
  return (
    <span>
      <Mono>{first.repoURL}</Mono>
      {sourceLocation(first) ? (
        <>
          {' '}
          <Mono>{sourceLocation(first)}</Mono>
        </>
      ) : null}
      {!first.chart && first.targetRevision ? (
        <>
          {' @ '}
          <Mono>{first.targetRevision}</Mono>
        </>
      ) : null}
      {sources.length > 1 ? ` (+${sources.length - 1} more source${sources.length > 2 ? 's' : ''})` : ''}
    </span>
  );
}

export function LinksMenu({ app }: { app: Application }) {
  const links = applicationExternalLinks(app);
  if (links.length === 0) return null;
  return (
    <Menu
      label={`Links of ${app.metadata.name ?? ''}`}
      trigger={(props) => <IconButton {...props} size="sm" aria-label={`External links of ${app.metadata.name ?? ''}`} icon={<IconLink />} />}
      items={links.map((l) => ({ id: l.url, label: l.title ?? l.url, onSelect: () => openExternal(l.url) }))}
    />
  );
}

import { useState } from 'react';
import type { Application } from '../../../api/types';
import { Banner } from '../../../design';
import { IconExternal } from '../../../design/icons';
import { dismissNotice, isNoticeDismissed, parseNotice, type NoticeSeverity } from '../shared/notice';

const TONE: Record<NoticeSeverity, 'info' | 'warning' | 'error'> = { info: 'info', warning: 'warning', critical: 'error' };

/** Banner from the notice.cd.patrol.example/* annotations (parity API-117). */
export function NoticeBanner({ app }: { app: Application }) {
  const notice = parseNotice(app);
  const [dismissedKey, setDismissedKey] = useState<string | null>(null);
  if (!notice || notice.scope === 'icon') return null;
  if (dismissedKey === notice.dismissKey || isNoticeDismissed(notice)) return null;
  return (
    <Banner
      tone={TONE[notice.severity]}
      title="Application notice"
      onDismiss={
        notice.permanent
          ? undefined
          : () => {
              dismissNotice(notice.dismissKey);
              setDismissedKey(notice.dismissKey);
            }
      }
      dismissLabel="Dismiss notice"
    >
      <p>{notice.content}</p>
      {notice.url ? (
        <p>
          <a href={notice.url} target="_blank" rel="noopener noreferrer">
            More information <IconExternal size={12} />
            <span className="sr-only"> (opens in a new tab)</span>
          </a>
        </p>
      ) : null}
    </Banner>
  );
}

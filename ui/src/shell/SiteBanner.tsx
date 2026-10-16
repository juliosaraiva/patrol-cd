import { useState } from 'react';
import type { Settings } from '../api/types';
import { Banner, Button } from '../design';
import { isHttpURL } from '../lib/url';
import { readStorage, removeStorage, writeStorage } from '../lib/storage';
import styles from './Shell.module.css';

export const HIDDEN_BANNER_KEY = 'patrol.hiddenBannerContent';

export type BannerPlacement = 'top' | 'bottom';

/** Where the instance banner renders: ui.bannerposition top (default) | bottom | both. */
export function bannerPlacements(settings: Settings | undefined): BannerPlacement[] {
  if (!settings?.uiBannerContent) return [];
  switch (settings.uiBannerPosition) {
    case 'bottom':
      return ['bottom'];
    case 'both':
      return ['top', 'bottom'];
    default:
      return ['top'];
  }
}

/**
 * Instance banner from settings (ui.bannercontent/bannerurl/bannerpermanent/bannerposition).
 * Non-permanent banners can be dismissed for this page load or permanently for this content;
 * changed content shows again.
 */
export function useSiteBanner(settings: Settings | undefined) {
  const content = settings?.uiBannerContent ?? '';
  const [dismissed, setDismissed] = useState(false);
  const [hiddenContent, setHiddenContent] = useState(() => readStorage(HIDDEN_BANNER_KEY));
  const permanent = settings?.uiBannerPermanent === true;
  const visible = content !== '' && (permanent || (!dismissed && hiddenContent !== content));
  return {
    visible,
    content,
    url: isHttpURL(settings?.uiBannerURL) ? settings.uiBannerURL : undefined,
    permanent,
    placements: visible ? bannerPlacements(settings) : [],
    dismiss: () => setDismissed(true),
    hideForever: () => {
      writeStorage(HIDDEN_BANNER_KEY, content);
      setHiddenContent(content);
    },
    reset: () => {
      removeStorage(HIDDEN_BANNER_KEY);
      setHiddenContent(null);
    },
  };
}

export function SiteBanner({ banner, placement }: { banner: ReturnType<typeof useSiteBanner>; placement: BannerPlacement }) {
  if (!banner.visible || !banner.placements.includes(placement)) return null;
  const text = banner.url ? (
    <a href={banner.url} target="_blank" rel="noopener noreferrer">
      {banner.content}
    </a>
  ) : (
    banner.content
  );
  return (
    <aside aria-label="Announcement">
      <Banner
        tone="info"
        className={[
          styles.siteBanner,
          placement === 'top' ? styles.siteBannerTop : styles.siteBannerBottom,
          banner.permanent ? styles.siteBannerPermanent : undefined,
        ]
          .filter(Boolean)
          .join(' ')}
        actions={
          banner.permanent ? undefined : (
            <>
              <Button size="sm" variant="ghost" onClick={banner.dismiss}>
                Dismiss for now
              </Button>
              <Button size="sm" variant="ghost" onClick={banner.hideForever}>
                Don&apos;t show again
              </Button>
            </>
          )
        }
      >
        {text}
      </Banner>
    </aside>
  );
}

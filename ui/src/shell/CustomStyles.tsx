import { useEffect } from 'react';
import { useSettings } from '../api/queries/resources';
import { isHttpOrRelativeURL } from '../lib/url';

/**
 * Appends <link rel="stylesheet"> for settings.uiCssURL (ui.cssurl). Relative values resolve
 * against the base href, like upstream's "./custom/styles.css". Only http(s) and relative URLs
 * are accepted.
 */
export function CustomStyles() {
  const { data } = useSettings();
  const href = data?.uiCssURL;
  useEffect(() => {
    if (!isHttpOrRelativeURL(href)) return undefined;
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = href;
    link.dataset.patrolCustomCss = '';
    document.head.appendChild(link);
    return () => link.remove();
  }, [href]);
  return null;
}

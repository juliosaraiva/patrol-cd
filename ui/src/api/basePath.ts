/**
 * The server injects `<base href="<rootpath>/">` into index.html. Every URL the UI builds (API
 * calls, router basename, SSO redirects) is resolved against document.baseURI so one build works
 * under any root path.
 */

/** Absolute URL of the UI root, always ending in "/". */
export function appBaseURL(): URL {
  const url = new URL(document.baseURI);
  url.search = '';
  url.hash = '';
  if (!url.pathname.endsWith('/')) url.pathname = url.pathname.replace(/[^/]*$/, '');
  return url;
}

/** Router basename: the base path without its trailing slash ("" at the origin root). */
export function routerBasename(): string {
  return appBaseURL().pathname.replace(/\/$/, '');
}

/**
 * Resolves a server path such as "/api/v1/applications" against the base URL. The leading slash
 * is dropped so the path stays under the root path.
 */
export function resolveServerURL(path: string): URL {
  return new URL(path.replace(/^\/+/, ''), appBaseURL());
}

/**
 * Converts an absolute same-origin URL under the base into a router path ("/applications?x=1").
 * Returns null for anything else, including paths that start with "//" or "/\\": routers and
 * browsers treat those as protocol-relative URLs to another host (e.g. "/.//evil.example"
 * resolves to the path "//evil.example").
 */
export function toRouterPath(href: string): string | null {
  let url: URL;
  try {
    url = new URL(href, appBaseURL());
  } catch {
    return null;
  }
  const base = appBaseURL();
  if (url.origin !== base.origin) return null;
  const basePath = base.pathname;
  const withSlash = url.pathname.endsWith('/') ? url.pathname : `${url.pathname}/`;
  if (!withSlash.startsWith(basePath)) return null;
  const rest = url.pathname.slice(basePath.length - 1) || '/';
  if (/^[\\/]{2}/.test(rest)) return null;
  return `${rest}${url.search}${url.hash}`;
}

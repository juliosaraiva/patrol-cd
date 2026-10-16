/** True for absolute http(s) URLs; rejects javascript:, data:, and other schemes. */
export function isHttpURL(value: string | undefined | null): value is string {
  if (!value) return false;
  try {
    const url = new URL(value);
    return url.protocol === 'http:' || url.protocol === 'https:';
  } catch {
    return false;
  }
}

/**
 * True for an http(s) URL or a relative path (resolved against the document base), the forms
 * accepted for operator-provided resources such as ui.cssurl.
 */
export function isHttpOrRelativeURL(value: string | undefined | null): value is string {
  if (!value) return false;
  if (/^[a-z][a-z0-9+.-]*:/i.test(value)) return isHttpURL(value);
  if (value.startsWith('//')) return false;
  try {
    const url = new URL(value, document.baseURI);
    return url.protocol === 'http:' || url.protocol === 'https:';
  } catch {
    return false;
  }
}

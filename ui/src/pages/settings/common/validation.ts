/** Client-side validation shared by the Settings forms. The server stays authoritative. */

const DNS1123_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

/** Kubernetes object names (DNS-1123 subdomain, at most 253 characters). */
export function validateObjectName(name: string, what = 'Name'): string | null {
  const value = name.trim();
  if (value === '') return `${what} is required.`;
  if (value.length > 253) return `${what} must be at most 253 characters.`;
  if (!value.split('.').every((label) => DNS1123_LABEL.test(label))) {
    return `${what} may contain only lowercase letters, digits, "-" and ".", and must start and end with a letter or digit.`;
  }
  return null;
}

/** Namespace names (DNS-1123 label, at most 63 characters); "*" globs are allowed when `glob`. */
export function validateNamespace(value: string, glob = false): string | null {
  const v = value.trim();
  if (v === '') return 'Enter a namespace.';
  if (glob && /[*?[\]!]/.test(v)) return null;
  if (v.length > 63 || !DNS1123_LABEL.test(v)) return 'Use lowercase letters, digits and "-" (at most 63 characters).';
  return null;
}

/** http(s) URL check for API endpoints and server URLs. */
export function validateHTTPURL(value: string, what = 'URL', required = true): string | null {
  const v = value.trim();
  if (v === '') return required ? `${what} is required.` : null;
  try {
    const url = new URL(v);
    if (url.protocol !== 'https:' && url.protocol !== 'http:') return `${what} must start with https:// or http://.`;
    return null;
  } catch {
    return `${what} must be a full URL such as https://api.example.com.`;
  }
}

/** Repository URL for the connection method (git over HTTPS/SSH, Helm, OCI). */
export function validateRepoURL(value: string, kind: 'https' | 'ssh' | 'helm' | 'oci'): string | null {
  const v = value.trim();
  if (v === '') return 'Repository URL is required.';
  if (/\s/.test(v)) return 'Repository URL must not contain spaces.';
  switch (kind) {
    case 'ssh':
      if (v.startsWith("ssh://") || /^[\w.-]+@[\w.-]+:/.test(v)) return null;
      return 'Use an SSH URL such as git@git.example.com:org/repo.git or ssh://git@git.example.com/org/repo.git.';
    case 'oci':
      if (/^oci:\/\/[^/]+/.test(v)) return null;
      return 'Use an OCI URL such as oci://registry.example.com/org/charts.';
    case 'helm':
      if (/^https?:\/\//.test(v) || v.startsWith("oci://")) return null;
      if (/^[\w.-]+(:\d+)?\/\S+/.test(v)) return null; // OCI registry reference without scheme (enable OCI)
      return 'Use an https:// chart repository URL, or a registry reference with OCI enabled.';
    case 'https':
      if (/^https?:\/\//.test(v)) return null;
      return 'Use an HTTPS URL such as https://git.example.com/org/repo.git.';
    default:
      return null;
  }
}

/** Proxy URLs must be http(s) or socks5. */
export function validateProxy(value: string): string | null {
  const v = value.trim();
  if (v === '') return null;
  try {
    const url = new URL(v);
    if (!['http:', 'https:', 'socks5:'].includes(url.protocol)) return 'Proxy must use http://, https:// or socks5://.';
    return null;
  } catch {
    return 'Proxy must be a URL such as http://proxy.example.com:3128.';
  }
}

export function validatePositiveInteger(value: string, what: string, required = false): string | null {
  const v = value.trim();
  if (v === '') return required ? `${what} is required.` : null;
  if (!/^\d+$/.test(v) || Number(v) <= 0) return `${what} must be a positive whole number.`;
  return null;
}

/** Label and annotation keys: optional DNS prefix + "/" + name (63 characters max). */
export function validateLabelKey(key: string): string | null {
  const k = key.trim();
  if (k === '') return 'Enter a key.';
  const [prefix, name, extra] = k.includes('/') ? k.split('/') : [undefined, k];
  if (extra !== undefined) return 'A key may contain at most one "/".';
  if (prefix !== undefined && validateObjectName(prefix, 'Key prefix')) return 'The key prefix must be a DNS subdomain such as example.com.';
  if (!name || name.length > 63 || !/^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$/.test(name)) {
    return 'Key names use letters, digits, "-", "_" and ".", start and end with a letter or digit, at most 63 characters.';
  }
  return null;
}

export function validateLabelValue(value: string): string | null {
  if (value === '') return null;
  if (value.length > 63 || !/^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$/.test(value)) {
    return 'Label values use letters, digits, "-", "_" and ".", at most 63 characters.';
  }
  return null;
}

/** Secret key names (data keys): letters, digits, "-", "_" and ".". */
export function validateSecretKey(value: string): string | null {
  const v = value.trim();
  if (v === '') return 'Enter the key inside the Secret.';
  if (v.length > 253 || !/^[-._a-zA-Z0-9]+$/.test(v)) return 'Secret keys use letters, digits, "-", "_" and ".".';
  return null;
}

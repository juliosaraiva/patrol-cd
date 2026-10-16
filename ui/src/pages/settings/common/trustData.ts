/**
 * Parsing and validation of trust material pasted into Settings: project role policies,
 * SSH known_hosts lines, PEM certificates and ASCII-armored GnuPG keys. Only shape checks; the
 * server parses and verifies the data again.
 */

// Project role policies -------------------------------------------------------------------------

/** Resources a project role policy may name (upstream ProjectScoped). */
export const PROJECT_POLICY_RESOURCES = ['applications', 'applicationsets', 'repositories', 'clusters', 'logs', 'exec'] as const;
const BASIC_ACTIONS = new Set(['get', 'create', 'update', 'delete', 'sync', 'override', '*']);

export function isValidPolicyAction(action: string): boolean {
  return BASIC_ACTIONS.has(action) || /^(action|update|delete)\/.*/.test(action);
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Validates one policy line of a project role: `p, proj:<project>:<role>, <resource>, <action>,
 * <project>[/<namespace>]/<object>, allow|deny`. Returns a message or null.
 */
export function validateProjectPolicy(project: string, role: string, policy: string): string | null {
  const parts = policy.split(',').map((p) => p.trim());
  if (parts.length !== 6 || parts[0] !== 'p') return 'Use the form "p, subject, resource, action, object, allow|deny".';
  const [, subject, resource = '', action = '', object = '', effect] = parts;
  const expected = `proj:${project}:${role}`;
  if (subject !== expected) return `The subject must be "${expected}".`;
  if (!(PROJECT_POLICY_RESOURCES as readonly string[]).includes(resource)) {
    return `The resource must be one of ${PROJECT_POLICY_RESOURCES.join(', ')}.`;
  }
  if (!isValidPolicyAction(action)) return `"${action}" is not a valid action (get, create, update, delete, sync, override, action/*, *).`;
  const objectPattern = new RegExp(`^${escapeRegExp(project)}(/[*\\w.-]+)?/[*\\w.-]+$`);
  if (!objectPattern.test(object)) return `The object must look like "${project}/*" or "${project}/<namespace>/<name>".`;
  if (effect !== 'allow' && effect !== 'deny') return 'The effect must be "allow" or "deny".';
  return null;
}

const ROLE_NAME = /^[a-zA-Z0-9]([-_a-zA-Z0-9]*[a-zA-Z0-9])?$/;

export function validateRoleName(name: string): string | null {
  if (name.trim() === '') return 'Role name is required.';
  if (!ROLE_NAME.test(name)) return 'Use letters, digits, "-" and "_", starting and ending with a letter or digit.';
  return null;
}

/** OIDC group names: no quotes, tabs or line breaks; commas only inside double quotes. */
export function validateGroupName(raw: string): string | null {
  let name = raw.trim();
  if (name.length > 1 && name.startsWith('"') && name.endsWith('"')) name = name.slice(1, -1);
  else if (name.includes(',')) return 'Group names containing "," must be wrapped in double quotes.';
  if (name === '') return 'Enter a group name.';
  if (/["\n\r\t]/.test(name)) return 'Group names may not contain quotes, tabs or line breaks.';
  if (raw !== raw.trim()) return 'Remove leading or trailing spaces.';
  return null;
}

// SSH known hosts -------------------------------------------------------------------------------

export const SSH_KEY_TYPES = [
  'ssh-rsa',
  'ssh-dss',
  'ssh-ed25519',
  'ecdsa-sha2-nistp256',
  'ecdsa-sha2-nistp384',
  'ecdsa-sha2-nistp521',
  'sk-ssh-ed25519@openssh.com',
  'sk-ecdsa-sha2-nistp256@openssh.com',
] as const;

export interface KnownHostEntry {
  line: number;
  hosts: string[];
  keyType: string;
  key: string;
  /** The entry as sent to the server: "<hosts> <type> <key>". */
  text: string;
}

export interface KnownHostsParse {
  entries: KnownHostEntry[];
  errors: { line: number; message: string }[];
}

function isBase64(value: string): boolean {
  return /^[A-Za-z0-9+/]+={0,2}$/.test(value) && value.length % 4 === 0;
}

/** Parses known_hosts text; comments and blank lines are skipped, one error per bad line. */
export function parseKnownHosts(text: string): KnownHostsParse {
  const entries: KnownHostEntry[] = [];
  const errors: { line: number; message: string }[] = [];
  text.split(/\r?\n/).forEach((raw, index) => {
    const line = index + 1;
    const trimmed = raw.trim();
    if (trimmed === '' || trimmed.startsWith('#')) return;
    const fields = trimmed.split(/\s+/);
    if (fields[0]?.startsWith('@')) {
      errors.push({ line, message: `Markers such as ${fields[0]} are not supported.` });
      return;
    }
    if (fields.length < 3) {
      errors.push({ line, message: 'Expected "<host>[,<host>…] <key type> <base64 key>".' });
      return;
    }
    const [hostField = '', keyType = '', key = ''] = fields;
    if (hostField.startsWith('|')) {
      errors.push({ line, message: 'Hashed host names are not supported; use plain host names (ssh-keyscan without -H).' });
      return;
    }
    if (!(SSH_KEY_TYPES as readonly string[]).includes(keyType)) {
      errors.push({ line, message: `Unknown key type "${keyType}".` });
      return;
    }
    if (!isBase64(key)) {
      errors.push({ line, message: 'The key is not valid base64.' });
      return;
    }
    const hosts = hostField.split(',').filter(Boolean);
    entries.push({ line, hosts, keyType, key, text: `${hostField} ${keyType} ${key}` });
  });
  return { entries, errors };
}

/** OpenSSH SHA256 fingerprint ("SHA256:…") of a base64 public key; null without WebCrypto. */
export async function sshFingerprint(base64Key: string): Promise<string | null> {
  try {
    const subtle = globalThis.crypto?.subtle;
    if (!subtle) return null;
    const bytes = Uint8Array.from(atob(base64Key), (c) => c.charCodeAt(0));
    const digest = new Uint8Array(await subtle.digest('SHA-256', bytes));
    let binary = '';
    digest.forEach((b) => {
      binary += String.fromCharCode(b);
    });
    return `SHA256:${btoa(binary).replace(/=+$/, '')}`;
  } catch {
    return null;
  }
}

// PEM certificates and GnuPG keys --------------------------------------------------------------

const PEM_BLOCK = /-----BEGIN ([A-Z0-9 ]+)-----\s*([A-Za-z0-9+/=\s]*?)\s*-----END \1-----/g;

/** Validates PEM text holding one or more certificates; returns the count or an error. */
export function checkPEMCertificates(text: string): { count: number; error: string | null } {
  const value = text.trim();
  if (value === '') return { count: 0, error: 'Paste one or more PEM certificates.' };
  if (value.includes('PRIVATE KEY-----')) {
    return { count: 0, error: 'This looks like a private key. Paste only the public certificate (BEGIN CERTIFICATE).' };
  }
  let count = 0;
  for (const match of value.matchAll(PEM_BLOCK)) {
    if (match[1] !== 'CERTIFICATE') return { count: 0, error: `Unexpected "${match[1] ?? ''}" block; only CERTIFICATE blocks are accepted.` };
    if ((match[2] ?? '').replace(/\s/g, '') === '') return { count: 0, error: 'A certificate block is empty.' };
    count += 1;
  }
  if (count === 0) return { count: 0, error: 'No certificate found. PEM data starts with "-----BEGIN CERTIFICATE-----".' };
  return { count, error: null };
}

/** Validates an ASCII-armored public key block. */
export function checkArmoredPublicKey(text: string): string | null {
  const value = text.trim();
  if (value === '') return 'Paste an ASCII-armored public key.';
  if (value.includes('BEGIN PGP PRIVATE KEY BLOCK')) return 'This is a private key. Export the public key with "gpg --armor --export <key ID>".';
  if (!/-----BEGIN PGP PUBLIC KEY BLOCK-----[\s\S]+-----END PGP PUBLIC KEY BLOCK-----/.test(value)) {
    return 'The key must be ASCII-armored, starting with "-----BEGIN PGP PUBLIC KEY BLOCK-----".';
  }
  return null;
}

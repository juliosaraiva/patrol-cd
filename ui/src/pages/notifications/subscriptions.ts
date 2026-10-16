/**
 * Notification subscription annotations on Applications (NOTIF-047, NOTIF-048, NOTIF-109).
 *
 *   notifications.cd.patrol.example/subscribe.<trigger>.<service>: "<recipient>[;<recipient>...]"
 *   notifications.cd.patrol.example/subscribe.<service>:           recipients for default triggers
 *   notifications.cd.patrol.example/subscriptions:                 YAML list (several triggers/services)
 *
 * The engine splits the key at the first "." after "subscribe.", so trigger names cannot
 * contain dots while service names may. Only trigger+service keys are edited row by row; the
 * other two forms are shown read-only (edit them as raw annotations).
 */

export const NOTIFICATIONS_GROUP = 'notifications.cd.patrol.example';
export const SUBSCRIBE_PREFIX = `${NOTIFICATIONS_GROUP}/subscribe.`;
export const SUBSCRIPTIONS_ANNOTATION = `${NOTIFICATIONS_GROUP}/subscriptions`;

export interface SubscriptionRow {
  /** Stable id for React keys and focus management. */
  id: string;
  trigger: string;
  service: string;
  recipients: string;
}

export interface ParsedSubscriptions {
  rows: SubscriptionRow[];
  /** subscribe.<service> keys: recipients subscribed to the service's default triggers. */
  defaultTriggerSubscriptions: { key: string; service: string; recipients: string }[];
  /** Raw YAML of the subscriptions annotation, when present. */
  yaml: string | null;
}

let nextId = 0;
export function newRowId(): string {
  nextId += 1;
  return `subscription-${nextId}`;
}

export function subscriptionKey(trigger: string, service: string): string {
  return `${SUBSCRIBE_PREFIX}${trigger}.${service}`;
}

export function parseSubscriptions(annotations: Record<string, string> | undefined): ParsedSubscriptions {
  const rows: SubscriptionRow[] = [];
  const defaults: ParsedSubscriptions['defaultTriggerSubscriptions'] = [];
  let yaml: string | null = null;
  for (const key of Object.keys(annotations ?? {}).sort()) {
    const value = annotations?.[key] ?? '';
    if (key === SUBSCRIPTIONS_ANNOTATION) {
      yaml = value;
      continue;
    }
    if (!key.startsWith(SUBSCRIBE_PREFIX)) continue;
    const rest = key.slice(SUBSCRIBE_PREFIX.length);
    const dot = rest.indexOf('.');
    if (dot === -1) {
      if (rest !== '') defaults.push({ key, service: rest, recipients: value });
      continue;
    }
    const trigger = rest.slice(0, dot);
    const service = rest.slice(dot + 1);
    if (trigger === '' || service === '') continue;
    rows.push({ id: newRowId(), trigger, service, recipients: value });
  }
  return { rows, defaultTriggerSubscriptions: defaults, yaml };
}

/** Recipients as the engine reads them: split on ";", trimmed, empty entries dropped. */
export function splitRecipients(value: string): string[] {
  return value
    .split(';')
    .map((r) => r.trim())
    .filter((r) => r !== '');
}

/** Canonical annotation value for a row ("a;b"). */
export function formatRecipients(value: string): string {
  return splitRecipients(value).join(';');
}

const TRIGGER_RE = /^[A-Za-z0-9](?:[A-Za-z0-9_-]*[A-Za-z0-9])?$/;
const SERVICE_RE = /^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$/;
const EMAIL_RE = /^[^\s@;]+@[^\s@;]+\.[^\s@;]+$/;
/** Kubernetes limits the name part of an annotation key (after the "/") to 63 characters. */
const MAX_KEY_NAME = 63;
const MAX_RECIPIENT = 512;
/** Services that deliver without a recipient (an empty annotation value). */
const NO_RECIPIENT_SERVICES = /^(webhook|github|awssqs|gcppubsub|alertmanager|grafana|newrelic|pagerdutyv2|nats)\b/i;
const CHAT_SERVICES = /^(slack|mattermost|rocketchat|googlechat|teams|webex|telegram)\b/i;

export interface RowErrors {
  trigger?: string;
  service?: string;
  recipients?: string;
}

export interface RowWarnings {
  trigger?: string;
  service?: string;
  recipients?: string;
}

/** Recipient errors for one row; null when valid. */
export function validateRecipients(service: string, value: string): string | null {
  if (/[\r\n\t]/.test(value)) return 'Recipients cannot contain line breaks or tabs. Separate recipients with ";".';
  const parts = value.split(';').map((r) => r.trim());
  // A trailing ";" is harmless; an empty entry between two recipients is a typo.
  if (parts.slice(0, -1).some((p) => p === '') && splitRecipients(value).length > 0) {
    return 'Remove the empty recipient between two semicolons.';
  }
  const recipients = splitRecipients(value);
  const tooLong = recipients.find((r) => r.length > MAX_RECIPIENT);
  if (tooLong) return `Recipients can have at most ${MAX_RECIPIENT} characters.`;
  const seen = new Set<string>();
  for (const r of recipients) {
    if (seen.has(r)) return `"${r}" is listed twice.`;
    seen.add(r);
  }
  if (/^email\b/i.test(service)) {
    const bad = recipients.find((r) => !EMAIL_RE.test(r));
    if (bad) return `"${bad}" is not an email address.`;
  }
  if (CHAT_SERVICES.test(service)) {
    const bad = recipients.find((r) => /\s/.test(r));
    if (bad) return `"${bad}" contains spaces; use the channel or chat name without spaces.`;
  }
  return null;
}

/**
 * Validates every row. `known` holds the configured trigger and service names (when the
 * catalog loaded): unknown names are warnings, not errors, because the catalog may be
 * unavailable to this user and deliveries are resolved by the controller.
 */
export function validateRows(
  rows: readonly SubscriptionRow[],
  known: { triggers?: readonly string[] | undefined; services?: readonly string[] | undefined } = {},
): { errors: Map<string, RowErrors>; warnings: Map<string, RowWarnings>; valid: boolean } {
  const errors = new Map<string, RowErrors>();
  const warnings = new Map<string, RowWarnings>();
  const pairs = new Map<string, number>();
  rows.forEach((row, index) => {
    const e: RowErrors = {};
    const w: RowWarnings = {};
    const trigger = row.trigger.trim();
    const service = row.service.trim();
    if (trigger === '') e.trigger = 'Choose a trigger.';
    else if (trigger.includes('.')) e.trigger = 'Trigger names in subscription keys cannot contain dots.';
    else if (!TRIGGER_RE.test(trigger)) e.trigger = 'Use letters, digits, "-" and "_"; start and end with a letter or digit.';
    else if (known.triggers && known.triggers.length > 0 && !known.triggers.includes(trigger)) {
      w.trigger = `"${trigger}" is not a configured trigger; nothing is sent until it is defined.`;
    }
    if (service === '') e.service = 'Choose a service.';
    else if (!SERVICE_RE.test(service)) e.service = 'Use letters, digits, ".", "-" and "_"; start and end with a letter or digit.';
    else if (known.services && known.services.length > 0 && !known.services.includes(service)) {
      w.service = `"${service}" is not a configured service; deliveries fail until it is defined.`;
    }
    if (!e.trigger && !e.service) {
      const name = subscriptionKey(trigger, service).split('/')[1] ?? '';
      if (name.length > MAX_KEY_NAME) {
        e.service = `"subscribe.${trigger}.${service}" has ${name.length} characters; annotation names allow ${MAX_KEY_NAME}.`;
      }
      const pair = `${trigger}\u0000${service}`;
      const first = pairs.get(pair);
      if (first !== undefined) e.service = `Duplicates subscription ${first + 1}; combine the recipients in one row.`;
      else pairs.set(pair, index);
    }
    const recipientError = validateRecipients(service, row.recipients);
    if (recipientError) e.recipients = recipientError;
    else if (splitRecipients(row.recipients).length === 0 && service !== '' && !NO_RECIPIENT_SERVICES.test(service)) {
      w.recipients = 'No recipient: only services that need none (webhook, GitHub) deliver anything.';
    }
    if (Object.keys(e).length > 0) errors.set(row.id, e);
    if (Object.keys(w).length > 0) warnings.set(row.id, w);
  });
  return { errors, warnings, valid: errors.size === 0 };
}

/**
 * Merge-patch annotations turning `before` rows into `after`: removed keys map to null, new or
 * changed keys to their canonical value. Other annotations are untouched.
 */
export function subscriptionPatch(before: readonly SubscriptionRow[], after: readonly SubscriptionRow[]): Record<string, string | null> {
  const old = new Map(before.map((r) => [subscriptionKey(r.trigger, r.service), formatRecipients(r.recipients)]));
  const next = new Map(after.map((r) => [subscriptionKey(r.trigger.trim(), r.service.trim()), formatRecipients(r.recipients)]));
  const patch: Record<string, string | null> = {};
  for (const key of old.keys()) if (!next.has(key)) patch[key] = null;
  for (const [key, value] of next) if (old.get(key) !== value) patch[key] = value;
  return patch;
}

/**
 * Descriptions of the default catalog triggers (derived from the Argo CD v3.5.5 notifications
 * catalog, Apache-2.0; see internal/notifications/catalog). A ConfigMap may redefine them.
 */
export const CATALOG_TRIGGER_DESCRIPTIONS: Readonly<Record<string, string>> = {
  'on-created': 'Application is created.',
  'on-deleted': 'Application is deleted.',
  'on-deployed': 'Application is synced and healthy. Triggered once per commit.',
  'on-health-degraded': 'Application has degraded',
  'on-sync-failed': 'Application syncing has failed',
  'on-sync-running': 'Application is being synced',
  'on-sync-status-unknown': "Application status is 'Unknown'",
  'on-sync-succeeded': 'Application syncing has succeeded',
};

// Smoke test of the built bundle: evaluates dist/extension-backup-schedule.js the way the UI
// loads extensions.js (a classic script with the host globals), checks the registration and
// renders the tab to HTML. Run after `pnpm build`: `pnpm smoke`.
import { readFileSync } from 'node:fs';
import React from 'react';
import * as ReactJSXRuntime from 'react/jsx-runtime';
import { renderToStaticMarkup } from 'react-dom/server';

const registrations = [];
globalThis.window = {
  React,
  ReactJSXRuntime,
  extensionsAPI: {
    registerResourceExtension: (component, group, kind, title, opts) => registrations.push({ component, group, kind, title, opts }),
  },
};

new Function(readFileSync(new URL('../dist/extension-backup-schedule.js', import.meta.url), 'utf8'))();

if (registrations.length !== 1) {
  throw new Error(`expected 1 registration, got ${registrations.length}`);
}
const [reg] = registrations;
if (reg.group !== 'ops.example.com' || reg.kind !== 'BackupSchedule' || reg.title !== 'Backups') {
  throw new Error(`unexpected registration ${JSON.stringify({ group: reg.group, kind: reg.kind, title: reg.title })}`);
}

const resource = {
  apiVersion: 'ops.example.com/v1alpha1',
  kind: 'BackupSchedule',
  metadata: { name: 'orders-db', namespace: 'backups', generation: 2 },
  spec: { schedule: '0 2 * * *', target: 'orders-db', retentionDays: 7 },
  status: {
    observedGeneration: 1,
    lastBackupResult: 'Succeeded',
    lastBackupTime: '2026-10-15T02:00:00Z',
    conditions: [{ type: 'Ready', status: 'True', reason: 'Scheduled' }],
  },
};
const html = renderToStaticMarkup(React.createElement(reg.component, { resource, application: {}, tree: {} }));
for (const want of ['orders-db', '0 2 * * *', 'Succeeded', 'not observed the latest spec', 'Show conditions (1)']) {
  if (!html.includes(want)) {
    throw new Error(`rendered tab misses "${want}": ${html}`);
  }
}
console.log(`ok: ${reg.group}/${reg.kind} tab "${reg.title}" registered and rendered (${html.length} bytes)`);

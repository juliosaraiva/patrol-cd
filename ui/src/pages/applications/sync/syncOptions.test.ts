import { describe, expect, it } from 'vitest';
import { guestbookApp, paymentsApp } from '../../../mocks/fixtures/apps-core';
import {
  buildSyncRequest,
  initialSyncForm,
  isBooleanOptionChecked,
  REPLACE_OPTION,
  retryFormFrom,
  retryFromForm,
  setBooleanOption,
  setSelectOption,
  SYNC_OPTIONS,
  syncOptionCatalog,
  validateRetry,
  type BooleanSyncOption,
  type SelectSyncOption,
} from './syncOptions';

const validate = SYNC_OPTIONS.find((o) => o.key === 'Validate') as BooleanSyncOption;
const prune = SYNC_OPTIONS.find((o) => o.key === 'Prune') as SelectSyncOption;

describe('sync options', () => {
  it('writes inverted boolean options and keeps unknown ones', () => {
    const options = setBooleanOption(['Custom=1'], validate, true);
    expect(options).toEqual(['Custom=1', 'Validate=false']);
    expect(isBooleanOptionChecked(options, validate)).toBe(true);
    expect(setBooleanOption(options, validate, false)).toEqual(['Custom=1']);
  });

  it('omits select options at their default', () => {
    expect(setSelectOption([], prune, 'confirm')).toEqual(['Prune=confirm']);
    expect(setSelectOption(['Prune=confirm'], prune, 'true')).toEqual([]);
  });

  it('offers Replace only when the server allows it', () => {
    expect(syncOptionCatalog(false)).not.toContain(REPLACE_OPTION);
    expect(syncOptionCatalog(true)).toContain(REPLACE_OPTION);
  });

  it('validates and converts retry settings', () => {
    const form = { ...retryFormFrom(undefined), enabled: true, limit: 'x', duration: '5 seconds', factor: '0.5', maxDuration: '3m' };
    expect(Object.keys(validateRetry(form)).sort()).toEqual(['duration', 'factor', 'limit']);
    expect(retryFromForm({ ...form, limit: '-1', duration: '1m30s', factor: '2', refresh: true })).toEqual({
      limit: -1,
      backoff: { duration: '1m30s', factor: 2, maxDuration: '3m' },
      refresh: true,
    });
    expect(retryFromForm({ ...form, enabled: false })).toBeUndefined();
    expect(retryFormFrom({ limit: 3, backoff: { duration: '10s' } })).toMatchObject({ enabled: true, limit: '3', duration: '10s' });
  });
});

describe('sync request (API-098)', () => {
  it('sends a plain full sync with the application options', () => {
    const app = guestbookApp();
    expect(buildSyncRequest(app, initialSyncForm(app))).toEqual({
      prune: false,
      dryRun: false,
      strategy: { hook: { force: false } },
      syncOptions: { items: ['CreateNamespace=true'] },
    });
  });

  it('sends empty sync options when the user clears the options the application declares', () => {
    const app = guestbookApp();
    expect(buildSyncRequest(app, { ...initialSyncForm(app), syncOptions: [] }).syncOptions).toEqual({ items: [] });
    const plain = { ...app, spec: { ...app.spec, syncPolicy: {} } };
    expect(buildSyncRequest(plain, initialSyncForm(plain)).syncOptions).toBeUndefined();
  });

  it('sends a revision override, apply strategy, selected resources and retry', () => {
    const app = guestbookApp();
    const form = {
      ...initialSyncForm(app),
      revisions: ['v2.0.0'],
      prune: true,
      applyOnly: true,
      force: true,
      resources: [{ group: 'apps', kind: 'Deployment', namespace: 'guestbook', name: 'guestbook-ui' }, { kind: 'Namespace', name: 'guestbook' }],
      retry: { ...retryFormFrom(undefined), enabled: true },
    };
    expect(buildSyncRequest(app, form)).toEqual({
      prune: true,
      dryRun: false,
      revision: 'v2.0.0',
      strategy: { apply: { force: true } },
      syncOptions: { items: ['CreateNamespace=true'] },
      resources: [
        { group: 'apps', kind: 'Deployment', namespace: 'guestbook', name: 'guestbook-ui' },
        { kind: 'Namespace', name: 'guestbook' },
      ],
      retryStrategy: { limit: 5, backoff: { duration: '5s', factor: 2, maxDuration: '3m' } },
    });
  });

  it('sends every revision of a multi-source application when one changes', () => {
    const app = paymentsApp();
    const form = initialSyncForm(app);
    expect(buildSyncRequest(app, form).revisions).toBeUndefined();
    const request = buildSyncRequest(app, { ...form, revisions: ['1.5.0', 'main'] });
    expect(request.revisions).toEqual(['1.5.0', 'main']);
    expect(request.revision).toBeUndefined();
  });
});

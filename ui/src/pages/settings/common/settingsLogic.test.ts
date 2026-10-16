import { describe, expect, it } from 'vitest';
import { formFromRepository, toCredentialRequest, toRepositoryRequest, validateRepoForm, emptyForm, connectionGuidance, suggestedPrefix, methodOf } from '../repositories/model';
import { repositoriesFixture, scmProvidersFixture } from '../../../mocks/fixtures/settings';
import { emptyProviderForm, formFromProvider, readiness, toProviderRequest, validateProviderForm } from '../scmproviders/model';
import { describeCron, nextRuns, parseCron, windowState } from './cron';
import { formatDuration, parseGoDuration, parseTokenLifetime } from './duration';
import { checkArmoredPublicKey, checkPEMCertificates, parseKnownHosts, sshFingerprint, validateGroupName, validateProjectPolicy, validateRoleName } from './trustData';
import { validateHTTPURL, validateLabelKey, validateObjectName, validateRepoURL } from './validation';

function schedule(expr: string) {
  const r = parseCron(expr);
  if (!r.ok) throw new Error(r.error);
  return r.schedule;
}

describe('cron', () => {
  it('parses fields, ranges, steps, lists, names and descriptors', () => {
    const s = schedule('*/15 9-17 * JAN,jul MON-FRI');
    expect(s.minutes).toEqual([0, 15, 30, 45]);
    expect(s.hours).toEqual([9, 10, 11, 12, 13, 14, 15, 16, 17]);
    expect([...s.months].sort((a, b) => a - b)).toEqual([1, 7]);
    expect([...s.weekdays].sort()).toEqual([1, 2, 3, 4, 5]);
    expect(s.daysStar).toBe(true);
    expect(schedule('@daily')).toEqual(schedule('0 0 * * *'));
    expect(schedule('5/20 * * * *').minutes).toEqual([5, 25, 45]);
  });

  it('rejects invalid expressions with a reason', () => {
    expect(parseCron('61 * * * *')).toEqual({ ok: false, error: 'Minute must be between 0 and 59.' });
    expect(parseCron('* * * *')).toMatchObject({ ok: false, error: expect.stringContaining('Expected 5 fields') as unknown });
    expect(parseCron('@every 1h')).toMatchObject({ ok: false });
    expect(parseCron('0 0 ? * *').ok).toBe(true);
    expect(parseCron('? 0 * * *').ok).toBe(false);
    expect(parseCron('5-1 * * * *')).toMatchObject({ ok: false });
  });

  it('computes the next runs in a time zone, across DST changes', () => {
    // 2026-03-29 01:00 UTC: Europe/Berlin switches to CEST (UTC+2).
    const runs = nextRuns(schedule('0 22 * * *'), Date.UTC(2026, 2, 27, 12, 0), 3, 'Europe/Berlin');
    expect(runs.map((d) => d.toISOString())).toEqual(['2026-03-27T21:00:00.000Z', '2026-03-28T21:00:00.000Z', '2026-03-29T20:00:00.000Z']);
    expect(nextRuns(schedule('0 0 29 2 *'), Date.UTC(2026, 0, 1), 1)[0]?.toISOString()).toBe('2028-02-29T00:00:00.000Z');
  });

  it('uses cron day matching: either day field when both are restricted', () => {
    const runs = nextRuns(schedule('0 0 1 * MON'), Date.UTC(2026, 5, 1, 1), 3);
    // June 2026: the 1st is a Monday; next matches are Mondays 8 and 15 (or a 1st).
    expect(runs.map((d) => d.getUTCDate())).toEqual([8, 15, 22]);
  });

  it('reports whether a window is open and when it closes', () => {
    const s = schedule('0 22 * * *');
    const eightHours = 8 * 3600_000;
    const open = windowState(s, eightHours, Date.UTC(2026, 4, 10, 23, 30), 'UTC');
    expect(open.active).toBe(true);
    expect(open.activeUntil?.toISOString()).toBe('2026-05-11T06:00:00.000Z');
    const closed = windowState(s, eightHours, Date.UTC(2026, 4, 10, 12), 'UTC');
    expect(closed.active).toBe(false);
    expect(closed.upcoming[0]?.start.toISOString()).toBe('2026-05-10T22:00:00.000Z');
  });

  it('describes common schedules', () => {
    expect(describeCron('0 9 * * 1-5')).toBe('Monday to Friday at 09:00');
    expect(describeCron('@weekly')).toBe('At midnight every Sunday');
    expect(describeCron('*/5 * * * *')).toBeNull();
  });
});

describe('durations', () => {
  it('parses Go durations', () => {
    expect(parseGoDuration('1h30m')).toBe(5_400_000);
    expect(parseGoDuration('1.5h')).toBe(5_400_000);
    expect(parseGoDuration('300ms')).toBe(300);
    expect(parseGoDuration('0')).toBe(0);
    expect(parseGoDuration('90')).toBeNull();
    expect(parseGoDuration('1d')).toBeNull();
    expect(parseGoDuration('')).toBeNull();
  });

  it('parses token lifetimes in seconds', () => {
    expect(parseTokenLifetime('')).toBe(0);
    expect(parseTokenLifetime('7d')).toBe(604_800);
    expect(parseTokenLifetime('1w2d')).toBe(777_600);
    expect(parseTokenLifetime('3600')).toBe(3600);
    expect(parseTokenLifetime('soon')).toBeNull();
    expect(formatDuration(90_061_000)).toBe('1d 1h 1m 1s');
  });
});

describe('validation', () => {
  it('validates names, URLs and label keys', () => {
    expect(validateObjectName('team-a')).toBeNull();
    expect(validateObjectName('Team_A')).toMatch(/lowercase/);
    expect(validateHTTPURL('ftp://x', 'API URL')).toMatch(/https:\/\//);
    expect(validateHTTPURL('', 'API URL', false)).toBeNull();
    expect(validateRepoURL('git@git.example.test:org/repo.git', 'ssh')).toBeNull();
    expect(validateRepoURL('https://git.example.test/org/repo.git', 'ssh')).toMatch(/SSH URL/);
    expect(validateRepoURL('registry.example.test/charts', 'helm')).toBeNull();
    expect(validateRepoURL('oci://registry.example.test/x', 'oci')).toBeNull();
    expect(validateLabelKey('team.example.test/owner')).toBeNull();
    expect(validateLabelKey('a/b/c')).toMatch(/at most one/);
  });
});

describe('trust data', () => {
  it('validates project role policies like the server', () => {
    expect(validateProjectPolicy('finance', 'ci', 'p, proj:finance:ci, applications, sync, finance/*, allow')).toBeNull();
    expect(validateProjectPolicy('finance', 'ci', 'p, proj:finance:ci, applications, action/apps/Deployment/restart, finance/ns/app, deny')).toBeNull();
    expect(validateProjectPolicy('finance', 'ci', 'p, proj:finance:ci, certificates, get, finance/*, allow')).toMatch(/resource must be one of/);
    expect(validateProjectPolicy('finance', 'ci', 'p, proj:finance:ci, applications, launch, finance/*, allow')).toMatch(/not a valid action/);
    expect(validateProjectPolicy('finance', 'ci', 'p, proj:finance:ci, applications, get, other/*, allow')).toMatch(/object must look like/);
    expect(validateProjectPolicy('finance', 'ci', 'p, proj:finance:ci, applications, get, finance/*, maybe')).toMatch(/effect/);
    expect(validateProjectPolicy('finance', 'ci', 'g, x, y')).toMatch(/Use the form/);
    expect(validateRoleName('ci-bot_1')).toBeNull();
    expect(validateRoleName('-bad')).not.toBeNull();
    expect(validateGroupName('"a,b"')).toBeNull();
    expect(validateGroupName(' lead')).toMatch(/leading or trailing/);
  });

  it('parses known_hosts lines and reports bad lines', async () => {
    const key = 'AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl';
    const parsed = parseKnownHosts(`# comment\n\na.example,b.example ssh-ed25519 ${key}\n@revoked x ssh-rsa AAAA\nc.example ssh-foo AAAA\nd.example ssh-rsa not*base64`);
    expect(parsed.entries).toEqual([{ line: 3, hosts: ['a.example', 'b.example'], keyType: 'ssh-ed25519', key, text: `a.example,b.example ssh-ed25519 ${key}` }]);
    expect(parsed.errors.map((e) => e.line)).toEqual([4, 5, 6]);
    expect(await sshFingerprint(key)).toBe('SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU');
  });

  it('checks PEM certificates and armored keys', () => {
    expect(checkPEMCertificates('-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----\nREVG\n-----END CERTIFICATE-----')).toEqual({ count: 2, error: null });
    expect(checkPEMCertificates('-----BEGIN RSA PRIVATE KEY-----').error).toMatch(/private key/);
    expect(checkPEMCertificates('hello').error).toMatch(/No certificate found/);
    expect(checkArmoredPublicKey('-----BEGIN PGP PUBLIC KEY BLOCK-----\nx\n-----END PGP PUBLIC KEY BLOCK-----')).toBeNull();
    expect(checkArmoredPublicKey('xyz')).toMatch(/ASCII-armored/);
  });
});

describe('repository form model', () => {
  it('derives the method of existing repositories and keeps secrets write-only', () => {
    const [apps, infra, stable, registry] = repositoriesFixture();
    if (!apps || !infra || !stable || !registry) throw new Error('fixtures');
    expect([apps, infra, stable, registry].map((r) => methodOf(r.spec, r.spec.url))).toEqual(['https', 'ssh', 'helm', 'oci']);
    const form = formFromRepository(apps);
    expect(form.password).toBe('');
    const request = toRepositoryRequest(form, 'read', apps);
    expect(request.credentials).toBeUndefined();
    // An update may repeat the server-managed credentialsRef unchanged (never set a new one).
    expect(request.spec.credentialsRef).toEqual({ name: 'repo-apps' });
    expect(request.metadata?.resourceVersion).toBe('10');
    expect(toRepositoryRequest(form, 'read').spec.credentialsRef).toBeUndefined();
  });

  it('builds method-specific specs and credentials', () => {
    const ssh = { ...emptyForm('ssh'), url: 'git@x:y.git', sshPrivateKey: '-----BEGIN OPENSSH PRIVATE KEY-----\n', insecureIgnoreHostKey: true, depth: '1' };
    expect(toRepositoryRequest(ssh, 'write')).toEqual({
      metadata: {},
      spec: { type: 'git', url: 'git@x:y.git', insecureIgnoreHostKey: true, depth: 1, purpose: 'write' },
      credentials: { sshPrivateKey: '-----BEGIN OPENSSH PRIVATE KEY-----\n' },
    });
    const helm = { ...emptyForm('helm'), url: 'registry.example.test/charts', name: 'charts', enableOCI: true, insecureOCIForceHTTP: true, httpsAuth: 'none' as const };
    expect(toRepositoryRequest(helm, 'read').spec).toEqual({ type: 'helm', url: 'registry.example.test/charts', name: 'charts', enableOCI: true, insecureOCIForceHTTP: true });
    const azure = { ...emptyForm('azure'), url: 'https://dev.azure.com/o/p/_git/r', azureTenantID: 't', azureClientID: 'c', azureClientSecret: 's' };
    expect(toCredentialRequest({ ...azure, url: 'https://dev.azure.com/o' }, 'read')).toEqual({
      metadata: {},
      spec: { type: 'git', urlPrefix: 'https://dev.azure.com/o', azureServicePrincipal: { tenantID: 't', clientID: 'c' } },
      credentials: { azureServicePrincipalClientSecret: 's' },
    });
  });

  it('validates per method and mode', () => {
    expect(validateRepoForm({ ...emptyForm('helm'), url: 'https://charts.example.test' }, { editing: false, mode: 'repository' }).map((e) => e.id)).toEqual(['repo-name']);
    expect(validateRepoForm({ ...emptyForm('gcp'), url: 'https://source.developers.google.com/p/x/r/y', gcpServiceAccountKey: '{"type":"user"}' }, { editing: false, mode: 'repository' })[0]?.message).toMatch(/service_account/);
    expect(validateRepoForm({ ...emptyForm('githubApp'), url: 'https://github.com/a/b', appID: 'x' }, { editing: true, mode: 'repository' }).map((e) => e.id)).toEqual(['repo-appID']);
    expect(validateRepoForm({ ...emptyForm('https'), url: 'https://x/y', tlsClientCertData: 'cert' }, { editing: false, mode: 'repository' })[0]?.message).toMatch(/both/);
    expect(validateRepoForm({ ...emptyForm('https'), url: 'https://x' }, { editing: false, mode: 'template' })).toEqual([]);
  });

  it('suggests prefixes and guidance', () => {
    expect(suggestedPrefix('https://git.example.test/team/app.git')).toBe('https://git.example.test/team');
    expect(suggestedPrefix('https://git.example.test')).toBe('https://git.example.test');
    expect(connectionGuidance('x509: certificate signed by unknown authority')).toMatch(/TLS certificate/);
    expect(connectionGuidance('knownhosts: key is unknown')).toMatch(/host key/);
  });
});

describe('SCM provider form model', () => {
  it('round-trips an existing provider without secret values', () => {
    const [github] = scmProvidersFixture();
    if (!github) throw new Error('fixture');
    const form = formFromProvider(github);
    expect(form.auth).toBe('githubApp');
    const request = toProviderRequest(form, github);
    expect(request.spec).toEqual(github.spec);
    expect(Object.keys(request).sort()).toEqual(['metadata', 'spec']);
    expect(readiness(github)).toEqual({ status: 'Successful', message: 'Authenticated: GitHub App installation token issued' });
  });

  it('requires secret references; writes carry only secretRefs', () => {
    const errors = validateProviderForm({ ...emptyProviderForm(), name: 'x', token: { name: '', key: '' } }, false);
    expect(errors.map((e) => e.id)).toEqual(['scm-token-name', 'scm-token-key']);
    const basic = { ...emptyProviderForm(), name: 'bb', type: 'bitbucketServer' as const, apiURL: 'https://bb.example.test', auth: 'basic' as const, basicSecret: 'bb' };
    expect(validateProviderForm(basic, false)).toEqual([]);
    expect(toProviderRequest(basic)).toEqual({
      metadata: { name: 'bb' },
      spec: { type: 'bitbucketServer', apiURL: 'https://bb.example.test', auth: { basicAuthSecretRef: { name: 'bb' } } },
    });
  });
});

import { describe, expect, it } from 'vitest';
import { parseSubscriptions, subscriptionPatch, validateRecipients, validateRows, type SubscriptionRow } from './subscriptions';

const P = 'notifications.cd.patrol.example';

function row(trigger: string, service: string, recipients: string, id = `${trigger}-${service}`): SubscriptionRow {
  return { id, trigger, service, recipients };
}

describe('parseSubscriptions', () => {
  it('splits trigger/service keys at the first dot and keeps other forms read-only', () => {
    const parsed = parseSubscriptions({
      [`${P}/subscribe.on-sync-succeeded.slack`]: 'ops; dev',
      [`${P}/subscribe.on-deployed.my.hook`]: '',
      [`${P}/subscribe.email`]: 'team@example.test',
      [`${P}/subscriptions`]: '- trigger: [on-sync-failed]\n',
      [`${P}/subscribe..slack`]: 'bad',
      'team.example.test/owner': 'web',
    });
    expect(parsed.rows.map((r) => [r.trigger, r.service, r.recipients])).toEqual([
      ['on-deployed', 'my.hook', ''],
      ['on-sync-succeeded', 'slack', 'ops; dev'],
    ]);
    expect(parsed.defaultTriggerSubscriptions).toEqual([{ key: `${P}/subscribe.email`, service: 'email', recipients: 'team@example.test' }]);
    expect(parsed.yaml).toBe('- trigger: [on-sync-failed]\n');
  });
});

describe('validateRecipients', () => {
  it.each([
    ['slack', 'ops;dev', null],
    ['slack', 'ops; dev;', null],
    ['slack', 'ops;;dev', 'Remove the empty recipient between two semicolons.'],
    ['slack', 'my channel', '"my channel" contains spaces; use the channel or chat name without spaces.'],
    ['slack', 'ops;ops', '"ops" is listed twice.'],
    ['email', 'a@example.test; b@example.test', null],
    ['email', 'not-an-email', '"not-an-email" is not an email address.'],
    ['webhook', '', null],
    ['slack', 'a\nb', 'Recipients cannot contain line breaks or tabs. Separate recipients with ";".'],
  ])('%s %j → %j', (service, value, expected) => {
    expect(validateRecipients(service, value)).toBe(expected);
  });
});

describe('validateRows', () => {
  it('requires trigger and service, rejects dots in triggers and duplicates', () => {
    const rows = [row('', '', '', 'a'), row('on.x', 'slack', 'c', 'b'), row('on-x', 'slack', 'c', 'c'), row('on-x', 'slack', 'd', 'd')];
    const { errors, valid } = validateRows(rows);
    expect(valid).toBe(false);
    expect(errors.get('a')).toEqual({ trigger: 'Choose a trigger.', service: 'Choose a service.' });
    expect(errors.get('b')?.trigger).toBe('Trigger names in subscription keys cannot contain dots.');
    expect(errors.get('c')).toBeUndefined();
    expect(errors.get('d')?.service).toBe('Duplicates subscription 3; combine the recipients in one row.');
  });

  it('rejects keys longer than the 63-character annotation name limit', () => {
    const { errors } = validateRows([row('on-' + 'x'.repeat(40), 'slack-' + 'y'.repeat(20), 'c', 'a')]);
    expect(errors.get('a')?.service).toMatch(/annotation names allow 63/);
  });

  it('warns about names missing from the catalog without blocking', () => {
    const { errors, warnings, valid } = validateRows([row('on-custom', 'pager', '', 'a')], { triggers: ['on-deployed'], services: ['slack'] });
    expect(valid).toBe(true);
    expect(errors.size).toBe(0);
    expect(warnings.get('a')).toEqual({
      trigger: '"on-custom" is not a configured trigger; nothing is sent until it is defined.',
      service: '"pager" is not a configured service; deliveries fail until it is defined.',
      recipients: 'No recipient: only services that need none (webhook, GitHub) deliver anything.',
    });
  });
});

describe('subscriptionPatch', () => {
  it('removes deleted keys, writes new and changed ones in canonical form, and leaves the rest', () => {
    const before = [row('on-sync-succeeded', 'slack', 'ops'), row('on-deployed', 'email', 'a@example.test')];
    const after = [row('on-sync-succeeded', 'slack', ' ops ; dev '), row('on-sync-failed', 'slack', 'ops')];
    expect(subscriptionPatch(before, after)).toEqual({
      [`${P}/subscribe.on-deployed.email`]: null,
      [`${P}/subscribe.on-sync-succeeded.slack`]: 'ops;dev',
      [`${P}/subscribe.on-sync-failed.slack`]: 'ops',
    });
    expect(subscriptionPatch(before, before)).toEqual({});
  });
});

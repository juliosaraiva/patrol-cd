import { describe, expect, it } from 'vitest';
import type { ApplicationSet } from '../../api/types';
import { brokenAppSet, generatedApplications, guestbookAppSet, previewsAppSet } from '../../mocks/fixtures/applicationsets';
import {
  applicationPath,
  appSetHealth,
  appSetPath,
  appSetProjects,
  conditionGuidance,
  deletionImpact,
  generatedAppRows,
  generatorLabel,
  generatorSummary,
  hasPullRequestGenerator,
  problemConditions,
} from './model';

function withStatus(status: ApplicationSet['status']): ApplicationSet {
  return { ...guestbookAppSet, status };
}

const cond = (type: string, status: 'True' | 'False' | 'Unknown', reason = 'R') => ({ type, status, reason, message: '', lastTransitionTime: '2026-10-01T00:00:00Z' });

describe('appSetHealth', () => {
  it('uses status.health when reported', () => {
    expect(appSetHealth(brokenAppSet)).toBe('Degraded');
  });

  it('derives health from conditions like upstream', () => {
    expect(appSetHealth(withStatus({}))).toBe('Unknown');
    expect(appSetHealth(withStatus({ conditions: [cond('ErrorOccurred', 'True'), cond('RolloutProgressing', 'True')] }))).toBe('Degraded');
    expect(appSetHealth(withStatus({ conditions: [cond('RolloutProgressing', 'True'), cond('ResourcesUpToDate', 'True')] }))).toBe('Progressing');
    expect(appSetHealth(withStatus({ conditions: [cond('ResourcesUpToDate', 'True')] }))).toBe('Healthy');
    expect(appSetHealth(withStatus({ conditions: [cond('ResourcesUpToDate', 'False')] }))).toBe('Unknown');
  });
});

describe('conditions', () => {
  it('flags problems and gives reason-specific guidance', () => {
    const problems = problemConditions(brokenAppSet);
    expect(problems.map((c) => c.type)).toEqual(['ErrorOccurred', 'ParametersGenerated']);
    expect(conditionGuidance(problems[0]!)).toMatch(/^A generator failed/);
    expect(conditionGuidance(cond('ErrorOccurred', 'False'))).toBeNull();
    expect(conditionGuidance(cond('RolloutProgressing', 'True'))).toMatch(/RollingSync rollout is in progress/);
  });
});

describe('generators', () => {
  it('labels nested generators', () => {
    expect(generatorSummary(brokenAppSet)).toBe('Matrix (Git, Clusters)');
    expect(generatorLabel(guestbookAppSet.spec.generators[0]!)).toBe('List');
    expect(hasPullRequestGenerator(previewsAppSet)).toBe(true);
    expect(hasPullRequestGenerator(guestbookAppSet)).toBe(false);
    expect(appSetProjects(previewsAppSet)).toEqual(['previews']);
  });
});

describe('generatedAppRows', () => {
  it('merges status.resources, live applications and progressive sync status', () => {
    const live = generatedApplications.filter((a) => a.metadata.name?.startsWith('guestbook'));
    const rows = generatedAppRows(guestbookAppSet, live);
    expect(rows.map((r) => [r.name, r.health, r.sync, r.step, r.progressiveStatus])).toEqual([
      ['guestbook-production', 'Progressing', 'OutOfSync', '2', 'Progressing'],
      ['guestbook-staging', 'Healthy', 'Synced', '1', 'Healthy'],
    ]);
    expect(rows[0]?.app?.metadata.name).toBe('guestbook-production');
  });

  it('falls back to status.resources when live applications are unavailable', () => {
    const rows = generatedAppRows(previewsAppSet, []);
    expect(rows.map((r) => [r.name, r.health, r.sync])).toEqual([
      ['shop-pr-42', 'Healthy', 'Synced'],
      ['shop-pr-57', 'Degraded', 'OutOfSync'],
    ]);
  });
});

describe('paths and deletion', () => {
  it('adds the namespace segment only outside the control plane', () => {
    expect(appSetPath(guestbookAppSet, 'patrol')).toBe('/applicationsets/guestbook');
    expect(appSetPath(brokenAppSet, 'patrol')).toBe('/applicationsets/team-infra/clusters-addons');
    expect(applicationPath('a b', 'team', 'patrol')).toBe('/applications/team/a%20b');
  });

  it('explains preserveResourcesOnDeletion', () => {
    expect(deletionImpact(guestbookAppSet)).toMatchObject({ preserve: true, order: null });
    expect(deletionImpact(previewsAppSet).summary).toMatch(/also delete the Kubernetes resources/);
    const reverse = { ...guestbookAppSet, spec: { ...guestbookAppSet.spec, strategy: { type: 'RollingSync' as const, deletionOrder: 'Reverse' as const } } };
    expect(deletionImpact(reverse).order).toMatch(/Reverse/);
  });
});

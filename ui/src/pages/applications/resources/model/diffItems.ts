import type { ResourceDiff } from '../../../../api/types';
import { manifestYAML } from './manifest';
import { resourceKey } from './resources';

export interface DiffItem {
  key: string;
  /** "group/kind/namespace/name" header (DIFF-042 file path). */
  path: string;
  diff: ResourceDiff;
  liveYAML: string;
  desiredYAML: string;
}

/** The two sides compared everywhere: normalized live vs predicted (else target) state. */
export function diffSides(diff: ResourceDiff): { liveYAML: string; desiredYAML: string } {
  return {
    liveYAML: manifestYAML(diff.normalizedLiveState ?? diff.liveState),
    desiredYAML: manifestYAML(diff.predictedLiveState ?? diff.targetState),
  };
}

/** Out-of-sync resources for the app-level diff: hooks and identical renderings are skipped. */
export function outOfSyncDiffs(items: readonly ResourceDiff[]): DiffItem[] {
  const result: DiffItem[] = [];
  for (const diff of items) {
    if (diff.hook) continue;
    const { liveYAML, desiredYAML } = diffSides(diff);
    if (liveYAML === desiredYAML) continue;
    const key = resourceKey(diff);
    result.push({ key, path: key, diff, liveYAML, desiredYAML });
  }
  return result;
}

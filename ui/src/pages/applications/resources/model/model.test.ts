import { describe, expect, it } from 'vitest';
import { largeTreeFixture, resourcesAppFixture, resourcesTreeFixture } from '../../../../mocks/fixtures/apps-resources';
import { layoutGraph } from '../graph/layout';
import { clampZoom, cullLayout, ensureVisible, fitView, zoomAt } from '../graph/viewport';
import { resolveResourceView, resourceSearch } from '../contract';
import { diffLines, splitLines, splitRows, unifiedRows } from './diff';
import { outOfSyncDiffs } from './diffItems';
import { applyFiltersToParams, compileNameFilter, createMatcher, EMPTY_FILTERS, filtersFromParams, globMatch, hasNarrowingFilters } from './filters';
import { kindCaption, kindForAbbreviation, kindInitials, resolveKindIcon } from './kinds';
import { containerNames, createMergePatch, manifestYAML, metadataMap, parseYAML } from './manifest';
import { buildNetworkGraph, EXTERNAL_TRAFFIC_ID } from './networkGraph';
import { formatQuantity, groupPods, podFacts } from './pods';
import { buildResourceModel, ownerChain, parseResourceKey, resourceKey } from './resources';
import { APP_NODE_ID, buildTreeGraph, groupKey, type TreeGraphOptions } from './treeGraph';

const app = resourcesAppFixture();
const tree = resourcesTreeFixture();
const all = () => true;
const baseOptions: TreeGraphOptions = {
  matcher: all,
  filtering: false,
  collapsed: new Set(),
  expandedParents: new Set(),
  expandedGroups: new Set(),
  groupSimilar: true,
};

describe('kind captions and icons (API-127/129/130)', () => {
  it('abbreviates known kinds and lowercases others', () => {
    expect(kindCaption('Deployment')).toBe('deploy');
    expect(kindCaption('ResourceQuota')).toBe('quota');
    expect(kindCaption('PodDisruptionBudget')).toBe('pdb');
    expect(kindCaption('EndpointSlice')).toBe('endpointslice');
    expect(kindCaption('Certificate')).toBe('certificate');
    expect(kindCaption('')).toBe('');
    expect(kindForAbbreviation('deploy')).toBe('Deployment');
  });

  it('resolves special kinds, the kind map, then initials', () => {
    expect(resolveKindIcon('node')).toBe('node');
    expect(resolveKindIcon('Application')).toBe('application');
    expect(resolveKindIcon('ApplicationSet')).toBe('applicationset');
    expect(resolveKindIcon('ConfigMap')).toBe('configmap');
    expect(resolveKindIcon('ResourceQuota')).toBeUndefined();
    expect(resolveKindIcon('deployment')).toBeUndefined();
    expect(kindInitials('UnknownResource')).toBe('UR');
    expect(kindInitials('MyCustomKind')).toBe('MCK');
    expect(kindInitials('V1Beta')).toBe('V1B');
    expect(kindInitials('lowercase')).toBe('');
  });
});

describe('resource model', () => {
  it('merges live nodes with managed status and adds missing managed resources', () => {
    const model = buildResourceModel(app, tree, false);
    const deployment = model.byKey.get('apps/Deployment/shop/web');
    expect(deployment).toMatchObject({ managed: true, sync: 'OutOfSync', syncWave: 1, id: 'uid-deploy-web' });
    const pvc = model.byKey.get('/PersistentVolumeClaim/shop/data');
    expect(pvc).toMatchObject({ missing: true, managed: true, health: { status: 'Missing' } });
    expect(model.byKey.get('/ConfigMap/shop/legacy')?.requiresPruning).toBe(true);
    expect(model.byKey.get('batch/Job/shop/db-migrate')?.hook).toBe(true);
    expect(model.byKey.has('/ConfigMap/shop/stray')).toBe(false);
    expect(buildResourceModel(app, tree, true).byKey.get('/ConfigMap/shop/stray')?.orphaned).toBe(true);
    expect(model.orphanedCount).toBe(1);
  });

  it('follows owner chains and round-trips keys', () => {
    const model = buildResourceModel(app, tree, false);
    const pod = model.byKey.get('/Pod/shop/web-7d9f-b2');
    expect(pod && ownerChain(pod, model.byId).map((i) => i.name)).toEqual(['web-7d9f', 'web']);
    expect(parseResourceKey('apps/Deployment/shop/web')).toEqual({ group: 'apps', kind: 'Deployment', namespace: 'shop', name: 'web' });
    expect(parseResourceKey('/Namespace//shop')).toEqual({ group: '', kind: 'Namespace', namespace: '', name: 'shop' });
    expect(parseResourceKey('bad')).toBeNull();
    expect(resourceKey({ kind: 'ClusterRole', name: 'admin' })).toBe('/ClusterRole//admin');
  });
});

describe('filters', () => {
  it('round-trips through URL parameters and keeps other parameters', () => {
    const filters = { ...EMPTY_FILTERS, name: 'web*', kinds: ['Pod', 'Service'], sync: ['OutOfSync'], showOrphaned: true, showHooks: false };
    const params = applyFiltersToParams(new URLSearchParams('view=list&node=x'), filters);
    expect(params.get('view')).toBe('list');
    expect(params.get('kind')).toBe('Pod,Service');
    expect(params.get('hooks')).toBe('false');
    expect(filtersFromParams(params)).toEqual(filters);
    expect(hasNarrowingFilters(filtersFromParams(new URLSearchParams('orphaned=true')))).toBe(false);
  });

  it('matches names with wildcards or substrings', () => {
    expect(compileNameFilter('web-*-a1')('web-7d9f-a1')).toBe(true);
    expect(compileNameFilter('web-?d9f*')('web-7d9f-a1')).toBe(true);
    expect(compileNameFilter('WEB')('my-web-app')).toBe(true);
    expect(compileNameFilter('web*')('my-web-app')).toBe(false);
    expect(compileNameFilter('a.b')('axb')).toBe(false);
  });

  it('matches wildcard edge cases like an anchored glob', () => {
    const cases: [pattern: string, name: string, expected: boolean][] = [
      ['*', '', true],
      ['*', 'anything', true],
      ['?', '', false],
      ['?', 'a', true],
      ['?', 'ab', false],
      ['a*', 'a', true],
      ['*a', 'ba', true],
      ['*a', 'ab', false],
      ['a**b', 'ab', true],
      ['a*b*c', 'a-x-b-y-c', true],
      ['a*b*c', 'a-x-c-y-b', false],
      ['*-a1', 'web-7d9f-a1-a1', true],
      ['web-??', 'web-a1', true],
      ['web-??', 'web-a', false],
      ['(web)*', '(web)-1', true],
      ['[a]*', '[a]x', true],
      ['[a]*', 'ax', false],
      ['é?', 'éß', true],
      ['?', '😀', true],
      ['Shop-*', 'shop-frontend', true],
    ];
    for (const [pattern, name, expected] of cases) {
      expect(compileNameFilter(pattern)(name), `${pattern} ~ ${name}`).toBe(expected);
    }
    expect(globMatch(Array.from('a*b'), Array.from('axxb'))).toBe(true);
  });

  it('matches pathological wildcard patterns from a shared link in linear-ish time', () => {
    const realistic = 'shop-frontend-deployment-5c8f7b9d4-x2k9p';
    const names = [realistic, 'a'.repeat(40), 'a'.repeat(253), `${'a'.repeat(5000)}c`];
    const patterns = ['*?*?*?*?*?*?*!', '*a*a*a*a*a*a*a*a*b', `${'*a'.repeat(30)}*b`, `${'*?'.repeat(40)}!`, '**********************b'];
    const started = performance.now();
    for (const pattern of patterns) {
      const match = compileNameFilter(pattern);
      // The views evaluate the matcher for every item, several times per render.
      for (let i = 0; i < 100; i += 1) {
        for (const name of names) expect(match(name)).toBe(false);
      }
    }
    // A backtracking RegExp needs seconds for a single `*a*a*a*a*a*a*a*a*b` test against 40
    // characters; the two-pointer matcher does all 2,000 evaluations in a few milliseconds.
    expect(performance.now() - started).toBeLessThan(1000);
  });

  it('treats OutOfSync as including Unknown, always shows hooks for sync filters, and can hide hooks', () => {
    const model = buildResourceModel(app, tree, false);
    const match = createMatcher({ ...EMPTY_FILTERS, sync: ['OutOfSync'] });
    const names = model.items.filter(match).map((i) => i.name);
    expect(names).toEqual(expect.arrayContaining(['web', 'legacy', 'data', 'db-migrate']));
    expect(names).not.toContain('cache');
    const noHooks = createMatcher({ ...EMPTY_FILTERS, showHooks: false });
    expect(model.items.filter(noHooks).some((i) => i.hook)).toBe(false);
    const unknown = { ...model.items[0], sync: 'Unknown', hook: false } as (typeof model.items)[number];
    expect(match(unknown)).toBe(true);
  });
});

describe('tree graph', () => {
  it('hangs managed resources off the application and children off their parents', () => {
    const graph = buildTreeGraph(app, buildResourceModel(app, tree, false), baseOptions);
    const parentOf = new Map(graph.edges.map((e) => [e.to, e.from]));
    expect(graph.roots).toEqual([APP_NODE_ID]);
    expect(parentOf.get('uid-deploy-web')).toBe(APP_NODE_ID);
    expect(parentOf.get('uid-rs-new')).toBe('uid-deploy-web');
    expect(parentOf.get('uid-pod-web-7d9f-a1')).toBe('uid-rs-new');
    expect(parentOf.get('/PersistentVolumeClaim/shop/data')).toBe(APP_NODE_ID);
  });

  it('keeps ancestors of filter matches visible but dimmed', () => {
    const model = buildResourceModel(app, tree, false);
    const graph = buildTreeGraph(app, model, { ...baseOptions, matcher: (i) => i.name === 'web-7d9f-b2', filtering: true });
    const visible = graph.nodes.map((n) => [n.label, n.dimmed]);
    expect(visible).toEqual([
      ['shop', undefined],
      ['web', true],
      ['web-7d9f', true],
      ['web-7d9f-b2', false],
    ]);
  });

  it('groups many same-kind leaves, pages wide parents and counts collapsed descendants', () => {
    const { app: bigApp, tree: bigTree } = largeTreeFixture(300, 2);
    const model = buildResourceModel(bigApp, bigTree, false);
    const grouped = buildTreeGraph(bigApp, model, baseOptions);
    const groups = grouped.nodes.filter((n) => n.type === 'group');
    expect(groups).toHaveLength(2);
    expect(groups[0]?.members).toHaveLength(150);
    expect(groups[0]?.label).toBe('150 Pod resources');
    expect(groups[0]?.health).toBe('Degraded');

    const expanded = buildTreeGraph(bigApp, model, { ...baseOptions, expandedGroups: new Set([groupKey('uid-big-rs0', 'Pod')]) });
    const more = expanded.nodes.find((n) => n.type === 'more');
    expect(more).toMatchObject({ label: 'Show 125 more', hiddenCount: 125, expandKey: 'uid-big-rs0' });
    const all = buildTreeGraph(bigApp, model, {
      ...baseOptions,
      expandedGroups: new Set([groupKey('uid-big-rs0', 'Pod')]),
      expandedParents: new Set(['uid-big-rs0']),
    });
    expect(all.nodes.filter((n) => n.type === 'resource' && n.kind === 'Pod')).toHaveLength(150);

    const collapsed = buildTreeGraph(bigApp, model, { ...baseOptions, collapsed: new Set(['uid-big']) });
    expect(collapsed.nodes.find((n) => n.id === 'uid-big')).toMatchObject({ expanded: false, collapsedCount: 302 });
    expect(collapsed.nodes).toHaveLength(2);
  });
});

describe('layout', () => {
  it('layers left to right, centers parents on children and never overlaps rows', () => {
    const layout = layoutGraph(
      ['r', 'a', 'b', 'a1', 'a2', 'b1'].map((id) => ({ id, width: 100 })),
      [
        { from: 'r', to: 'a' },
        { from: 'r', to: 'b' },
        { from: 'a', to: 'a1' },
        { from: 'a', to: 'a2' },
        { from: 'b', to: 'b1' },
      ],
      ['r'],
    );
    const n = (id: string) => layout.nodes.get(id) ?? { x: NaN, y: NaN, layer: -1, height: 0 };
    expect([n('r').layer, n('a').layer, n('a1').layer]).toEqual([0, 1, 2]);
    expect(n('a').x).toBeGreaterThan(n('r').x);
    expect(n('a').y).toBe((n('a1').y + n('a2').y) / 2);
    expect(n('b1').y).toBeGreaterThan(n('a2').y);
    expect(layout.order).toEqual(['r', 'a', 'a1', 'a2', 'b', 'b1']);
    expect(layout.edges.every((e) => e.primary)).toBe(true);
  });

  it('places multi-parent nodes after their deepest parent and survives cycles', () => {
    const layout = layoutGraph(
      ['r', 'a', 'b', 'c'].map((id) => ({ id, width: 50 })),
      [
        { from: 'r', to: 'a' },
        { from: 'a', to: 'b' },
        { from: 'r', to: 'b' },
        { from: 'b', to: 'c' },
        { from: 'c', to: 'a' },
      ],
      ['r'],
    );
    expect(layout.nodes.get('b')?.layer).toBe(2);
    expect(layout.edges.find((e) => e.from === 'r' && e.to === 'b')?.primary).toBe(false);
    expect(layout.nodes.size).toBe(4);
    expect(new Set(layout.order).size).toBe(4);
  });

  it('lays out thousands of nodes quickly', () => {
    const { app: bigApp, tree: bigTree } = largeTreeFixture(5000, 50);
    const model = buildResourceModel(bigApp, bigTree, false);
    const graph = buildTreeGraph(bigApp, model, { ...baseOptions, groupSimilar: false, childLimit: 10_000 });
    const started = performance.now();
    const layout = layoutGraph(graph.nodes.map((node) => ({ id: node.id, width: 236 })), graph.edges, graph.roots);
    const elapsed = performance.now() - started;
    expect(layout.nodes.size).toBe(5052);
    expect(elapsed).toBeLessThan(1500);
    const ys = [...layout.nodes.values()].filter((p) => p.layer === 3).map((p) => p.y).sort((a, b) => a - b);
    for (let i = 1; i < ys.length; i += 1) expect((ys[i] ?? 0) - (ys[i - 1] ?? 0)).toBeGreaterThanOrEqual(56);
  });
});

describe('viewport math', () => {
  it('zooms around a point, fits and pans a node into view', () => {
    const zoomed = zoomAt({ x: 0, y: 0, k: 1 }, 2, 100, 100);
    expect(zoomed).toEqual({ x: -100, y: -100, k: 2 });
    expect(clampZoom(10)).toBe(2);
    expect(clampZoom(0.01)).toBe(0.1);
    const fit = fitView({ width: 2000, height: 1000 }, { width: 1000, height: 600 });
    expect(fit.k).toBeCloseTo(0.484, 2);
    const moved = ensureVisible({ x: 0, y: 0, k: 1 }, { x: 900, y: 10, width: 200, height: 50 }, { width: 1000, height: 600 });
    expect(moved.x).toBe(-124);
    expect(ensureVisible(moved, { x: 900, y: 10, width: 200, height: 50 }, { width: 1000, height: 600 })).toBe(moved);
  });

  it('culls nodes outside the viewport and disables culling before measurement', () => {
    const layout = layoutGraph(
      Array.from({ length: 200 }, (_, i) => ({ id: `n${i}`, width: 100 })),
      Array.from({ length: 199 }, (_, i) => ({ from: 'n0', to: `n${i + 1}` })),
      ['n0'],
    );
    const culled = cullLayout(layout, { x: 0, y: 0, k: 1 }, { width: 800, height: 400 }, 0);
    expect(culled.nodes.size).toBeLessThan(20);
    expect(cullLayout(layout, { x: 0, y: 0, k: 1 }, { width: 0, height: 0 }).nodes.size).toBe(200);
  });
});

describe('network graph', () => {
  it('links external traffic → Ingress → Service → Pods and collects endpoints', () => {
    const model = buildResourceModel(app, tree, false);
    const graph = buildNetworkGraph(model, all);
    const edges = graph.edges.map((e) => `${e.from}>${e.to}`);
    expect(edges).toContain(`${EXTERNAL_TRAFFIC_ID}>uid-ing-web`);
    expect(edges).toContain('uid-ing-web>uid-svc-web');
    expect(edges).toContain('uid-svc-web>uid-pod-web-7d9f-a1');
    expect(edges).not.toContain('uid-svc-web>uid-pod-cache-0');
    expect(graph.endpoints[0]).toMatchObject({ addresses: ['203.0.113.10'], urls: ['https://shop.example.test/'] });
    expect(graph.nodes.find((n) => n.id === 'uid-ing-web')?.subtitle).toBe('203.0.113.10');
  });
});

describe('pods', () => {
  it('groups by node with hosts, by parent and by top-level resource', () => {
    const model = buildResourceModel(app, tree, false);
    const byNode = groupPods(model, tree.hosts ?? [], 'node', all);
    expect(byNode.map((g) => [g.title, g.pods.length])).toEqual([
      ['node-a', 3],
      ['node-b', 2],
    ]);
    expect(byNode[0]?.host?.name).toBe('node-a');
    const byParent = groupPods(model, [], 'parentResource', all);
    expect(byParent.map((g) => g.title)).toEqual(['Job/db-migrate', 'ReplicaSet/web-7d9f', 'StatefulSet/cache']);
    const byTop = groupPods(model, [], 'topLevelResource', all);
    expect(byTop.map((g) => g.title)).toEqual(['Deployment/web', 'Job/db-migrate', 'StatefulSet/cache']);
  });

  it('formats host quantities and reads pod facts', () => {
    expect(formatQuantity('cpu', 750)).toBe('750m');
    expect(formatQuantity('cpu', 4000)).toBe('4 cores');
    expect(formatQuantity('memory', 1610612736)).toBe('1.5 GiB');
    expect(formatQuantity('memory', undefined)).toBe('—');
    const model = buildResourceModel(app, tree, false);
    const pod = model.byKey.get('/Pod/shop/web-7d9f-b2');
    expect(pod && podFacts(pod)).toEqual({ phase: 'CrashLoopBackOff', ready: '1/2', restarts: 12, node: 'node-b' });
  });
});

describe('line diff', () => {
  it('produces a minimal diff with line numbers', () => {
    const ops = diffLines(['a', 'b', 'c', 'd'], ['a', 'x', 'c', 'd', 'e']);
    expect(ops.map((o) => `${o.type[0] ?? ''}${o.text}`)).toEqual(['ea', 'db', 'ix', 'ec', 'ed', 'ie']);
    expect(ops.find((o) => o.text === 'x')).toEqual({ type: 'insert', newLine: 2, text: 'x' });
    expect(diffLines([], ['a'])).toEqual([{ type: 'insert', newLine: 1, text: 'a' }]);
    expect(diffLines(['a'], [])).toEqual([{ type: 'delete', oldLine: 1, text: 'a' }]);
  });

  it('stays correct (if not minimal) beyond the edit budget', () => {
    const a = Array.from({ length: 300 }, (_, i) => `a${i}`);
    const b = Array.from({ length: 300 }, (_, i) => `b${i}`);
    const ops = diffLines(a, b, 10);
    expect(ops.filter((o) => o.type !== 'insert').map((o) => o.text)).toEqual(a);
    expect(ops.filter((o) => o.type !== 'delete').map((o) => o.text)).toEqual(b);
  });

  it('builds compact unified rows with expandable skips and side-by-side pairs', () => {
    const lines = Array.from({ length: 20 }, (_, i) => `l${i}`);
    const changed = [...lines];
    changed[10] = 'changed';
    const ops = diffLines(lines, changed);
    const rows = unifiedRows(ops, 2);
    expect(rows.filter((r) => r.type === 'skip').map((r) => (r.type === 'skip' ? r.count : 0))).toEqual([8, 7]);
    const skip = rows.find((r) => r.type === 'skip');
    const expanded = unifiedRows(ops, 2, new Set(skip?.type === 'skip' ? [skip.id] : []));
    expect(expanded.filter((r) => r.type === 'skip')).toHaveLength(1);
    expect(unifiedRows(ops, null)).toHaveLength(ops.length);
    const pairs = splitRows(rows).filter((r) => r.type === 'pair' && (r.left?.type === 'delete' || r.right?.type === 'insert'));
    expect(pairs).toHaveLength(1);
    expect(splitLines('a\nb\n')).toEqual(['a', 'b']);
  });
});

describe('manifests', () => {
  it('renders JSON as YAML without managed fields', () => {
    const yaml = manifestYAML(JSON.stringify({ kind: 'ConfigMap', metadata: { name: 'x', managedFields: [{ manager: 'm' }] } }));
    expect(yaml).toBe('kind: ConfigMap\nmetadata:\n  name: x\n');
    expect(manifestYAML('')).toBe('');
    expect(manifestYAML('not json')).toBe('');
  });

  it('computes RFC 7386 merge patches', () => {
    expect(createMergePatch({ a: 1, b: { c: 1, d: 2 }, e: [1] }, { a: 1, b: { c: 2 }, e: [1, 2], f: 'x' })).toEqual({ b: { c: 2, d: null }, e: [1, 2], f: 'x' });
    expect(createMergePatch({ a: 1 }, { a: 1 })).toBeUndefined();
  });

  it('parses YAML with line-numbered errors', () => {
    expect(parseYAML('a: 1')).toEqual({ ok: true, value: { a: 1 } });
    const bad = parseYAML('a: 1\na: 2');
    expect(bad).toMatchObject({ ok: false, line: 2, message: 'Map keys must be unique' });
  });

  it('reads containers from pods, workload templates and cron jobs, and metadata maps', () => {
    expect(containerNames({ spec: { containers: [{ name: 'a' }], initContainers: [{ name: 'i' }] } })).toEqual({ containers: ['a'], initContainers: ['i'] });
    expect(containerNames({ spec: { template: { spec: { containers: [{ name: 'w' }] } } } }).containers).toEqual(['w']);
    expect(containerNames({ spec: { jobTemplate: { spec: { template: { spec: { containers: [{ name: 'c' }] } } } } } }).containers).toEqual(['c']);
    expect(containerNames(null).containers).toEqual([]);
    expect(metadataMap({ metadata: { labels: { a: 'b', n: 1 } } }, 'labels')).toEqual({ a: 'b', n: '1' });
  });

  it('selects out-of-sync diffs for the app-level panel (DIFF-042)', () => {
    const items = outOfSyncDiffs([
      { kind: 'A', name: 'same', normalizedLiveState: '{"a":1}', predictedLiveState: '{"a":1}' },
      { kind: 'B', name: 'hook', hook: true, normalizedLiveState: '{"a":1}', predictedLiveState: '{"a":2}' },
      { kind: 'C', name: 'changed', normalizedLiveState: '{"a":1}', predictedLiveState: '{"a":2}' },
      { kind: 'D', name: 'fallback', liveState: '{"a":1}', targetState: '{"a":3}' },
    ]);
    expect(items.map((i) => i.path)).toEqual(['/C//changed', '/D//fallback']);
  });
});

describe('contract helpers', () => {
  it('resolves the view from the URL, then the annotation, then the tree', () => {
    expect(resolveResourceView(app, 'pods')).toBe('pods');
    expect(resolveResourceView(app, 'bogus')).toBe('tree');
    expect(resolveResourceView(resourcesAppFixture({ annotations: { 'pref.cd.patrol.example/default-view': 'list' } }), null)).toBe('list');
    expect(resourceSearch('apps/Deployment/shop/web', 'logs')).toBe('?node=apps%2FDeployment%2Fshop%2Fweb&tab=logs');
  });
});

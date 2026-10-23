# Resource graph: cheap first render (2026-10-23)

Issue [#222](https://github.com/juliosaraiva/patrol-cd/issues/222). Follow-up to finding 4 of the issue [#86](https://github.com/juliosaraiva/patrol-cd/issues/86) record
(`2026-10-23-ui-shuffle-order.md`): the two 2000-pod tests in
`ui/src/pages/applications/resources/graph/GraphCanvas.test.tsx` spent 4–6 s in
`render(<GraphCanvas>)`, because the first render mounted all 2022 nodes and 2021 edges.
`cullLayout` culls only once the viewport has a size, and `useElementSize` measured it in a layout
effect after that render. In a browser the same render built every SVG node of the graph for one
frame on each mount.

## Change

- `useElementSize` returns `undefined` until the element is measured.
- Until then `GraphCanvas` culls everything: only the tab stop and the selected node are drawn. The
  measurement runs in a layout effect, so a browser never paints this render.
- A viewport that measures 0×0 (never laid out, e.g. jsdom without size spies) still disables
  culling: every node is drawn, as before. `ResourceViews.test.tsx` and
  `details/ResourceViewsSlot.test.tsx` rely on that. They now get one extra, nearly empty render
  first.
- New tests in `GraphCanvas.test.tsx`:
  - What the first commit drew, read from the `clientWidth` spy when the canvas measures. For the
    2000-pod graph: 1 node and 0 edges, then between 1 and 100 of each after measuring. For a 0×0
    viewport: 1 node first, then every node and every edge.
  - What is in the DOM when a `flushSync` mount outside `act()` returns, i.e. when a browser would
    paint: already culled to the measured viewport. This fails if the measurement moves from a
    layout effect to a passive effect (`act()` runs both, so the first test cannot tell).

## Environment

| Item | Value |
|------|-------|
| Host | macOS 27.0.1 (arm64), 10 CPU |
| Node / pnpm | v22.22.3 / 10.33.0 |
| vitest / jsdom / React / MSW | 3.2.7 / 26.1.0 / 19.2.8 / 2.15.0 (`ui/package.json`) |
| Tree | worktree of `main` at `774c10a`, `pnpm -C ui install --frozen-lockfile` |
| CPU load | no synthetic load; other agents' processes on the host: load average 110–180 during every run below |

All UI tests run against MSW mocks (simulated API).

## Results

| Command (in `ui/` unless noted) | Tree | Exit | Result |
|---------------------------------|------|------|--------|
| `pnpm exec vitest run --no-cache src/pages/applications/resources/graph/GraphCanvas.test.tsx --reporter=verbose` (load 112) | `774c10a` (before) | 1 | `Tests 2 failed \| 2 passed (4)`: "renders only the nodes near the viewport…" 14647 ms and "keeps the focused node mounted…" 10435 ms, both `Test timed out in 10000ms` |
| same (load 142) | change, first version of the new tests | 0 | `Tests 6 passed (6)`: the two 2000-pod tests 2882 ms and 2924 ms; new tests 906 ms and 5469 ms (the 0×0 test then used 200 pods; cut to 40) |
| same with `-t "draws"`, `GraphCanvas.tsx` reverted to `774c10a` (load 182) | new tests, old component | 1 | `Tests 2 failed \| 4 skipped (6)`: `expected { nodes: 2022, edges: 2021 } to deeply equal { nodes: 1, edges: +0 }` and `expected { nodes: 62, edges: 61 } to deeply equal { nodes: 1, edges: +0 }`: the new tests fail without the change |
| `pnpm exec vitest run --no-cache src/pages/applications/resources/graph/GraphCanvas.test.tsx src/pages/applications/resources/ResourceViews.test.tsx src/pages/applications/details/ResourceViewsSlot.test.tsx src/pages/applications/resources/views --reporter=verbose` (load 176) | change | 0 | `Test Files 4 passed (4)`, `Tests 37 passed (37)`; the two 2000-pod tests 396 ms and 288 ms, new tests 59 ms and 186 ms |
| `pnpm -C ui typecheck` (repository root) | change | 0 | `tsc -p tsconfig.json && tsc -p tsconfig.node.json`, no output |
| `pnpm -C ui lint` (repository root) | change | 0 | `eslint . --max-warnings=0`, no output |
| `pnpm -C ui test` (repository root, load 158–169) | change | 0 | `Test Files 76 passed (76)`, `Tests 914 passed (914)`, no `Errors` line, duration 120 s; the two 2000-pod tests 1619 ms and 1256 ms, new tests 356 ms and 625 ms; `ResourceViews.test.tsx` 15 passed, `ResourceViewsSlot.test.tsx` 8 passed |

## Agent review and its fixes

The review-change workflow (from branch `feat/claude-code-sdlc`, not yet on `main`) ran
`ui-reviewer` and `test-reviewer` on commit `e92a8e7` (tree `abfda1c`): no blocker or major
finding, two minor findings in the new tests, both fixed:

1. The first-commit test passed whether the viewport was measured in a layout effect or a passive
   effect, so nothing checked that the unmeasured render is never painted. Added "measures the
   viewport before the first paint".
2. The edge counts had only upper bounds, so a selector that stopped matching passed. Added lower
   bounds: more than 0 edges after measuring, every edge for a 0×0 viewport.

| Command (in `ui/`) | Tree | Exit | Result |
|--------------------|------|------|--------|
| `pnpm exec vitest run --no-cache src/pages/applications/resources/graph/GraphCanvas.test.tsx --reporter=verbose` (load 163) | change with the fixes | 0 | `Tests 7 passed (7)`: the two 2000-pod tests 2008 ms and 1111 ms; new tests 417 ms, 288 ms, 582 ms |
| same with `-t "draws\|measures"`, `useElementSize` on `useEffect` instead of `useLayoutEffect` | mutation | 1 | `Tests 1 failed \| 2 passed \| 4 skipped (7)`: "measures the viewport before the first paint": `expected 1 to be greater than 1` |
| same with `-t "draws"`, the edge layer wrapped in one more `<g>` | mutation | 1 | `Tests 2 failed \| 5 skipped (7)`: `expected 0 to be greater than 0`; `expected +0 to be 61` |

## Timeout

`main` has no per-test timeout on the two 2000-pod tests (the 10 s default applies). The issue #86
change, not merged when this was recorded, gives them `{ timeout: 60_000 }` (`BIG_GRAPH_TIMEOUT`).
With this change they took 0.3–2.9 s at load average 110–180 (1.6 s and 1.3 s in the full run), so
that timeout can be removed once both changes are on the same branch.

import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { largeTreeFixture } from '../../../../mocks/fixtures/apps-resources';
import { buildResourceModel } from '../model/resources';
import { buildTreeGraph } from '../model/treeGraph';
import { GraphCanvas } from './GraphCanvas';
import type { Graph } from './types';

function bigGraph(pods = 2000): Graph {
  const { app, tree } = largeTreeFixture(pods, 20);
  const model = buildResourceModel(app, tree, false);
  return buildTreeGraph(app, model, {
    matcher: () => true,
    filtering: false,
    collapsed: new Set(),
    expandedParents: new Set(),
    expandedGroups: new Set(),
    groupSimilar: false,
    childLimit: 10_000,
  });
}

describe('GraphCanvas', () => {
  beforeEach(() => {
    // jsdom does not lay out; give the viewport a size so culling applies.
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(1000);
    vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(600);
  });
  afterEach(() => vi.restoreAllMocks());

  it('renders only the nodes near the viewport of a graph with thousands of nodes', () => {
    const graph = bigGraph();
    render(<GraphCanvas graph={graph} label="Big tree" onActivate={() => undefined} />);
    const svg = document.querySelector('svg[data-node-count]');
    expect(Number(svg?.getAttribute('data-node-count'))).toBe(graph.nodes.length);
    const rendered = Number(svg?.getAttribute('data-rendered-count'));
    expect(rendered).toBeGreaterThan(0);
    expect(rendered).toBeLessThan(100);
    const items = within(screen.getByRole('tree', { name: 'Big tree' })).getAllByRole('treeitem');
    expect(items.length).toBe(rendered);
    // The root starts in view; pods around it carry their position among 100 siblings.
    expect(within(screen.getByRole('tree')).getByRole('treeitem', { name: /^Application big/ })).toHaveAttribute('aria-level', '1');
    expect(items.some((i) => i.getAttribute('aria-level') === '4' && i.getAttribute('aria-setsize') === '100')).toBe(true);
  });

  it('keeps the focused node mounted while moving far down the tree with the keyboard', async () => {
    const user = userEvent.setup();
    render(<GraphCanvas graph={bigGraph()} label="Big tree" onActivate={() => undefined} />);
    const tree = screen.getByRole('tree', { name: 'Big tree' });
    const root = within(tree).getByRole('treeitem', { name: /^Application big/ });
    expect(root).toHaveAttribute('tabindex', '0');
    root.focus();
    await user.keyboard('{End}');
    const focused = document.activeElement as HTMLElement;
    expect(focused.getAttribute('role')).toBe('treeitem');
    expect(focused.getAttribute('aria-label')).toMatch(/^Pod big-rs19-p1999/);
    // The view panned so the focused node is on screen: its transform is inside the viewport.
    const svg = document.querySelector('svg[data-node-count]');
    expect(Number(svg?.getAttribute('data-rendered-count'))).toBeLessThan(100);
  });

  it('zooms with buttons and keys, resets and fits', async () => {
    const user = userEvent.setup();
    const onZoom = vi.fn();
    render(<GraphCanvas graph={bigGraph(200)} label="Big tree" onActivate={() => undefined} zoom={1} onZoomChange={onZoom} />);
    const readout = () => screen.getByRole('button', { name: /^Zoom \d+%, reset to 100%$/ });
    expect(readout()).toHaveTextContent('100%');
    await user.click(screen.getByRole('button', { name: 'Zoom in' }));
    expect(readout()).toHaveTextContent('125%');
    await user.click(screen.getByRole('button', { name: 'Zoom out' }));
    await user.click(screen.getByRole('button', { name: 'Zoom out' }));
    expect(readout()).toHaveTextContent('80%');
    within(screen.getByRole('tree')).getByRole('treeitem', { name: /^Application big/ }).focus();
    await user.keyboard('+');
    expect(readout()).toHaveTextContent('100%');
    await user.keyboard('-');
    expect(readout()).toHaveTextContent('80%');
    await user.click(readout());
    expect(readout()).toHaveTextContent('100%');
    await user.click(screen.getByRole('button', { name: 'Fit to screen' }));
    expect(Number.parseInt(readout().textContent ?? '0', 10)).toBeLessThan(100);
    await vi.waitFor(() => expect(onZoom).toHaveBeenCalled());
  });

  it('activates nodes with Enter, Space and click', async () => {
    const user = userEvent.setup();
    const onActivate = vi.fn();
    render(<GraphCanvas graph={bigGraph(200)} label="Big tree" onActivate={onActivate} />);
    const first = within(screen.getByRole('tree')).getByRole('treeitem', { name: /^Application big/ });
    first.focus();
    await user.keyboard('{Enter}');
    await user.keyboard(' ');
    await user.click(first);
    expect(onActivate).toHaveBeenCalledTimes(3);
    expect(onActivate.mock.calls[0]?.[0]).toMatchObject({ type: 'app', label: 'big' });
  });

  it('draws only the tab stop before the viewport is measured, then only the nodes near it', () => {
    const graph = bigGraph();
    // The viewport is measured in a layout effect after the first commit: record what that commit drew.
    const atMeasurement: Drawn[] = [];
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockImplementation(() => {
      atMeasurement.push(drawn());
      return 1000;
    });
    render(<GraphCanvas graph={graph} label="Big tree" onActivate={() => undefined} />);
    expect(atMeasurement[0]).toEqual({ nodes: 1, edges: 0 });
    const after = drawn();
    expect(after.nodes).toBeGreaterThan(1);
    expect(after.nodes).toBeLessThan(100);
    expect(after.edges).toBeLessThan(100);
  });

  it('draws every node when the viewport measures 0×0', () => {
    const graph = bigGraph(40);
    const atMeasurement: Drawn[] = [];
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockImplementation(() => {
      atMeasurement.push(drawn());
      return 0;
    });
    vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(0);
    render(<GraphCanvas graph={graph} label="Big tree" onActivate={() => undefined} />);
    expect(atMeasurement[0]).toEqual({ nodes: 1, edges: 0 });
    expect(within(screen.getByRole('tree', { name: 'Big tree' })).getAllByRole('treeitem')).toHaveLength(graph.nodes.length);
  });
});

interface Drawn {
  nodes: number;
  edges: number;
}

/** Nodes and edges currently in the canvas' SVG. */
function drawn(): Drawn {
  const svg = document.querySelector('svg[data-node-count]');
  return {
    nodes: svg?.querySelectorAll('[role="treeitem"]').length ?? 0,
    edges: svg?.querySelectorAll(':scope > g > g[aria-hidden="true"] > path').length ?? 0,
  };
}

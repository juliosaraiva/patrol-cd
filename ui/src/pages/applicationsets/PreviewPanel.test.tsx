import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { generatedApplication, guestbookAppSet } from '../../mocks/fixtures/applicationsets';
import { server } from '../../mocks/server';
import { renderWithProviders } from '../../test/render';
import { AUTO_EXPAND, CHANGES_PAGE, DIFF_ROWS, MANIFEST_PAGE, PreviewPanel } from './PreviewPanel';

const TOTAL = CHANGES_PAGE + 70;

/** `count` existing Applications; the generated ones change the revision of the first `changed`. */
function largeSet(count: number, changed: number) {
  const existing = Array.from({ length: count }, (_, i) => generatedApplication(`app-${String(i).padStart(4, '0')}`, guestbookAppSet));
  const desired = existing.map((a, i) => (i < changed ? { ...a, spec: { ...a.spec, source: { ...a.spec.source!, targetRevision: 'release' } } } : a));
  server.use(http.post('*/api/v1/applicationsets/generate', () => HttpResponse.json({ applications: desired })));
  return existing;
}

describe('ApplicationSet preview with many Applications (ASET-126)', () => {
  it('pages the changed Applications as collapsed rows and computes a diff only when expanded', async () => {
    const user = userEvent.setup();
    const existing = largeSet(TOTAL + 30, TOTAL);
    renderWithProviders(<PreviewPanel set={guestbookAppSet} existing={existing} />);
    await user.click(screen.getByRole('button', { name: 'Preview changes' }));

    const list = await screen.findByRole('list', { name: 'Changed applications' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(CHANGES_PAGE);
    expect(screen.getByText(`${TOTAL} Applications change. Expand one to see its diff.`)).toBeInTheDocument();
    // Collapsed rows: name, change and line counts, but no diff table rendered.
    expect(screen.queryAllByRole('region', { name: /^Changes to / })).toHaveLength(0);
    const toggle = within(list).getByRole('button', { name: 'app-0000' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(within(list).getAllByRole('listitem')[0]).toHaveTextContent('1 line added, 1 line removed');
    expect(screen.getByText(/^Unchanged \(30\): app-0120, .*, and 10 more$/)).toBeInTheDocument();

    // Keyboard: expand with Enter, the diff appears and the toggle reports it.
    toggle.focus();
    await user.keyboard('{Enter}');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('region', { name: 'Changes to app-0000' })).toHaveTextContent('added: targetRevision: release');
    await user.keyboard('{Enter}');
    expect(screen.queryByRole('region', { name: 'Changes to app-0000' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: `Show ${CHANGES_PAGE} more of 70 remaining` }));
    expect(within(list).getAllByRole('listitem')).toHaveLength(CHANGES_PAGE * 2);
    await user.click(screen.getByRole('button', { name: 'Show 20 more of 20 remaining' }));
    expect(within(list).getAllByRole('listitem')).toHaveLength(TOTAL);
    expect(screen.queryByRole('button', { name: /more of .* remaining/ })).not.toBeInTheDocument();

    // Manifests are paged too.
    await user.click(screen.getByRole('tab', { name: `Desired (${TOTAL + 30})` }));
    expect(screen.getByText(`Showing ${MANIFEST_PAGE} of ${TOTAL + 30} Applications.`)).toBeInTheDocument();
    const manifests = screen.getByLabelText('Desired applications');
    expect(manifests).toHaveTextContent('app-0024');
    expect(manifests).not.toHaveTextContent('app-0025');
    await user.click(screen.getByRole('button', { name: `Show ${MANIFEST_PAGE} more` }));
    expect(screen.getByLabelText('Desired applications')).toHaveTextContent('app-0049');
  });

  it(`expands every diff when at most ${AUTO_EXPAND} Applications change`, async () => {
    const user = userEvent.setup();
    renderWithProviders(<PreviewPanel set={guestbookAppSet} existing={largeSet(10, 2)} />);
    await user.click(screen.getByRole('button', { name: 'Preview changes' }));
    await screen.findByRole('list', { name: 'Changed applications' });
    expect(screen.getAllByRole('region', { name: /^Changes to / })).toHaveLength(2);
    expect(screen.getByText(/^Unchanged \(8\): /)).toBeInTheDocument();
  });

  it('caps the rows of one very long diff until asked for all of them', async () => {
    const user = userEvent.setup();
    const [base] = largeSet(1, 0);
    const before = { ...base!, metadata: { ...base!.metadata, annotations: Object.fromEntries(Array.from({ length: DIFF_ROWS }, (_, i) => [`k${i}`, 'old'])) } };
    const after = { ...base!, metadata: { ...base!.metadata, annotations: Object.fromEntries(Array.from({ length: DIFF_ROWS }, (_, i) => [`k${i}`, 'new'])) } };
    server.use(http.post('*/api/v1/applicationsets/generate', () => HttpResponse.json({ applications: [after] })));
    renderWithProviders(<PreviewPanel set={guestbookAppSet} existing={[before]} />);
    await user.click(screen.getByRole('button', { name: 'Preview changes' }));
    const region = await screen.findByRole('region', { name: `Changes to ${base!.metadata.name ?? ''}` });
    expect(within(region).getAllByRole('row')).toHaveLength(DIFF_ROWS);
    await user.click(screen.getByRole('button', { name: /^Show all \d+ lines of / }));
    expect(within(region).getAllByRole('row').length).toBeGreaterThan(DIFF_ROWS * 2);
  });
});

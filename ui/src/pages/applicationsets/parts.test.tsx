import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { guestbookAppSet } from '../../mocks/fixtures/applicationsets';
import { renderWithProviders } from '../../test/render';
import type { GeneratedAppRow } from './model';
import { AppSetTree, TREE_PAGE } from './parts';

describe('AppSetTree', () => {
  it('renders large sets incrementally', async () => {
    const user = userEvent.setup();
    const rows: GeneratedAppRow[] = Array.from({ length: TREE_PAGE + 5 }, (_, i) => ({
      key: `patrol/app-${String(i).padStart(4, '0')}`,
      name: `app-${String(i).padStart(4, '0')}`,
      namespace: 'patrol',
      health: 'Healthy',
      sync: 'Synced',
    }));
    renderWithProviders(<AppSetTree set={guestbookAppSet} tree={{ nodes: [] }} rows={rows} controlPlaneNamespace="patrol" />);
    const list = screen.getByRole('list', { name: 'Applications of guestbook' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(TREE_PAGE);
    expect(screen.getByText(`${TREE_PAGE + 5} applications`)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Show 5 more of 5 remaining' }));
    expect(within(list).getAllByRole('listitem')).toHaveLength(TREE_PAGE + 5);
    expect(screen.queryByRole('button', { name: /more of/ })).not.toBeInTheDocument();
  });
});

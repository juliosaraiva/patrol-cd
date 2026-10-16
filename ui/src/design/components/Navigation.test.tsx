import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState, type ReactElement } from 'react';
import { createMemoryRouter, RouterProvider } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import { Breadcrumbs, FilterChips, Pagination, SearchInput } from './Navigation';

function withRouter(ui: ReactElement) {
  return render(<RouterProvider router={createMemoryRouter([{ path: '*', element: ui }])} />);
}

describe('Breadcrumbs', () => {
  it('is a labelled navigation list with the current page marked', () => {
    withRouter(<Breadcrumbs items={[{ label: 'Applications', to: '/applications' }, { label: 'guestbook' }]} />);
    const nav = screen.getByRole('navigation', { name: 'Breadcrumb' });
    expect(nav).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Applications' })).toHaveAttribute('href', '/applications');
    expect(screen.getByText('guestbook')).toHaveAttribute('aria-current', 'page');
  });
});

describe('Pagination', () => {
  it('moves between pages and disables the ends', async () => {
    const onPageChange = vi.fn();
    const onPageSizeChange = vi.fn();
    render(
      <Pagination page={1} pageCount={3} onPageChange={onPageChange} pageSize={10} onPageSizeChange={onPageSizeChange} totalItems={25} />,
    );
    expect(screen.getByRole('navigation', { name: 'Pagination' })).toHaveTextContent('Page 1 of 3 (25 items)');
    expect(screen.getByRole('button', { name: 'Previous page' })).toBeDisabled();
    await userEvent.click(screen.getByRole('button', { name: 'Next page' }));
    expect(onPageChange).toHaveBeenCalledWith(2);
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Rows per page' }), '50');
    expect(onPageSizeChange).toHaveBeenCalledWith(50);
  });
});

describe('SearchInput', () => {
  function Harness({ onSubmit }: { onSubmit?: (v: string) => void }) {
    const [value, setValue] = useState('');
    return <SearchInput aria-label="Search applications" value={value} onValueChange={setValue} shortcut="/" onSubmit={onSubmit} />;
  }

  it('clears with Escape and the clear button, submits with Enter', async () => {
    const onSubmit = vi.fn();
    render(<Harness onSubmit={onSubmit} />);
    const box = screen.getByRole('searchbox', { name: 'Search applications' });
    await userEvent.type(box, 'guest');
    expect(box).toHaveValue('guest');
    await userEvent.keyboard('{Enter}');
    expect(onSubmit).toHaveBeenCalledWith('guest');
    await userEvent.keyboard('{Escape}');
    expect(box).toHaveValue('');
    expect(box).toHaveFocus();
    await userEvent.keyboard('{Escape}');
    expect(box).not.toHaveFocus();
    await userEvent.type(box, 'x');
    await userEvent.click(screen.getByRole('button', { name: 'Clear search' }));
    expect(box).toHaveValue('');
  });
});

describe('FilterChips', () => {
  it('removes individual filters and clears all', async () => {
    const onRemove = vi.fn();
    const onClearAll = vi.fn();
    render(
      <FilterChips
        chips={[
          { id: 'health', label: 'Health: Degraded' },
          { id: 'sync', label: 'Sync: OutOfSync' },
        ]}
        onRemove={onRemove}
        onClearAll={onClearAll}
      />,
    );
    expect(screen.getByRole('list', { name: 'Active filters' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Remove filter Health: Degraded' }));
    expect(onRemove).toHaveBeenCalledWith('health');
    await userEvent.click(screen.getByRole('button', { name: 'Clear all filters' }));
    expect(onClearAll).toHaveBeenCalled();
  });
});

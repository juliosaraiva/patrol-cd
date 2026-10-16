import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { RowListEditor, StringListEditor, type Row, type RowField } from './editors';

const FIELDS: readonly RowField[] = [
  { key: 'key', label: 'Key', mono: true },
  { key: 'value', label: 'Value' },
];

function Labels({ initial }: { initial: Row[] }) {
  const [rows, setRows] = useState<Row[]>(initial);
  return <RowListEditor legend="Labels" noun="label" idPrefix="labels" rows={rows} onChange={setRows} fields={FIELDS} />;
}

describe('RowListEditor', () => {
  it('shows visible field labels for multi-field rows, keeping each input’s accessible name', () => {
    render(
      <Labels
        initial={[
          { key: 'team', value: 'payments' },
          { key: 'env', value: 'prod' },
        ]}
      />,
    );
    const group = screen.getByRole('group', { name: 'Labels' });
    // One column header row (wide screens) plus a label per input (stacked, narrow screens);
    // both are visual: assistive technology reads the inputs' own names.
    const head = group.querySelector('[aria-hidden="true"] > div');
    expect(head).not.toBeNull();
    expect(Array.from(head?.children ?? []).map((c) => c.textContent)).toEqual(['Key', 'Value']);
    expect(within(group).getAllByText('Key')).toHaveLength(3);
    expect(within(group).getAllByText('Value')).toHaveLength(3);
    const key2 = within(group).getByRole('textbox', { name: 'Key, label 2' });
    expect(key2).toHaveValue('env');
    // Visible label text appears at the start of the accessible name (WCAG 2.5.3).
    expect(key2.parentElement).toHaveTextContent(/^Key$/);
    // The visual header is not exposed as a list item.
    expect(within(group).getAllByRole('listitem')).toHaveLength(2);
  });

  it('labels a row added from the keyboard and drops the header when the list is empty', async () => {
    const user = userEvent.setup();
    render(<Labels initial={[]} />);
    const group = screen.getByRole('group', { name: 'Labels' });
    expect(within(group).queryByText('Key')).not.toBeInTheDocument();
    await user.click(within(group).getByRole('button', { name: 'Add label' }));
    const key = within(group).getByRole('textbox', { name: 'Key, label 1' });
    expect(key).toHaveFocus();
    expect(within(group).getAllByText('Key')).toHaveLength(2);
    await user.keyboard('team');
    await user.tab();
    expect(within(group).getByRole('textbox', { name: 'Value, label 1' })).toHaveFocus();
  });

  it('uses the legend as the only visible label of single-field lists', () => {
    render(<StringListEditor legend="Namespaces" label="Namespace" noun="namespace" idPrefix="ns" values={['payments']} onChange={() => undefined} />);
    const group = screen.getByRole('group', { name: 'Namespaces' });
    expect(within(group).getByRole('textbox', { name: 'Namespace 1' })).toHaveValue('payments');
    expect(within(group).queryByText('Namespace')).not.toBeInTheDocument();
  });
});

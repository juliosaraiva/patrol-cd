import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { Checkbox, FormField, Input, RadioGroup, Select, Switch, TextArea } from './Form';

describe('FormField', () => {
  it('labels the control and links help and error text', () => {
    render(
      <FormField label="Repository URL" help="HTTPS or SSH" error="URL is required" required>
        <Input />
      </FormField>,
    );
    const input = screen.getByLabelText(/Repository URL/);
    expect(input).toBeRequired();
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(input).toHaveAccessibleDescription('URL is required HTTPS or SSH');
  });

  it('wires TextArea and Select the same way', () => {
    render(
      <>
        <FormField label="Values" help="YAML">
          <TextArea />
        </FormField>
        <FormField label="Type">
          <Select options={[{ value: 'git', label: 'Git' }, { value: 'helm', label: 'Helm' }]} defaultValue="helm" />
        </FormField>
      </>,
    );
    expect(screen.getByRole('textbox', { name: 'Values' })).toHaveAccessibleDescription('YAML');
    expect(screen.getByRole('combobox', { name: 'Type' })).toHaveValue('helm');
    expect(screen.getByRole('textbox', { name: 'Values' })).not.toHaveAttribute('aria-invalid');
  });
});

describe('Checkbox', () => {
  it('toggles with Space and reports the mixed state', async () => {
    const onChange = vi.fn();
    const { rerender } = render(<Checkbox label="Prune" onChange={onChange} />);
    const box = screen.getByRole('checkbox', { name: 'Prune' });
    box.focus();
    await userEvent.keyboard(' ');
    expect(onChange).toHaveBeenCalledTimes(1);
    rerender(<Checkbox label="Prune" onChange={onChange} indeterminate />);
    expect(box).toHaveAttribute('aria-checked', 'mixed');
    expect((box as HTMLInputElement).indeterminate).toBe(true);
  });
});

describe('RadioGroup', () => {
  function Harness() {
    const [value, setValue] = useState<'a' | 'b' | 'c'>('a');
    return (
      <RadioGroup
        legend="Propagation"
        value={value}
        onChange={setValue}
        options={[
          { value: 'a', label: 'Foreground' },
          { value: 'b', label: 'Background' },
          { value: 'c', label: 'Orphan', disabled: true },
        ]}
      />
    );
  }

  it('groups radios under a legend and selects on click', async () => {
    render(<Harness />);
    expect(screen.getByRole('group', { name: 'Propagation' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('radio', { name: 'Background' }));
    expect(screen.getByRole('radio', { name: 'Background' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Orphan' })).toBeDisabled();
  });
});

describe('Switch', () => {
  it('is a switch whose aria-checked follows the value', async () => {
    function Harness() {
      const [on, setOn] = useState(false);
      return <Switch checked={on} onChange={setOn} label="Auto-sync" />;
    }
    render(<Harness />);
    const sw = screen.getByRole('switch', { name: 'Auto-sync' });
    expect(sw).toHaveAttribute('aria-checked', 'false');
    sw.focus();
    await userEvent.keyboard(' ');
    expect(sw).toHaveAttribute('aria-checked', 'true');
    await userEvent.keyboard('{Enter}');
    expect(sw).toHaveAttribute('aria-checked', 'false');
  });
});

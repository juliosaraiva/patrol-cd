import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import {
  CONNECTION_STATES,
  ConnectionStatus,
  HEALTH_STATES,
  HealthStatus,
  OPERATION_PHASES,
  OperationPhaseStatus,
  SYNC_STATES,
  SyncStatus,
} from './Status';

function glyphMarkup(el: HTMLElement): string {
  const svg = el.querySelector('svg[data-glyph]');
  if (!svg) throw new Error('missing glyph');
  return svg.innerHTML;
}

function renderAll(family: 'health' | 'sync' | 'operation' | 'connection') {
  const states = { health: HEALTH_STATES, sync: SYNC_STATES, operation: OPERATION_PHASES, connection: CONNECTION_STATES }[family];
  const { container } = render(
    <>
      {states.map((s) => {
        switch (family) {
          case 'health':
            return <HealthStatus key={s} status={s} />;
          case 'sync':
            return <SyncStatus key={s} status={s} />;
          case 'operation':
            return <OperationPhaseStatus key={s} phase={s} />;
          case 'connection':
            return <ConnectionStatus key={s} status={s} />;
        }
      })}
    </>,
  );
  return Array.from(container.querySelectorAll<HTMLElement>(`[data-status-family="${family}"]`));
}

describe('status components', () => {
  it.each(['health', 'sync', 'operation', 'connection'] as const)('%s: every state has a text label and a distinct glyph', (family) => {
    const elements = renderAll(family);
    const glyphs = elements.map(glyphMarkup);
    expect(new Set(glyphs).size).toBe(glyphs.length);
    for (const el of elements) {
      expect(el.textContent?.trim()).not.toBe('');
      expect(el.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
    }
  });

  it('keeps the glyph families apart (no glyph shared between health, sync and operation)', () => {
    const all = (['health', 'sync', 'operation'] as const).flatMap((family) => renderAll(family).map(glyphMarkup));
    expect(new Set(all).size).toBe(all.length);
  });

  it('places sync glyphs in a box and operation phases in a tag', () => {
    const [sync] = renderAll('sync');
    const [op] = renderAll('operation');
    expect(sync?.querySelector('svg')?.parentElement?.className).toMatch(/syncBox/);
    expect(op?.className).toMatch(/operation/);
  });

  it('labels Out of sync in words and falls back to Unknown for unexpected values', () => {
    render(
      <>
        <SyncStatus status="OutOfSync" />
        <HealthStatus status="Weird" />
        <OperationPhaseStatus phase={undefined} />
      </>,
    );
    expect(screen.getByText('Out of sync')).toBeInTheDocument();
    const weird = screen.getByText('Weird').closest('[data-status]');
    expect(weird).toHaveAttribute('data-status', 'Unknown');
    expect(screen.getByText('Pending')).toBeInTheDocument();
  });

  it('keeps the label for assistive technology when visually hidden', () => {
    render(<HealthStatus status="Degraded" hideLabel description="2 pods crash-looping" />);
    expect(screen.getByText('Degraded')).toHaveClass('sr-only');
    expect(screen.getByText(/2 pods crash-looping/)).toBeInTheDocument();
  });
});

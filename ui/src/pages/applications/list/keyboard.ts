/** Keyboard helpers for the applications list (parity API-133..135). */

export function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (target.isContentEditable) return true;
  const tag = target.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') {
    const type = (target as HTMLInputElement).type;
    return !['checkbox', 'radio', 'button', 'submit', 'reset'].includes(type);
  }
  return false;
}

export function isModalOpen(): boolean {
  return document.querySelector('[aria-modal="true"]') !== null;
}

/** Digit (top row or numpad) pressed, as a number; null otherwise. */
export function digitOf(event: KeyboardEvent): number | null {
  if (/^Digit[0-9]$/.test(event.code) || /^Numpad[0-9]$/.test(event.code)) return Number(event.code.slice(-1));
  if (/^[0-9]$/.test(event.key)) return Number(event.key);
  return null;
}

/**
 * Next cursor for a tiles grid, or null when the key does not move it. Moves that would leave
 * [0, count) are rejected (the cursor stays), as upstream's useNav.
 */
export function nextTileCursor(key: string, cursor: number, count: number, columns: number): number | null {
  const step = (delta: number) => {
    const next = cursor < 0 ? 0 : cursor + delta;
    return next >= 0 && next < count ? next : cursor;
  };
  switch (key) {
    case 'ArrowRight':
      return step(1);
    case 'ArrowLeft':
      return cursor < 0 ? null : step(-1);
    case 'ArrowDown':
      return step(Math.max(1, columns));
    case 'ArrowUp':
      return cursor < 0 ? null : step(-Math.max(1, columns));
    default:
      return null;
  }
}

/** Tiles per row, measured from the rendered grid (1 when unknown). */
export function measureColumns(container: HTMLElement | null): number {
  if (!container) return 1;
  const tiles = Array.from(container.querySelectorAll<HTMLElement>('[data-tile-index]'));
  const first = tiles[0];
  if (!first) return 1;
  const top = first.getBoundingClientRect().top;
  const sameRow = tiles.filter((t) => t.getBoundingClientRect().top === top).length;
  return Math.max(1, sameRow);
}

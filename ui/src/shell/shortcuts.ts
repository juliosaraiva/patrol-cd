import { useEffect, useRef } from 'react';

function isEditable(target: EventTarget | null): boolean {
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

export interface GlobalShortcuts {
  /** "/" — focus the global search. */
  onSearch: () => void;
  /** "?" — open the keyboard shortcuts dialog. */
  onHelp: () => void;
}

/** Single-key shortcuts, ignored while typing in a field or with Ctrl/Meta/Alt held. */
export function useGlobalShortcuts(handlers: GlobalShortcuts): void {
  const ref = useRef(handlers);
  useEffect(() => {
    ref.current = handlers;
  });
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey) return;
      if (isEditable(event.target)) return;
      if (document.querySelector('[aria-modal="true"]')) return;
      if (event.key === '/') {
        event.preventDefault();
        ref.current.onSearch();
      } else if (event.key === '?') {
        event.preventDefault();
        ref.current.onHelp();
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, []);
}

export const SHORTCUTS: readonly { keys: string[]; description: string }[] = [
  { keys: ['/'], description: 'Focus search' },
  { keys: ['?'], description: 'Show keyboard shortcuts' },
  { keys: ['Esc'], description: 'Close a dialog or menu, clear the search field' },
  { keys: ['↑', '↓'], description: 'Move between table rows' },
  { keys: ['Enter'], description: 'Open the focused row' },
  { keys: ['Space'], description: 'Select the focused row' },
  { keys: ['←', '→'], description: 'Move between tabs' },
];

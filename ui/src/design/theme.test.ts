import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { getThemePreference, initTheme, setThemePreference, THEME_STORAGE_KEY, useTheme } from './theme';

describe('theme preference', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    setThemePreference('system');
  });

  it('applies a stored override to <html> and clears it for system', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'dark');
    expect(initTheme()).toBe('dark');
    expect(document.documentElement.dataset.theme).toBe('dark');
    setThemePreference('system');
    expect(document.documentElement.dataset.theme).toBeUndefined();
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBeNull();
  });

  it('ignores unknown stored values', () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, 'neon');
    expect(initTheme()).toBe('system');
  });

  it('keeps working when storage throws', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError');
    });
    expect(initTheme()).toBe('system');
    expect(() => setThemePreference('light')).not.toThrow();
    expect(getThemePreference()).toBe('light');
    expect(document.documentElement.dataset.theme).toBe('light');
  });

  it('notifies subscribers and resolves the effective theme', () => {
    const { result } = renderHook(() => useTheme());
    expect(result.current.resolved).toBe('light');
    act(() => result.current.setPreference('dark'));
    expect(result.current.preference).toBe('dark');
    expect(result.current.resolved).toBe('dark');
  });
});

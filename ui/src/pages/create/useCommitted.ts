import { useEffect, useState } from 'react';

/**
 * Value used for server lookups while the user types: the live value when the field is not
 * focused (picked from a list, set by the YAML editor), otherwise the last value seen unfocused.
 * Avoids one discovery request per keystroke.
 */
export function useCommitted(value: string, focused: boolean, immediate = false): string {
  const [committed, setCommitted] = useState(value);
  useEffect(() => {
    if (!focused || immediate) setCommitted(value);
  }, [value, focused, immediate]);
  return immediate ? value : committed;
}

import { useEffect } from 'react';

const PRODUCT = 'Patrol CD';

/** Sets document.title to "<parts> · Patrol CD" (most specific first). */
export function useDocumentTitle(...parts: (string | undefined | null | false)[]): void {
  const title = [...parts.filter((p): p is string => Boolean(p)), PRODUCT].join(' · ');
  useEffect(() => {
    document.title = title;
  }, [title]);
}

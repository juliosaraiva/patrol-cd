import { useQueryClient, type QueryKey } from '@tanstack/react-query';
import { useEffect, useRef } from 'react';
import type { StreamHandle } from '../sse';
import type { ListMeta, ObjectMeta, WatchEvent } from '../types';

interface KubeObject {
  metadata: ObjectMeta;
}

interface KubeList<T> {
  items: T[];
  metadata?: ListMeta;
}

/** Empty strings become undefined (omitted from query strings). */
export function nonEmpty(value: string | undefined): string | undefined {
  return value === '' ? undefined : value;
}

export function objectKey(obj: KubeObject): string {
  return `${obj.metadata.namespace ?? ''}/${obj.metadata.name ?? ''}`;
}

/**
 * Applies watch events to a list in order: ADDED/MODIFIED upsert by namespace/name (existing
 * items keep their position), DELETED removes. Returns the input when nothing changed.
 */
export function mergeWatchEvents<T extends KubeObject>(items: readonly T[], events: readonly WatchEvent<T>[]): T[] {
  if (events.length === 0) return items as T[];
  const index = new Map(items.map((item, i) => [objectKey(item), i]));
  const next: (T | undefined)[] = [...items];
  for (const event of events) {
    const key = objectKey(event.object);
    const at = index.get(key);
    if (event.type === 'DELETED') {
      if (at !== undefined) {
        next[at] = undefined;
        index.delete(key);
      }
      continue;
    }
    if (at !== undefined && next[at] !== undefined) {
      next[at] = event.object;
    } else {
      index.set(key, next.length);
      next.push(event.object);
    }
  }
  return next.filter((item): item is T => item !== undefined);
}

/** Latest resourceVersion seen in a batch of events (opaque; last one wins). */
function lastResourceVersion<T extends KubeObject>(events: readonly WatchEvent<T>[]): string | undefined {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const rv = events[i]?.object.metadata.resourceVersion;
    if (rv) return rv;
  }
  return undefined;
}

export interface ListWatchOptions<T extends KubeObject> {
  /** Query key of the list whose cache the events are merged into. */
  listKey: QueryKey;
  enabled: boolean;
  /** resourceVersion of the initial list (stream starts after it). */
  resourceVersion: string | undefined;
  /** Opens the stream; `resume()` returns the resourceVersion to resume from on reconnect. */
  open: (args: {
    resourceVersion: string | undefined;
    resume: () => string | undefined;
    onEvent: (event: WatchEvent<T>) => void;
  }) => StreamHandle;
  /** Detail cache key for an object, kept in sync with the stream. */
  detailKey?: (obj: T) => QueryKey;
  /** Events are applied in batches to bound re-render frequency (default 500 ms). */
  bufferMs?: number;
}

/**
 * Query options for a list kept live by useListWatch: the stream is the source of truth, so
 * focus and reconnect refetches (which would only restart the watch) are off. Explicit
 * refetches and invalidations still work; see useListWatch for how they are reconciled.
 */
export const watchedListOptions = { refetchOnWindowFocus: false, refetchOnReconnect: false } as const;

/**
 * Keeps a cached list fresh from an SSE watch. Starts once the list has loaded (so the stream
 * resumes from its resourceVersion); merged events keep the list's metadata, so they do not
 * restart it.
 *
 * A refetch replaces the cached items with a snapshot that may predate events already merged,
 * and the running watch would never resend those. So when a fetched list arrives with a new
 * metadata.resourceVersion, the watch restarts from it, and pending events of the old watch are
 * dropped instead of being merged onto the newer snapshot (the new watch resends what is newer).
 * Lists without a resourceVersion cannot be reconciled this way.
 */
export function useListWatch<T extends KubeObject>(options: ListWatchOptions<T>): void {
  const queryClient = useQueryClient();
  const { enabled, listKey, resourceVersion } = options;
  const ready = enabled && resourceVersion !== undefined;
  const optionsRef = useRef(options);
  const listKeyHash = JSON.stringify(listKey);
  useEffect(() => {
    optionsRef.current = options;
  });

  useEffect(() => {
    if (!ready) return undefined;
    const { open, bufferMs = 500 } = optionsRef.current;
    const startRV = resourceVersion;
    let latestRV = resourceVersion;
    let buffer: WatchEvent<T>[] = [];
    let timer: ReturnType<typeof setTimeout> | undefined;

    const flush = () => {
      timer = undefined;
      const batch = buffer;
      buffer = [];
      let superseded = false;
      queryClient.setQueryData<KubeList<T>>(listKey, (old) => {
        if (!old) return old;
        // A fetch replaced the list since this watch started; its restart (from the new
        // resourceVersion) is about to replace this watch.
        if ((old.metadata?.resourceVersion ?? '') !== startRV) {
          superseded = true;
          return old;
        }
        return { ...old, items: mergeWatchEvents(old.items, batch) };
      });
      if (superseded) return;
      latestRV = lastResourceVersion(batch) ?? latestRV;
      const { detailKey } = optionsRef.current;
      if (detailKey) {
        for (const event of batch) {
          const key = detailKey(event.object);
          if (event.type === 'DELETED') queryClient.removeQueries({ queryKey: key, exact: true });
          else queryClient.setQueryData(key, event.object);
        }
      }
    };

    const handle = open({
      resourceVersion,
      resume: () => latestRV,
      onEvent: (event) => {
        buffer.push(event);
        timer ??= setTimeout(flush, bufferMs);
      },
    });
    return () => {
      handle.close();
      if (timer !== undefined) clearTimeout(timer);
    };
    // listKeyHash stands in for listKey (a fresh array each render). resourceVersion changes
    // only when a fetch replaces the list (merges keep the list metadata): restart from it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queryClient, ready, listKeyHash, resourceVersion]);
}

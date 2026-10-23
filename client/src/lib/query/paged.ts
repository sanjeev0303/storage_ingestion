"use client";

import { useInfiniteQuery, type QueryKey } from "@tanstack/react-query";

export type CursorPage<T, C> = { items: T[]; next?: C | null };

type Options<T, C> = {
  queryKey: QueryKey;
  fetchPage: (cursor: C | undefined, signal: AbortSignal) => Promise<CursorPage<T, C>>;
  enabled?: boolean;
};

// The backend pages with a different cursor on almost every list (opaque
// strings, version numbers, positions). Each caller adapts its endpoint to
// a CursorPage; this hook only handles "load more".
export function usePagedQuery<T, C = string>({ queryKey, fetchPage, enabled = true }: Options<T, C>) {
  const query = useInfiniteQuery({
    queryKey,
    enabled,
    initialPageParam: undefined as C | undefined,
    queryFn: ({ pageParam, signal }) => fetchPage(pageParam as C | undefined, signal),
    getNextPageParam: (last) => (last.next === null || last.next === undefined || last.next === "" ? undefined : last.next),
  });

  const items = query.data?.pages.flatMap((page) => page.items) ?? [];
  return { ...query, items };
}

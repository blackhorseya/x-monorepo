import { useEffect } from "react";

// Polls the first page of a paged list every FIRST_PAGE_POLL_MS while the tab is visible (see
// refreshFirstPage in queries.ts for why only the first page). `refresh` is re-created per render;
// the interval restarts when `deps` change (the list's filter), not on every render.
export const FIRST_PAGE_POLL_MS = 2000;

export function useFirstPagePoll(refresh: () => Promise<unknown>, deps: readonly unknown[]) {
  useEffect(() => {
    const t = setInterval(() => {
      if (document.visibilityState === "visible") void refresh().catch(() => {});
    }, FIRST_PAGE_POLL_MS);
    return () => clearInterval(t);
    // `deps` is the caller's list of what `refresh` closes over.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

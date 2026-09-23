import { UnauthorizedError } from "@/api";
import { useCallback, useEffect, useRef, useState } from "react";

/** How often to re-fetch while the tab is visible. */
export const POLL_MS = 60_000;
/** Older than this and the freshness indicator turns amber. */
export const STALE_MS = 5 * 60_000;

type LiveState<T> = {
  /** The last failure was an expired session rather than an outage: it needs a login, not a retry. */
  unauthorized: boolean;
  data: T | null;
  /** What the last load threw, or null. Kept raw so the page can tell an outage from a rejection. */
  error: unknown;
  /** True while the data on screen belongs to another key; never for a background poll. */
  loading: boolean;
  updatedAt: number | null;
  refreshing: boolean;
  refresh: () => void;
};

/**
 * Fetches on mount, whenever `key` changes, and on an interval.
 *
 * A background poll updates in place and never sets `loading`. Polling stops
 * while the tab is hidden, so a dashboard left on a second monitor is not
 * polling all night, and fires on return. `loading` is derived from the key
 * the data belongs to: set in an effect it would be a synchronous setState
 * in an effect body.
 */
export function useLiveData<T>(
  load: (signal: AbortSignal) => Promise<T>,
  key: string,
): LiveState<T> {
  const [result, setResult] = useState<{
    key: string;
    data: T | null;
    error: unknown;
    unauthorized: boolean;
    updatedAt: number | null;
  }>({ key: "", data: null, error: null, unauthorized: false, updatedAt: null });
  const [refreshing, setRefreshing] = useState(false);

  // Assigned in an effect: a ref written during render is a side effect React may discard.
  const loadRef = useRef(load);
  useEffect(() => {
    loadRef.current = load;
  }, [load]);

  const generation = useRef(0);

  // The request set in flight. A newer one aborts it, so a superseded filter's
  // queries stop contending for the server's read connection.
  const inFlight = useRef<AbortController | null>(null);

  // Sets no state synchronously, so an effect body may call it; marking a
  // refresh in flight is left to event handlers and timers.
  const run = useCallback((forKey: string) => {
    const gen = ++generation.current;
    inFlight.current?.abort();
    const controller = new AbortController();
    inFlight.current = controller;

    loadRef
      .current(controller.signal)
      .then((data) => {
        // A superseded request must not overwrite newer data.
        if (gen !== generation.current) return;
        setResult({
          key: forKey,
          data,
          error: null,
          unauthorized: false,
          updatedAt: Date.now(),
        });
      })
      .catch((e: unknown) => {
        if (gen !== generation.current) return;
        // An abort is this hook superseding itself, not a failure.
        if (e instanceof DOMException && e.name === "AbortError") return;
        // A failed poll keeps the last good data. A failed *new* key drops it:
        // the old range's numbers do not answer the range now selected.
        setResult((r) => ({
          ...r,
          key: forKey,
          ...(forKey === r.key ? null : { data: null, updatedAt: null }),
          error: e,
          unauthorized: e instanceof UnauthorizedError,
        }));
      })
      .finally(() => {
        if (gen !== generation.current) return;
        setRefreshing(false);
      });
  }, []);

  useEffect(() => {
    run(key);
    // Captured now: at cleanup the ref may already point at a newer request.
    const started = inFlight.current;
    return () => started?.abort();
  }, [key, run]);

  // Restarted with the key, so the timer always fetches the current range.
  useEffect(() => {
    let timer: number | undefined;
    const stop = () => {
      if (timer !== undefined) window.clearInterval(timer);
      timer = undefined;
    };
    const start = () => {
      stop();
      timer = window.setInterval(() => {
        if (document.visibilityState !== "visible") return;
        setRefreshing(true);
        run(key);
      }, POLL_MS);
    };
    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        setRefreshing(true);
        run(key);
        start();
      } else {
        stop();
      }
    };

    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [key, run]);

  return {
    data: result.data,
    error: result.error,
    unauthorized: result.unauthorized,
    loading: result.key !== key,
    updatedAt: result.updatedAt,
    refreshing,
    refresh: () => {
      setRefreshing(true);
      run(key);
    },
  };
}

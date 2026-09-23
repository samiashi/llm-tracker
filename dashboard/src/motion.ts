import { useSyncExternalStore } from "react";

const QUERY = "(prefers-reduced-motion: reduce)";

// jsdom has no matchMedia, and a component test must not crash on a
// preference it has no way to express.
function mediaQuery(): MediaQueryList | null {
  return typeof window !== "undefined" && typeof window.matchMedia === "function"
    ? window.matchMedia(QUERY)
    : null;
}

function subscribe(onChange: () => void): () => void {
  const m = mediaQuery();
  m?.addEventListener("change", onChange);
  return () => m?.removeEventListener("change", onChange);
}

const reducedNow = () => mediaQuery()?.matches ?? false;

/**
 * Whether the reader asked for reduced motion, read live. Recharts animates in
 * JavaScript, out of reach of the CSS media query, and these charts redraw on
 * every poll: without this their entry animation replays once a minute.
 */
export function usePrefersReducedMotion(): boolean {
  return useSyncExternalStore(subscribe, reducedNow, () => false);
}

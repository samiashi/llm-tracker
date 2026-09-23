import { useSyncExternalStore } from "react";

/**
 * Which card is expanded lives in the URL (`?card=by-model`), so a link can
 * open one and Back closes it: people press Back to leave a full-screen view,
 * and without an entry of its own that would leave the dashboard instead.
 * Opening pushes an entry; closing pops it.
 */
export const CARD_PARAM = "card";

/** Opening and closing change the URL without a popstate; this tells readers. */
const CHANGED = "llm-tracker:card";

/** A card's id in the URL, from its title: "By model" is `by-model`. */
export function cardId(title: string): string {
  return title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

const current = () => new URLSearchParams(window.location.search).get(CARD_PARAM);

function subscribe(onChange: () => void) {
  window.addEventListener("popstate", onChange);
  window.addEventListener(CHANGED, onChange);
  return () => {
    window.removeEventListener("popstate", onChange);
    window.removeEventListener(CHANGED, onChange);
  };
}

/** The expanded card's id, or null. */
export function useExpandedCard(): string | null {
  return useSyncExternalStore(subscribe, current);
}

export function openCard(id: string) {
  if (current() === id) return;
  const url = new URL(window.location.href);
  url.searchParams.set(CARD_PARAM, id);
  window.history.pushState({ card: id }, "", url);
  window.dispatchEvent(new Event(CHANGED));
}

export function closeCard(id: string) {
  if (current() !== id) return;
  // Pop the entry that opening pushed, so Back afterwards behaves as if the
  // card had never been open. A link that arrived open has none to pop.
  const state: unknown = window.history.state;
  if (typeof state === "object" && state !== null && "card" in state && state.card === id) {
    window.history.back();
    return;
  }
  const url = new URL(window.location.href);
  url.searchParams.delete(CARD_PARAM);
  window.history.replaceState(null, "", url);
  window.dispatchEvent(new Event(CHANGED));
}

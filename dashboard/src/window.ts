import type { Filter } from "@/api";
import { CARD_PARAM } from "@/expanded";

/**
 * What the user asked for: a relative window ("last 7 days") or two fixed
 * dates. The concrete range is derived from this and the clock, so a relative
 * window follows midnight instead of freezing on the day it was chosen.
 */
export type Intent = { preset?: number; from?: string; to?: string; person?: string };

/** The widest range, in days: the "All" preset. Every day axis gap-fills at most this many. */
export const MAX_DAYS = 3650;

/**
 * The UTC day `daysAgo` days before `now`, as an ISO date.
 *
 * UTC because the server stores, rolls up and prunes by UTC day. A local
 * "today" ends before the newest events west of UTC and starts after them
 * east of it. UTC has no DST, so the day arithmetic is exact.
 */
export function isoAt(now: number, daysAgo = 0): string {
  const d = new Date(now);
  d.setUTCDate(d.getUTCDate() - daysAgo);
  return d.toISOString().slice(0, 10);
}

/**
 * A real calendar day in ISO form. Round-tripped, because Date parsing rolls
 * an impossible day over ("2026-04-31" reads as 1 May) and the server rejects it.
 */
export function isISODate(v: string | null): v is string {
  if (!v || !/^\d{4}-\d{2}-\d{2}$/.test(v)) return false;
  const d = new Date(`${v}T00:00:00Z`);
  return !Number.isNaN(d.getTime()) && d.toISOString().slice(0, 10) === v;
}

const clamp = (day: string, lo: string, hi: string) => (day < lo ? lo : day > hi ? hi : day);

/**
 * The concrete range for an intent. Pure given `now`, so safe during render.
 *
 * The one place a range is bounded: ends are ordered, then clamped to the
 * last MAX_DAYS UTC days, so no source (a link, a typed date, a preset) can
 * send a reversed range or gap-fill years that have no data.
 */
export function resolve(intent: Intent, now: number): Filter {
  const today = isoAt(now);
  const floor = isoAt(now, MAX_DAYS - 1);
  let from: string;
  let to: string;
  if (intent.preset) {
    const days = Number.isFinite(intent.preset) ? Math.max(1, Math.trunc(intent.preset)) : MAX_DAYS;
    from = isoAt(now, Math.min(days, MAX_DAYS) - 1);
    to = today;
  } else {
    // `||`: a cleared date box yields "", which would send `from=` and let the
    // server pick its own window without anything on screen saying so.
    from = intent.from || isoAt(now, 29);
    to = intent.to || today;
    if (from > to) [from, to] = [to, from];
  }
  return { from: clamp(from, floor, today), to: clamp(to, floor, today), person: intent.person };
}

/**
 * The intent a link carries, so a view can be shared. Everything here arrives
 * from a pasted URL: a malformed value falls back to the default window, and
 * dates go through `resolve`, so the recipient sees the range the sender saw.
 */
export function intentFromURL(now: number = Date.now()): Intent {
  const p = new URLSearchParams(window.location.search);
  const person = p.get("person") || undefined;

  const preset = Number(p.get("preset"));
  if (Number.isInteger(preset) && preset > 0 && preset <= MAX_DAYS) return { preset, person };

  const from = p.get("from");
  const to = p.get("to");
  if (isISODate(from) && isISODate(to)) {
    const r = resolve({ from, to }, now);
    return { from: r.from, to: r.to, person };
  }
  return { preset: 30, person };
}

export function writeIntentToURL(i: Intent) {
  // A preset is written as the preset, so a shared link means "the last 7
  // days" to whoever opens it rather than the week the sender was looking at.
  const p = i.preset
    ? new URLSearchParams({ preset: String(i.preset) })
    : new URLSearchParams({ from: i.from ?? "", to: i.to ?? "" });
  if (i.person) p.set("person", i.person);
  // An expanded card is a history entry of its own, not part of the range:
  // it keeps its parameter and its state, which is how closing it knows to
  // pop the entry rather than leave one behind.
  const card = new URLSearchParams(window.location.search).get(CARD_PARAM);
  if (card) p.set(CARD_PARAM, card);
  // Replace, not push: adjusting a range is not navigation, and Back should leave the page.
  window.history.replaceState(window.history.state, "", `?${p}`);
}

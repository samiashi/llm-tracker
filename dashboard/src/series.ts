/**
 * Calendar helpers for time series.
 *
 * The API returns only days that had activity, and Recharts' categorical
 * x-axis draws that list as consecutive points: a workless weekend renders as
 * wide as one night, and every line is displaced from its real dates. Filling
 * the gaps keeps the x-axis proportional to time.
 */

/**
 * Every ISO date from `from` to `to` inclusive.
 *
 * Iterates in UTC, so the result does not depend on the viewer's timezone. Not
 * local midnight: where the DST jump is at midnight (Santiago, Havana, Cairo,
 * Tehran), 00:00 does not exist that day and the engine returns 01:00. Every
 * later step carries that hour, so `cursor <= last` against a 00:00 bound
 * stops a day early and every chart silently loses its most recent day.
 */
export function calendarDays(from: string, to: string): string[] {
  const out: string[] = [];
  const cursor = new Date(`${from}T00:00:00Z`);
  const last = new Date(`${to}T00:00:00Z`);
  if (isNaN(cursor.getTime()) || isNaN(last.getTime())) return out;
  // `cursor` advances by mutation, which this rule does not see as movement.
  // oxlint-disable-next-line no-unmodified-loop-condition
  while (cursor <= last) {
    out.push(cursor.toISOString().slice(0, 10));
    cursor.setUTCDate(cursor.getUTCDate() + 1);
  }
  return out;
}

/**
 * Expand rows to cover every day of `range` (without one, the first day
 * present to the last), inserting `blank(day)` for the missing days.
 *
 * Filled across the range asked for, not the days that came back: a collector
 * that stopped three days ago must leave three visibly empty days, not a chart
 * that ends early with its last point against the right edge.
 */
export function fillDays<T>(
  rows: T[],
  dayOf: (row: T) => string,
  blank: (day: string) => T,
  range?: { from: string; to: string },
): T[] {
  const byDay = new Map(rows.map((r) => [dayOf(r), r]));
  const present = [...byDay.keys()].sort();

  const from = range?.from || present[0];
  const to = range?.to || present[present.length - 1];
  if (!from || !to) return rows;

  return calendarDays(from, to).map((d) => byDay.get(d) ?? blank(d));
}

/** The window the page is showing; charts gap-fill to it (see `fillDays`). */
export type DayRange = { from: string; to: string };

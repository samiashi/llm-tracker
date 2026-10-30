import type { HeatCell } from "@/api";
import { calendarDays, calendarHours } from "@/series";
import type { DayRange } from "@/series";
import { offsetSeconds } from "@/timezone";

export type Row = {
  day: string;
  weekday: number;
  hours: Map<number, HeatCell>;
  total: number;
  /** A server-local day outside the selected UTC range. */
  outside: boolean;
  /** Present for rolling windows: missing clock hours are excluded, not idle. */
  selectedHours?: Set<number>;
};

/**
 * The grid the heatmap draws: one row per day, newest first, gap-filled over
 * the range, with the quantile cuts that map tokens onto `levels` shades (the
 * first of them empty). Pure, so it is tested without rendering.
 */
export function buildHeatModel(
  cells: HeatCell[],
  range: DayRange,
  detailFrom: string | undefined,
  levels: number,
) {
  const { from, to, start, end, timezone = "UTC" } = range;
  const rolling = start !== undefined && end !== undefined;
  if (cells.length === 0 && !rolling) return null;

  // Derive local dates from the exact window, not its UTC dates or the days
  // with activity. Otherwise an idle local edge date disappears and an
  // unrelated UTC date is drawn instead. Reuse the series' DST-aware buckets:
  // repeated hours share a cell, nonexistent hours are never marked idle.
  const selected = new Map<string, Set<number>>();
  if (rolling) {
    for (const instant of calendarHours(start, end, timezone)) {
      const at = Date.parse(instant);
      const local = new Date(at + offsetSeconds(at, timezone) * 1000);
      const day = local.toISOString().slice(0, 10);
      const hours = selected.get(day) ?? new Set<number>();
      hours.add(local.getUTCHours());
      selected.set(day, hours);
    }
    if (selected.size === 0) return null;
  }

  const byDay = new Map<string, Row>();
  for (const c of cells) {
    let row = byDay.get(c.day);
    if (!row) {
      const outside = !rolling && ((!!from && c.day < from) || (!!to && c.day > to));
      row = { day: c.day, weekday: c.weekday, hours: new Map(), total: 0, outside };
      byDay.set(c.day, row);
    }
    row.hours.set(c.hour, c);
    row.total += c.tokens;
  }

  // The server selects UTC days but buckets by its own local day, so an
  // event of the range's first or last UTC day can land on the local day
  // beside it. Rows span both, or the legend counts cells no row draws.
  const present = [...byDay.keys()].sort();
  const earliest = present[0];
  const latest = present[present.length - 1];
  const selectedDays = [...selected.keys()].sort();
  const first = rolling ? selectedDays[0] : from && from < earliest ? from : earliest;
  const last = rolling ? selectedDays[selectedDays.length - 1] : to && to > latest ? to : latest;

  // A day with no work keeps its row: skipping it would compress an idle week
  // into a busy one. A day before detailFrom has no hourly record at all, so
  // it is not drawn as idle; the whole span is named once instead.
  const rows: Row[] = [];
  const pruned: string[] = [];
  for (const iso of calendarDays(first, last)) {
    // Retention bounds are UTC dates. The API rejects unavailable rolling
    // windows; comparing their local dates with detailFrom would hide idle
    // hours west of UTC that still have full detail.
    if (!rolling && !byDay.has(iso) && detailFrom && iso < detailFrom) {
      pruned.push(iso);
      continue;
    }
    const row = byDay.get(iso) ?? {
      day: iso,
      weekday: new Date(`${iso}T00:00:00Z`).getUTCDay(),
      hours: new Map<number, HeatCell>(),
      total: 0,
      outside: false,
    };
    rows.push({ ...row, selectedHours: rolling ? selected.get(iso) : undefined });
  }
  rows.reverse(); // newest first: the rows people care about are recent ones

  // Quantiles over non-empty hours, so a mostly idle range does not push
  // every working hour into the darkest step. N filled steps need N-1
  // boundaries; duplicates collapse, so no legend entry names an unused shade.
  const values = cells
    .map((c) => c.tokens)
    .filter((v) => v > 0)
    .sort((a, b) => a - b);
  const filled = levels - 1;
  const rawCuts =
    values.length === 0
      ? []
      : Array.from(
          { length: filled - 1 },
          (_, i) =>
            values[Math.min(values.length - 1, Math.floor(((i + 1) / filled) * values.length))],
        );
  const cuts = [...new Set(rawCuts)];

  const step = (v: number) => {
    if (v <= 0) return 0;
    let i = 0;
    while (i < cuts.length && v > cuts[i]) i++;
    return Math.min(levels - 1, i + 1);
  };

  return {
    rolling,
    rows,
    byIso: new Map(rows.map((r) => [r.day, r])),
    cuts,
    step,
    pruned,
    maxTotal: Math.max(...rows.map((r) => r.total), 1),
    behind: !rolling && !!from && earliest < from,
    ahead: !rolling && !!to && latest > to,
  };
}

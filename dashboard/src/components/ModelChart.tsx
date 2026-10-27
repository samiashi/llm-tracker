import type { ModelDay } from "@/api";
import { shortDay, spansYears } from "@/format";
import { MODEL_SLOTS } from "@/palette";
import { calendarDays, calendarHours, hourLabel } from "@/series";
import type { DayRange } from "@/series";
import { OverlaidAreas } from "@/components/OverlaidAreas";

/**
 * Model mix over time: a team moving from a premium model to a cheaper one
 * looks identical in total tokens while costing a fraction as much.
 */
export function ModelChart({
  points,
  range,
  height = 220,
}: {
  points: ModelDay[];
  range?: DayRange;
  height?: number;
}) {
  if (points.length === 0) return <p className="empty">No activity in this range.</p>;

  // Ranked over the whole range, so a colour means one model on every day.
  const totals = new Map<string, number>();
  for (const p of points) totals.set(p.model, (totals.get(p.model) ?? 0) + p.tokens);
  const models = [...totals.entries()]
    .sort((a, b) => b[1] - a[1])
    .map(([m]) => m)
    .slice(0, MODEL_SLOTS.length);

  const byDay = new Map<string, Record<string, number>>();
  for (const p of points) {
    const row = byDay.get(p.day) ?? {};
    row[p.model] = (row[p.model] ?? 0) + p.tokens;
    byDay.set(p.day, row);
  }

  // Every day of the range (see series.ts), and every model on every day: a
  // missing model draws as a gap rather than the zero it is.
  const days = [...byDay.keys()].sort();
  const withYear = spansYears(range?.from, range?.to);
  const hourly = range?.start !== undefined && range?.end !== undefined;
  const buckets = hourly
    ? calendarHours(range.start!, range.end!)
    : calendarDays(range?.from || days[0], range?.to || days[days.length - 1]);
  const data = buckets.map((day) => {
    const row: Record<string, number | string> = {
      day: hourly ? hourLabel(day) : shortDay(day, withYear),
    };
    const present = byDay.get(day);
    for (const m of models) row[m] = present?.[m] ?? 0;
    return row;
  });

  return (
    <OverlaidAreas
      data={data}
      lines={models.map((m, i) => ({ key: m, name: m, color: MODEL_SLOTS[i] }))}
      height={height}
      showTotal={false}
      label={`Tokens per ${hourly ? "hour (UTC)" : "day"} for the busiest models, ${data.length} ${hourly ? "hourly buckets" : "days"}`}
    />
  );
}

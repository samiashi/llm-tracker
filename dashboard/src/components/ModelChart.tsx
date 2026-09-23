import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { ModelDay } from "@/api";
import { shortDay, tokens, spansYears } from "@/format";
import { MODEL_SLOTS } from "@/palette";
import { calendarDays } from "@/series";
import type { DayRange } from "@/series";
import { ChartTooltip, Legend } from "@/components/parts";
import { usePrefersReducedMotion } from "@/motion";

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
  const reducedMotion = usePrefersReducedMotion();
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
  const data = calendarDays(range?.from || days[0], range?.to || days[days.length - 1]).map(
    (day) => {
      const row: Record<string, number | string> = { day: shortDay(day, withYear) };
      const present = byDay.get(day);
      for (const m of models) row[m] = present?.[m] ?? 0;
      return row;
    },
  );

  return (
    <>
      <Legend items={models.map((m, i) => ({ color: MODEL_SLOTS[i], label: m }))} />
      <ResponsiveContainer width="100%" height={height}>
        <AreaChart
          accessibilityLayer
          role="img"
          aria-label={`Tokens per day for the busiest models, ${data.length} days`}
          data={data}
          margin={{ top: 4, right: 6, left: 0, bottom: 0 }}
        >
          <CartesianGrid stroke="var(--grid)" vertical={false} />
          <XAxis
            dataKey="day"
            tick={{ fill: "var(--text-muted)", fontSize: 11 }}
            stroke="var(--axis)"
            tickLine={false}
            minTickGap={24}
          />
          <YAxis
            tick={{ fill: "var(--text-muted)", fontSize: 11 }}
            stroke="var(--axis)"
            tickLine={false}
            axisLine={false}
            width={44}
            tickFormatter={(v) => tokens(v as number)}
          />
          <Tooltip
            content={<ChartTooltip showTotal={false} />}
            cursor={{ stroke: "var(--axis)", strokeWidth: 1 }}
          />
          {models.map((m, i) => (
            <Area
              key={m}
              type="monotone"
              dataKey={m}
              name={m}
              // Not stacked: the highest line is the heaviest model. Stacked,
              // the top line is a running total no model ever reached.
              stroke={MODEL_SLOTS[i]}
              strokeWidth={2}
              fill={MODEL_SLOTS[i]}
              fillOpacity={0.16}
              activeDot={{ r: 4, strokeWidth: 2, stroke: "var(--surface-1)" }}
              isAnimationActive={!reducedMotion}
            />
          ))}
        </AreaChart>
      </ResponsiveContainer>
    </>
  );
}

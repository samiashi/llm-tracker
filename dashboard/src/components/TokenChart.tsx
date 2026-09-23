import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { Daily, Totals } from "@/api";
import { shortDay, tokens, spansYears } from "@/format";
import { fillDays } from "@/series";
import type { DayRange } from "@/series";
import { ChartTooltip, Legend } from "@/components/chart";
import { usePrefersReducedMotion } from "@/motion";

/**
 * A numeric field of Totals. Tied to the type, a field renamed server-side is
 * a build error rather than a flat line at zero.
 */
type NumericTotal = {
  [K in keyof Totals]: Totals[K] extends number ? K : never;
}[keyof Totals];

type Series = { key: NumericTotal; name: string; color: string };

/** A day with no activity: every measure is zero, not missing. */
const EMPTY_TOTALS: Totals = {
  events: 0,
  total_tokens: 0,
  input_tokens: 0,
  output_tokens: 0,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  billed_usd: 0,
  billed_tokens: 0,
  rate_card_usd: 0,
  unknown_basis_usd: 0,
  unpriced_tokens: 0,
};

/**
 * Series over time, each on its own baseline rather than stacked, so a line's
 * height is its own volume. Billable I/O and cache reads are separate charts:
 * cache reads run about forty times larger and would flatten the rest.
 */
export function TokenChart({
  days,
  series,
  range,
  height = 200,
}: {
  days: Daily[];
  series: Series[];
  range?: DayRange;
  height?: number;
}) {
  const reducedMotion = usePrefersReducedMotion();
  // Checked before gap-filling, which would turn no data into confident zeros.
  if (days.length === 0) return <p className="empty">No activity in this range.</p>;

  const filled = fillDays(
    days,
    (d) => d.day,
    (day) => ({ day, totals: EMPTY_TOTALS }),
    range,
  );
  const withYear = spansYears(range?.from, range?.to);
  const data = filled.map((d) => ({
    day: shortDay(d.day, withYear),
    ...Object.fromEntries(series.map((s) => [s.key, d.totals[s.key]])),
  }));

  return (
    <>
      <Legend items={series.map((s) => ({ color: s.color, label: s.name }))} />
      <ResponsiveContainer width="100%" height={height}>
        <AreaChart
          accessibilityLayer
          role="img"
          aria-label={`${series.map((s) => s.name).join(" and ")} per day, ${filled.length} days`}
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
          <Tooltip content={<ChartTooltip />} cursor={{ stroke: "var(--axis)", strokeWidth: 1 }} />
          {series.map((s) => (
            <Area
              key={s.key}
              type="monotone"
              dataKey={s.key}
              name={s.name}
              stroke={s.color}
              strokeWidth={2}
              fill={s.color}
              // Translucent, so a series behind another stays visible.
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

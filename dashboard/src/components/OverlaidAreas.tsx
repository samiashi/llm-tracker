import {
  Area,
  AreaChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import { tokens } from "@/format";
import { ChartTooltip, Legend } from "@/components/chart";
import { usePrefersReducedMotion } from "@/motion";

/** One series: the row field it reads, its name in the legend and tooltip, and its colour. */
export type Line = { key: string; name: string; color: string };

/**
 * Series per day, drawn over one another, each from its own baseline. Never
 * stacked: the top line of a stack is a running total no series reached, and
 * every reader takes the highest line for the biggest series. The entry
 * animation follows prefers-reduced-motion, since these charts redraw on
 * every poll.
 */
export function OverlaidAreas({
  data,
  lines,
  label,
  height,
  showTotal = true,
}: {
  /** One row per day, keyed `day` for the axis and by each line's key. */
  data: Record<string, string | number>[];
  lines: Line[];
  /** What the chart shows, for a screen reader. */
  label: string;
  height: number;
  /** False where the chart draws a selection; see ChartTooltip. */
  showTotal?: boolean;
}) {
  const reducedMotion = usePrefersReducedMotion();
  return (
    <>
      <Legend items={lines.map((l) => ({ color: l.color, label: l.name }))} />
      <ResponsiveContainer width="100%" height={height}>
        <AreaChart
          accessibilityLayer
          role="img"
          aria-label={label}
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
            content={<ChartTooltip showTotal={showTotal} />}
            cursor={{ stroke: "var(--axis)", strokeWidth: 1 }}
          />
          {lines.map((l) => (
            <Area
              key={l.key}
              type="monotone"
              dataKey={l.key}
              name={l.name}
              stroke={l.color}
              strokeWidth={2}
              fill={l.color}
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

import { exact } from "@/format";

export function Legend({ items }: { items: { color: string; label: string }[] }) {
  return (
    <div className="legend">
      {items.map((i) => (
        <span className="pill" key={i.label}>
          <span className="dot" style={{ background: i.color }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}

/** Recharts types its tooltip payload loosely, so the shape is declared here at the boundary. */
type TooltipEntry = { dataKey?: string; name?: string; value?: number; color?: string };
type ChartTooltipProps = {
  active?: boolean;
  payload?: TooltipEntry[];
  label?: string;
  /**
   * False where the chart draws a selection rather than everything: a "Total"
   * under six of nineteen models reads as the day's total and is not.
   */
  showTotal?: boolean;
};

export function ChartTooltip({ active, payload, label, showTotal = true }: ChartTooltipProps) {
  if (!active || !payload?.length) return null;
  // Largest first, so the biggest contributor is never at the bottom.
  const rows = [...payload].sort((a, b) => (b.value ?? 0) - (a.value ?? 0));
  const total = rows.reduce((sum, p) => sum + (p.value ?? 0), 0);
  return (
    <div className="tooltip">
      <div className="t-day">{label}</div>
      {rows.map((p) => (
        <div className="t-row" key={p.dataKey}>
          <span>
            <span
              className="dot"
              style={{ background: p.color, display: "inline-block", marginRight: 6 }}
            />
            {p.name}
          </span>
          <b>{exact(p.value ?? 0)}</b>
        </div>
      ))}
      {showTotal && rows.length > 1 && (
        <div className="t-row t-total">
          <span>Total</span>
          <b>{exact(total)}</b>
        </div>
      )}
    </div>
  );
}

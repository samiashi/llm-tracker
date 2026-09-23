import type { ReactNode } from "react";
import type { Group } from "@/api";
import { More } from "@/components/Card";

/**
 * Magnitudes as horizontal bars: length reads far more accurately than angle.
 * A `value` of null is a row that exists but has no number to show: it prints
 * a dash and draws no bar, rather than a zero or a guess.
 */
export function BarList({
  groups,
  color,
  value,
  format,
  max = 8,
  emptyNote = "No data in this range.",
  onSelect,
  selected,
  summable = true,
  onMore,
  mark,
}: {
  groups: Group[];
  color: string;
  value: (g: Group) => number | null;
  format: (n: number) => string;
  max?: number;
  emptyNote?: string;
  /** When set, rows become buttons that drill into that key. */
  onSelect?: (key: string) => void;
  selected?: string;
  /** False for rates and ratios: hidden rows of those cannot be summed into a total. */
  summable?: boolean;
  /** Shows the hidden rows, making the "+N more" line a button. */
  onMore?: () => void;
  /** Something to say beside a row's name, such as that it has no price. */
  mark?: (g: Group) => ReactNode;
}) {
  const all = groups.filter((g) => {
    const v = value(g);
    return v === null || v > 0;
  });
  const rows = all.slice(0, max);
  if (rows.length === 0) return <p className="empty">{emptyNote}</p>;
  const top = Math.max(0, ...rows.map((g) => value(g) ?? 0));
  const hidden = all.length - rows.length;
  return (
    <div className="barlist">
      {rows.map((g) => {
        const v = value(g);
        const clickable = Boolean(onSelect);
        return (
          <div
            className={[
              "barrow",
              clickable ? "clickable" : "",
              selected === g.key ? "selected" : "",
            ]
              .filter(Boolean)
              .join(" ")}
            key={g.key}
            // A div that only answers a mouse is not a control.
            role={clickable ? "button" : undefined}
            tabIndex={clickable ? 0 : undefined}
            onClick={clickable ? () => onSelect!(g.key) : undefined}
            onKeyDown={
              clickable
                ? (e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      onSelect!(g.key);
                    }
                  }
                : undefined
            }
          >
            <div className="head">
              <span className="name" title={g.key}>
                {g.key}
              </span>
              {mark?.(g)}
              <span className="val">{v === null ? "—" : format(v)}</span>
            </div>
            <div className="bartrack">
              {v !== null && top > 0 && (
                <div
                  className="barfill"
                  style={{ width: `${Math.max(2, (v / top) * 100)}%`, background: color }}
                />
              )}
            </div>
          </div>
        );
      })}
      {hidden > 0 && (
        <More onMore={onMore}>
          {summable
            ? `+${hidden} more, ${format(all.slice(max).reduce((s, g) => s + (value(g) ?? 0), 0))} combined`
            : `+${hidden} more not shown`}
        </More>
      )}
    </div>
  );
}

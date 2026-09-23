import { Fragment, useMemo } from "react";
import type { MatrixCell } from "@/api";
import { tokens, usd } from "@/format";
import { More } from "@/components/Card";
import { Legend } from "@/components/chart";
import { OTHER_FILL, SLOTS } from "@/palette";

/** The key for columns past the palette; no normalised value can equal it. */
const OTHER = "\0other";

/** "High" and " high" are one level: columns are keyed and ranked on this. */
const norm = (col: string) => col.trim().toLowerCase();

type Segment = {
  key: string;
  tokens: number;
  billed: number;
  rateCard: number;
  unknownBasis: number;
};

const share = (part: number, whole: number) => {
  const pct = whole > 0 ? (part / whole) * 100 : 0;
  return pct > 0 && pct < 1 ? "<1%" : `${Math.round(pct)}%`;
};

/**
 * A two-dimensional breakdown as one stacked bar per row: which model ran at
 * which effort, since what an effort level costs depends on the model. The
 * segments are part-to-whole of their row, so stacking them is honest.
 */
export function MatrixBars({
  cells,
  colOrder,
  max = 6,
  onMore,
}: {
  cells: MatrixCell[];
  /** Canonical column order for an ordinal dimension; volume order when absent. */
  colOrder?: string[];
  max?: number;
  /** Shows the hidden rows, making the "+N more" line a button. */
  onMore?: () => void;
}) {
  const model = useMemo(() => {
    if (cells.length === 0) return null;

    const rank = new Map((colOrder ?? []).map((c, i) => [norm(c), i]));
    const names = new Map((colOrder ?? []).map((c) => [norm(c), c]));
    names.set(OTHER, "other");
    const colTotals = new Map<string, number>();
    const rowTotals = new Map<string, number>();
    for (const c of cells) {
      const k = norm(c.col);
      if (!names.has(k)) names.set(k, c.col.trim());
      colTotals.set(k, (colTotals.get(k) ?? 0) + c.tokens);
      rowTotals.set(c.row, (rowTotals.get(c.row) ?? 0) + c.tokens);
    }

    // The canonical scale when there is one, volume otherwise; fixed across
    // rows either way, so a colour means one level in every bar.
    const unranked = colOrder?.length ?? 0;
    const ordered = [...colTotals.entries()]
      .sort(
        (a, b) =>
          (colOrder ? (rank.get(a[0]) ?? unranked) - (rank.get(b[0]) ?? unranked) : 0) ||
          b[1] - a[1],
      )
      .map(([k]) => k);

    // Columns past the palette fold into one declared "other" segment, so a
    // bar always reaches its own total and the legend explains every segment.
    const shown = ordered.slice(0, SLOTS.length);
    const overflow = new Set(ordered.slice(SLOTS.length));
    const cols = overflow.size > 0 ? [...shown, OTHER] : shown;
    const colour = new Map(shown.map((k, i) => [k, SLOTS[i]]));
    colour.set(OTHER, OTHER_FILL);

    const byRow = new Map<string, Map<string, Segment>>();
    for (const c of cells) {
      const key = overflow.has(norm(c.col)) ? OTHER : norm(c.col);
      const segments = byRow.get(c.row) ?? new Map<string, Segment>();
      const s = segments.get(key) ?? { key, tokens: 0, billed: 0, rateCard: 0, unknownBasis: 0 };
      s.tokens += c.tokens;
      s.billed += c.billed_usd;
      s.rateCard += c.rate_card_usd;
      s.unknownBasis += c.unknown_basis_usd;
      segments.set(key, s);
      byRow.set(c.row, segments);
    }

    const ranked = [...rowTotals.entries()].sort((a, b) => b[1] - a[1]);
    const hidden = ranked.slice(max);
    const rows = ranked.slice(0, max).map(([row, total]) => {
      const segments = cols.flatMap((k) => byRow.get(row)?.get(k) ?? []);
      // Billed only: added to the rate-card equivalent it would be neither
      // spend nor list price.
      const billed = segments.reduce((sum, s) => sum + s.billed, 0);
      return { row, total, billed, segments };
    });

    return {
      rows,
      cols,
      colour,
      names,
      widest: Math.max(...rows.map((r) => r.total), 1),
      hidden: hidden.length,
      hiddenTokens: hidden.reduce((sum, [, total]) => sum + total, 0),
    };
  }, [cells, colOrder, max]);

  if (!model) return <p className="empty">No data in this range.</p>;

  const name = (key: string) => model.names.get(key) ?? key;
  const describe = (row: string, s: Segment, total: number) =>
    `${row} · ${name(s.key)} — ${tokens(s.tokens)} tokens (${share(s.tokens, total)}) · ` +
    `${usd(s.billed)} billed, ${usd(s.rateCard)} rate card` +
    (s.unknownBasis > 0 ? `, ${usd(s.unknownBasis)} basis unknown (list prices)` : "");

  return (
    <>
      <Legend items={model.cols.map((k) => ({ color: model.colour.get(k)!, label: name(k) }))} />
      <ul className="matrix" aria-label="Tokens by model and reasoning effort">
        {model.rows.map((r) => (
          <li className="mrow" key={r.row}>
            <div className="head">
              <span className="name" title={r.row}>
                {r.row}
              </span>
              <span className="val">
                {tokens(r.total)}
                <span className="cost">{usd(r.billed)} billed</span>
              </span>
            </div>
            {/* Relative to the widest row, so bars compare across models too.
                Hidden from assistive tech: the summary below says the same. */}
            <div
              className="mtrack"
              aria-hidden="true"
              style={{ width: `${Math.max(3, (r.total / model.widest) * 100)}%` }}
            >
              {r.segments.map((s) => (
                <span
                  key={s.key}
                  className="mseg"
                  style={{
                    width: `${r.total > 0 ? (s.tokens / r.total) * 100 : 0}%`,
                    background: model.colour.get(s.key),
                  }}
                  title={describe(r.row, s, r.total)}
                />
              ))}
            </div>
            {/* The pairing as text, so it is readable without a mouse. */}
            <p className="msum">
              {r.segments.map((s, i) => (
                <Fragment key={s.key}>
                  {i > 0 && " · "}
                  <span className="mitem" title={describe(r.row, s, r.total)}>
                    <span className="dot" style={{ background: model.colour.get(s.key) }} />
                    {name(s.key)} {share(s.tokens, r.total)}
                  </span>
                </Fragment>
              ))}
            </p>
          </li>
        ))}
      </ul>
      {model.hidden > 0 && (
        <More onMore={onMore}>
          +{model.hidden} more, {tokens(model.hiddenTokens)} combined
        </More>
      )}
    </>
  );
}

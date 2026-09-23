import { memo, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { FocusEvent, KeyboardEvent, MouseEvent } from "react";
import type { HeatCell } from "@/api";
import { exact, spansYears, tokens, utcOffset } from "@/format";
import type { DayRange } from "@/series";
import { buildHeatModel } from "@/heatmap";
import type { Row } from "@/heatmap";

const DAY_NAMES = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const HOURS = Array.from({ length: 24 }, (_, h) => h);
/** Rows a PageUp or PageDown moves: a week. */
const PAGE = 7;

// Sequential encoding, one hue light to dark: an empty step plus six filled.
const STEPS = [
  "var(--seq-0)",
  "var(--seq-1)",
  "var(--seq-2)",
  "var(--seq-3)",
  "var(--seq-4)",
  "var(--seq-5)",
  "var(--seq-6)",
];

type Box = { left: number; top: number; width: number; height: number };
type Hover = {
  day: string;
  hour: number;
  /** The cell the tooltip points at. */
  anchor: HTMLElement;
  /** The marks drawn over the grid: the cell, its row and its hour's column. */
  cell: Box;
  row: Box;
  column: Box;
};

const hh = (h: number) => String(h).padStart(2, "0");

/**
 * Whose clock the hours are on. A server left on UTC (a container's default)
 * shifts every hour by the team's offset, so a mismatch is said outright.
 */
function zoneOf(server: number, local?: number): string {
  if (server === local) return `Hours are your local time (${utcOffset(server)})`;
  if (local === undefined) return `Hours are the server's time (${utcOffset(server)})`;
  return `Hours are the server's time (${utcOffset(server)}), not yours (${utcOffset(local)})`;
}

/** "Mon 22 Sep", with the year when the range crosses one. */
function rowLabel(day: string, weekday: number, withYear: boolean): string {
  const [y, m, d] = day.split("-");
  const base = `${DAY_NAMES[weekday]} ${Number(d)} ${MONTHS[Number(m) - 1]}`;
  return withYear ? `${base} ${y}` : base;
}

/** "22 Sep" or "22 Sep 2026". */
function dayLabel(day: string, withYear: boolean): string {
  const [y, m, d] = day.split("-");
  return `${Number(d)} ${MONTHS[Number(m) - 1]}${withYear ? ` ${y}` : ""}`;
}

/** The cell an event came from, read off the DOM: handlers are delegated to the grid. */
function cellOf(target: EventTarget): { el: HTMLElement; r: number; h: number } | null {
  if (!(target instanceof HTMLElement) || target.dataset.h === undefined) return null;
  return { el: target, r: Number(target.dataset.r), h: Number(target.dataset.h) };
}

/**
 * One day's row. Memoised, and marked for hover by overlays rather than by
 * props, so moving the pointer re-renders no cell, and moving keyboard focus
 * only the two rows whose tab stop changed.
 */
const HeatRow = memo(function HeatRow({
  row,
  r,
  label,
  tabHour,
  step,
  maxTotal,
}: {
  row: Row;
  r: number;
  label: string;
  /** The hour holding the grid's single tab stop, or -1. */
  tabHour: number;
  step: (v: number) => number;
  maxTotal: number;
}) {
  return (
    <div
      className={`hrow${row.total === 0 ? " idle" : ""}${row.outside ? " outside" : ""}`}
      role="row"
    >
      <span className="rowlabel" role="rowheader">
        {label}
      </span>
      {HOURS.map((h) => {
        const v = row.hours.get(h)?.tokens ?? 0;
        return (
          <span
            key={h}
            role="gridcell"
            className="cell"
            data-r={r}
            data-h={h}
            tabIndex={h === tabHour ? 0 : -1}
            aria-label={`${label} ${hh(h)}:00 — ${v ? `${tokens(v)} tokens` : "no activity"}`}
            style={{ background: STEPS[step(v)] }}
          />
        );
      })}
      <span className="rowtotal" role="gridcell">
        <span className="rowbar" style={{ width: `${(row.total / maxTotal) * 100}%` }} />
        <span className="rowtext">{row.total ? tokens(row.total) : "—"}</span>
      </span>
    </div>
  );
});

/**
 * Activity by date and hour. Rows are real dates, not an aggregate over every
 * Monday: "what ran at 03:00 on the 21st" is the question that leads somewhere.
 *
 * Shading is by quantile, not linear: one runaway hour would otherwise
 * saturate its cell and flatten the rest. The legend prints the boundaries.
 *
 * A grid with a single tab stop: arrow keys, Home/End and PageUp/PageDown move
 * between cells, and focus shows the same tooltip as hover.
 */
export function Heatmap({
  cells,
  range,
  detailFrom,
  serverOffset,
  localOffset,
}: {
  cells: HeatCell[];
  range?: DayRange;
  /** First UTC day with per-event detail; days before it hold daily rollups only. */
  detailFrom?: string;
  /** The zone the server bucketed hours in, in minutes east of UTC. */
  serverOffset: number;
  /** The viewer's own offset, to say whether those hours are theirs. */
  localOffset?: number;
}) {
  const [hover, setHover] = useState<Hover | null>(null);
  const [tab, setTab] = useState<{ day: string; hour: number } | null>(null);
  // Strings, not the object: a caller rebuilding `range` each render must not rebuild the grid.
  const from = range?.from ?? "";
  const to = range?.to ?? "";
  const withYear = spansYears(from, to);
  const hintId = useId();
  const wrapRef = useRef<HTMLDivElement>(null);
  const gridRef = useRef<HTMLDivElement>(null);
  const bodyRef = useRef<HTMLDivElement>(null);
  const tipRef = useRef<HTMLDivElement>(null);

  const model = useMemo(
    () => buildHeatModel(cells, from, to, detailFrom, STEPS.length),
    [cells, from, to, detailFrom],
  );

  // The tab stop survives a refresh by day; a day that left the range hands
  // it back to the first cell.
  const found = model && tab ? model.rows.findIndex((r) => r.day === tab.day) : -1;
  const tabRow = found === -1 ? 0 : found;
  const tabHour = found === -1 || !tab ? 0 : tab.hour;

  const rowEls = useMemo(
    () =>
      model?.rows.map((row, r) => (
        <HeatRow
          key={row.day}
          row={row}
          r={r}
          label={rowLabel(row.day, row.weekday, withYear)}
          tabHour={r === tabRow ? tabHour : -1}
          step={model.step}
          maxTotal={model.maxTotal}
        />
      )),
    [model, withYear, tabRow, tabHour],
  );

  // Fixed to the viewport rather than placed inside the grid: the grid sits in
  // scroll containers that would clip it, as they did below a short range.
  // Above the cell unless the window has no room there, clamped to the
  // window's width, and placed again whenever anything scrolls.
  useLayoutEffect(() => {
    const tip = tipRef.current;
    if (!tip || !hover) return undefined;
    const place = () => {
      const cell = hover.anchor.getBoundingClientRect();
      const gap = 8;
      const below = cell.top - gap - tip.offsetHeight < 4;
      const centre = cell.left + cell.width / 2;
      const left = Math.min(
        Math.max(centre - tip.offsetWidth / 2, 4),
        window.innerWidth - tip.offsetWidth - 4,
      );
      tip.style.left = `${left}px`;
      tip.style.top = `${below ? cell.bottom + gap : cell.top - gap - tip.offsetHeight}px`;
      tip.style.setProperty("--arrow-x", `${centre - left}px`);
      tip.dataset.side = below ? "below" : "above";
    };
    place();
    window.addEventListener("scroll", place, true);
    window.addEventListener("resize", place);
    return () => {
      window.removeEventListener("scroll", place, true);
      window.removeEventListener("resize", place);
    };
  }, [hover]);

  if (!model) {
    if (detailFrom && from && from < detailFrom) {
      return (
        <p className="empty">
          {to < detailFrom
            ? `Hourly detail is not kept before ${detailFrom}, and this range ends before it.`
            : `No activity since ${detailFrom}; hourly detail before it is not kept.`}
        </p>
      );
    }
    return <p className="empty">No activity in this range.</p>;
  }

  // Anchored to the cell, not the pointer, so the tooltip holds still over
  // 14px cells. Hover and keyboard focus share this one state, so whichever
  // came last is the one marked.
  const show = (el: HTMLElement, day: string, hour: number) => {
    const wrap = wrapRef.current;
    const body = bodyRef.current;
    const first = el.parentElement?.querySelector('[data-h="0"]');
    const last = el.parentElement?.querySelector('[data-h="23"]');
    if (!wrap || !body || !first || !last) return;
    const box = wrap.getBoundingClientRect();
    const at = (r: DOMRect): Box => ({
      left: r.left - box.left,
      top: r.top - box.top,
      width: r.width,
      height: r.height,
    });
    const cell = at(el.getBoundingClientRect());
    const view = at(body.getBoundingClientRect());
    const start = at(first.getBoundingClientRect());
    setHover({
      day,
      hour,
      anchor: el,
      cell,
      row: {
        ...cell,
        left: start.left,
        width: last.getBoundingClientRect().right - box.left - start.left,
      },
      column: { ...cell, top: view.top, height: view.height },
    });
  };

  const onMouseOver = (e: MouseEvent<HTMLDivElement>) => {
    const at = cellOf(e.target);
    if (at) show(at.el, model.rows[at.r].day, at.h);
  };
  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const at = cellOf(e.target);
    if (!at) return;
    const day = model.rows[at.r].day;
    setTab({ day, hour: at.h });
    show(at.el, day, at.h);
  };
  // The pointer leaving hands the marks back to a focused cell, if any.
  const onMouseLeave = () => {
    const at = document.activeElement ? cellOf(document.activeElement) : null;
    if (at && gridRef.current?.contains(at.el)) show(at.el, model.rows[at.r].day, at.h);
    else setHover(null);
  };
  // Focus leaving the grid takes the tooltip with it.
  const onBlur = (e: FocusEvent<HTMLDivElement>) => {
    if (!(e.relatedTarget instanceof Node && e.currentTarget.contains(e.relatedTarget))) {
      setHover(null);
    }
  };
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const at = cellOf(e.target);
    if (!at) return;
    const last = model.rows.length - 1;
    let { r, h } = at;
    switch (e.key) {
      case "ArrowRight":
        h = Math.min(23, h + 1);
        break;
      case "ArrowLeft":
        h = Math.max(0, h - 1);
        break;
      case "ArrowDown":
        r = Math.min(last, r + 1);
        break;
      case "ArrowUp":
        r = Math.max(0, r - 1);
        break;
      case "PageDown":
        r = Math.min(last, r + PAGE);
        break;
      case "PageUp":
        r = Math.max(0, r - PAGE);
        break;
      case "Home":
        h = 0;
        if (e.ctrlKey || e.metaKey) r = 0;
        break;
      case "End":
        h = 23;
        if (e.ctrlKey || e.metaKey) r = last;
        break;
      default:
        return;
    }
    e.preventDefault();
    gridRef.current?.querySelector<HTMLElement>(`[data-r="${r}"][data-h="${h}"]`)?.focus();
  };

  const hoveredRow = hover ? model.byIso.get(hover.day) : undefined;
  const hoveredCell = hover && hoveredRow ? hoveredRow.hours.get(hover.hour) : undefined;
  const dayShare =
    hoveredRow && hoveredRow.total > 0 ? ((hoveredCell?.tokens ?? 0) / hoveredRow.total) * 100 : 0;
  const { pruned } = model;

  return (
    <>
      <p className="heathint" id={hintId}>
        {[
          `${zoneOf(serverOffset, localOffset)}; the other cards count UTC days, so the first and last rows can be partial.`,
          model.behind &&
            "The server is behind UTC, so the bottom row is the evening before the range starts.",
          model.ahead &&
            "The server is ahead of UTC, so the top row is the morning after the range ends.",
          "Hover or focus a cell for details; arrow keys move between cells.",
        ]
          .filter(Boolean)
          .join(" ")}
      </p>

      <div className="heat2" ref={wrapRef} onMouseLeave={onMouseLeave}>
        <div
          role="grid"
          aria-label="Tokens by day and hour"
          aria-describedby={hintId}
          ref={gridRef}
          onMouseOver={onMouseOver}
          onFocus={onFocus}
          onBlur={onBlur}
          onKeyDown={onKeyDown}
        >
          <div role="rowgroup">
            <div className="hhead" role="row">
              <span role="columnheader" aria-label="Day" />
              {HOURS.map((h) => (
                <span className="collabel" key={h} role="columnheader" aria-label={`${hh(h)}:00`}>
                  {h % 3 === 0 ? hh(h) : ""}
                </span>
              ))}
              <span className="totlabel" role="columnheader">
                day total
              </span>
            </div>
          </div>
          <div className="hbody" role="rowgroup" ref={bodyRef}>
            {rowEls}
          </div>
        </div>

        {pruned.length > 0 && (
          <p className="heatpruned">
            {pruned.length === 1
              ? dayLabel(pruned[0], withYear)
              : `${dayLabel(pruned[0], withYear)} – ${dayLabel(pruned[pruned.length - 1], withYear)}`}
            : hourly detail not kept ({pruned.length} {pruned.length === 1 ? "day" : "days"}). The
            other cards still count these days.
          </p>
        )}

        {hover && hoveredRow && (
          <>
            <div className="hmark axis" aria-hidden="true" style={hover.row} />
            <div className="hmark axis" aria-hidden="true" style={hover.column} />
            <div className="hmark" aria-hidden="true" style={hover.cell} />
            <div ref={tipRef} className="heattip" role="tooltip">
              <div className="t-head">
                {rowLabel(hover.day, hoveredRow.weekday, withYear)}
                <span className="t-hour">
                  {hh(hover.hour)}:00–{hh((hover.hour + 1) % 24)}:00
                </span>
              </div>
              {hoveredCell ? (
                <>
                  <div className="t-row">
                    <span>Tokens</span>
                    <b>{tokens(hoveredCell.tokens)}</b>
                  </div>
                  <div className="t-row">
                    <span>Responses</span>
                    <b>{exact(hoveredCell.events)}</b>
                  </div>
                  <div className="t-row">
                    <span>Share of day</span>
                    <b>{dayShare.toFixed(1)}%</b>
                  </div>
                  <div className="t-row t-sep">
                    <span>Day total</span>
                    <b>{tokens(hoveredRow.total)}</b>
                  </div>
                </>
              ) : (
                <div className="t-row t-idle">
                  <span>{hoveredRow.total ? "No activity this hour" : "No activity this day"}</span>
                </div>
              )}
              {hoveredRow.outside && (
                <div className="t-row t-idle">
                  <span>Outside the selected dates, in server time</span>
                </div>
              )}
            </div>
          </>
        )}
      </div>

      <div className="heatfoot">
        <span className="cell" style={{ background: STEPS[0] }} />
        <span>none</span>
        {model.cuts.map((c, i) => (
          <span className="pill" key={c}>
            <span className="cell" style={{ background: STEPS[i + 1] }} />
            <span>≤{tokens(c)}</span>
          </span>
        ))}
        <span className="pill">
          <span className="cell" style={{ background: STEPS[model.cuts.length + 1] }} />
          <span>more</span>
        </span>
      </div>
    </>
  );
}

import { useCallback, useId, useRef, useSyncExternalStore, type ReactNode } from "react";
import { exact } from "@/format";
import { cardId, closeCard, openCard, useExpandedCard } from "@/expanded";
import type { Group } from "@/api";
import type { Change } from "@/dashboard";

/**
 * Stands in for a card or tile whose request failed. Its empty state would say
 * "no activity", a claim about the team that the page cannot make.
 */
export const FAILED = "Could not load — retrying.";

/** Where a card's content is drawn: in the grid, or expanded over the page. */
export type View = {
  expanded: boolean;
  /** A chart's height when expanded; in the grid each chart keeps its own. */
  chartHeight?: number;
  /** Opens the expanded view; does nothing once in it. */
  open: () => void;
  /** Closes the expanded view, for an action whose result is on the page. */
  close: () => void;
};

const noop = () => {};

export function Card({
  title,
  note,
  children,
  collapsed,
  failed,
}: {
  title: string;
  note?: string;
  /**
   * A function makes the card expandable: it draws the content for the grid,
   * and again with its limits lifted when the card is expanded.
   */
  children: ReactNode | ((view: View) => ReactNode);
  /**
   * Start folded away, for reference material nobody checks daily. A native
   * <details>: keyboard-operable, and no state of ours to synchronise.
   */
  collapsed?: boolean;
  /** The card's request failed: say so in place of its contents. */
  failed?: boolean;
}) {
  const id = cardId(title);
  const expanded = useExpandedCard() === id;
  const trigger = useRef<HTMLButtonElement>(null);
  const draw = typeof children === "function" ? children : undefined;
  const failure = <p className="empty failed">{FAILED}</p>;
  const open = () => openCard(id);
  const content =
    typeof children === "function" ? children({ expanded: false, open, close: noop }) : children;
  const body = failed ? failure : content;

  if (collapsed) {
    return (
      <details className="card">
        <summary>
          <h2>{title}</h2>
        </summary>
        {note && <p className="note">{note}</p>}
        {body}
      </details>
    );
  }
  return (
    <section className="card">
      <div className="card-head">
        <h2>{title}</h2>
        {draw && !failed && (
          <button
            type="button"
            ref={trigger}
            className="icon-button"
            aria-haspopup="dialog"
            aria-label={`Expand ${title}`}
            title="Expand"
            onClick={open}
          >
            <ExpandIcon />
          </button>
        )}
      </div>
      {note && <p className="note">{note}</p>}
      {body}
      {expanded && draw && (
        <ExpandedCard
          title={title}
          note={note}
          onClose={() => {
            closeCard(id);
            trigger.current?.focus();
          }}
        >
          {(chartHeight, close) =>
            // A card whose request fails while open says so here too, and
            // recovers with the next poll like the one in the grid.
            failed ? failure : draw({ expanded: true, chartHeight, open: noop, close })
          }
        </ExpandedCard>
      )}
    </section>
  );
}

const subscribeResize = (onChange: () => void) => {
  window.addEventListener("resize", onChange);
  return () => window.removeEventListener("resize", onChange);
};

/**
 * A card over the whole page, in a native modal <dialog>: the browser keeps
 * focus inside it, makes the page behind inert and closes it on Esc. Mounted
 * only while open, so the grid never renders a card twice.
 */
function ExpandedCard({
  title,
  note,
  onClose,
  children,
}: {
  title: string;
  note?: string;
  onClose: () => void;
  children: (chartHeight: number, close: () => void) => ReactNode;
}) {
  const titleId = useId();
  const viewport = useSyncExternalStore(subscribeResize, () => window.innerHeight);
  // Opened as it attaches, before the charts inside measure themselves: in a
  // dialog that is still closed they measure zero width and draw nothing
  // until a resize is reported, which a browser may never do.
  // Closing because the card unmounted (the page failing, say) is not the
  // user closing it: the URL still names it, and it reopens when it returns.
  const leaving = useRef(false);
  const attach = useCallback((el: HTMLDialogElement) => {
    leaving.current = false;
    el.showModal();
    return () => {
      leaving.current = true;
      el.close();
    };
  }, []);
  const pressed = useRef<EventTarget | null>(null);
  return (
    // Every way out calls onClose, which takes the card out of the URL and so
    // unmounts this. None waits for the dialog's close event: Chrome can close
    // a dialog on Esc without firing it, leaving the URL naming a card that is
    // no longer shown and cannot be reopened.
    //
    // The panel fills the dialog, so the dialog itself is only ever hit on the
    // backdrop around it. Both the press and the click must land there: a text
    // selection dragged out of the panel also ends in a click on the dialog.
    <dialog
      ref={attach}
      className="expanded"
      aria-labelledby={titleId}
      onCancel={(e) => {
        // Esc. Kept open where the browser allows, so the URL change closes it.
        e.preventDefault();
        onClose();
      }}
      // Anything else that closes it. Queued, so one can arrive after the
      // dialog has opened again: StrictMode attaches the ref twice.
      onClose={(e) => {
        if (!leaving.current && !e.currentTarget.open) onClose();
      }}
      onPointerDown={(e) => {
        pressed.current = e.target;
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget && pressed.current === e.currentTarget) onClose();
      }}
    >
      <div className="panel">
        <div className="card-head">
          <h2 id={titleId}>{title}</h2>
          <button
            type="button"
            className="icon-button"
            aria-label="Close"
            title="Close (Esc)"
            onClick={onClose}
          >
            <CloseIcon />
          </button>
        </div>
        {note && <p className="note">{note}</p>}
        {/* The header, note and legend take about 240px of the viewport. */}
        {children(Math.max(240, viewport - 240), onClose)}
      </div>
    </dialog>
  );
}

function ExpandIcon() {
  return (
    <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
      <path
        d="M9.5 2.5h4v4M6.5 13.5h-4v-4M13.5 2.5 9 7M2.5 13.5 7 9"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function CloseIcon() {
  return (
    <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
      <path
        d="M4 4l8 8M12 4l-8 8"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
      />
    </svg>
  );
}

/**
 * The line under a truncated list. With `onMore` it is a button that expands
 * the card: the line that says there is more is where people look for it.
 */
export function More({ onMore, children }: { onMore?: () => void; children: ReactNode }) {
  if (!onMore) return <p className="more">{children}</p>;
  return (
    <button type="button" className="more" onClick={onMore}>
      {children}
    </button>
  );
}

export function Tile({
  label,
  value,
  hint,
  delta,
  failed,
}: {
  label: string;
  value: string;
  hint?: string;
  delta?: Change | null;
  failed?: boolean;
}) {
  return (
    <div className="card tile">
      <div className="label">{label}</div>
      <div className="value">{failed ? "—" : value}</div>
      {delta && !failed && <Delta {...delta} />}
      {(failed || hint) && <div className="hint">{failed ? FAILED : hint}</div>}
    </div>
  );
}

/**
 * Deliberately uncoloured: up is not bad and down is not good, so red and
 * green would assert a judgement the data does not support.
 */
function Delta(c: Change) {
  if ("note" in c) {
    return (
      <div className="delta muted" title={`the prior period is ${c.window}`}>
        {c.note}
      </div>
    );
  }
  const rounded = Math.abs(c.pct) < 0.5 ? 0 : c.pct;
  const arrow = rounded > 0 ? "↑" : rounded < 0 ? "↓" : "→";
  return (
    <div className="delta" title={`compared with ${c.window}`}>
      <span className="arrow">{arrow}</span>
      {rounded === 0 ? "flat" : `${Math.abs(rounded).toFixed(0)}%`}
      <span className="vs">vs prior period</span>
    </div>
  );
}

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

import { useCallback, useId, useRef, useSyncExternalStore, type ReactNode } from "react";
import { cardId, closeCard, openCard, useExpandedCard } from "@/expanded";

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

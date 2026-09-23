import type { Change } from "@/dashboard";
import { FAILED } from "@/components/Card";

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

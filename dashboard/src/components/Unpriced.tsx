/**
 * Stands in for a cost of $0 on usage the price table has no entry for:
 * counted, deliberately not costed, and never passed off as free
 * (invariant 8).
 */
export function Unpriced({ label = "unpriced" }: { label?: string }) {
  return (
    <span
      className="tag unpriced"
      title="A model with no entry in the price table: its tokens are counted but not costed."
    >
      {label}
    </span>
  );
}

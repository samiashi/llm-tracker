import type { Totals } from "@/api";
import { tokens } from "@/format";
import { TOKEN_KIND } from "@/palette";

/**
 * Where the tokens go, as one proportional bar: how much of the total is cache
 * reads is hard to believe from a number alone.
 */
export function Composition({ totals }: { totals: Totals }) {
  const parts = [
    {
      key: "cache_read",
      label: "Cache read",
      value: totals.cache_read_tokens,
      color: TOKEN_KIND.cacheRead,
    },
    { key: "input", label: "Input", value: totals.input_tokens, color: TOKEN_KIND.input },
    {
      key: "cache_write",
      label: "Cache write",
      value: totals.cache_write_tokens,
      color: TOKEN_KIND.cacheWrite,
    },
    { key: "output", label: "Output", value: totals.output_tokens, color: TOKEN_KIND.output },
  ].filter((p) => p.value > 0);

  const total = parts.reduce((s, p) => s + p.value, 0);
  if (total === 0) return <p className="empty">No tokens in this range.</p>;

  return (
    <>
      <div className="comp-bar">
        {parts.map((p) => (
          <span
            key={p.key}
            style={{ width: `${(p.value / total) * 100}%`, background: p.color }}
            title={`${p.label} — ${tokens(p.value)} (${((p.value / total) * 100).toFixed(1)}%)`}
          />
        ))}
      </div>
      <ul className="comp-list">
        {parts.map((p) => {
          const pct = (p.value / total) * 100;
          return (
            <li key={p.key}>
              <span className="dot" style={{ background: p.color }} />
              <span className="label">{p.label}</span>
              {/* Segments under about 1% vanish from the bar, so the figures carry them. */}
              <span className="pct">{pct < 0.1 ? "<0.1%" : `${pct.toFixed(1)}%`}</span>
              <span className="abs">{tokens(p.value)}</span>
            </li>
          );
        })}
      </ul>
      <p className="comp-note">
        Cache reads cost about a tenth of the input price and cache writes 1.25–2×, so a large cache
        share means efficient, not expensive.
      </p>
    </>
  );
}

/**
 * Loading placeholders shaped like the real layout, so the page does not jump
 * when data lands and a slow first load reads as loading, not as breakage.
 */
export function Skeleton() {
  return (
    <div className="wrap" aria-busy="true" aria-live="polite">
      <header className="top">
        <h1>Token usage</h1>
        <span className="sub">loading…</span>
      </header>

      <div className="grid tiles" style={{ marginTop: 22, marginBottom: 14 }}>
        {/* Five, matching the real tile row, so the layout does not shift on load. */}
        {[0, 1, 2, 3, 4].map((i) => (
          <div className="card tile" key={i}>
            <div className="sk line" style={{ width: "42%" }} />
            <div className="sk value" />
            <div className="sk line" style={{ width: "72%" }} />
          </div>
        ))}
      </div>

      <div className="grid two" style={{ marginBottom: 14 }}>
        {[0, 1].map((i) => (
          <section className="card" key={i}>
            <div className="sk line" style={{ width: "34%", height: 13 }} />
            <div className="sk line" style={{ width: "88%" }} />
            <div className="sk chart" />
          </section>
        ))}
      </div>

      <div className="grid two">
        {[0, 1].map((i) => (
          <section className="card" key={i}>
            <div className="sk line" style={{ width: "28%", height: 13 }} />
            <div style={{ marginTop: 14 }}>
              {[70, 52, 38, 24, 16].map((w) => (
                <div key={w} style={{ marginBottom: 12 }}>
                  <div className="sk line" style={{ width: `${w}%`, margin: "0 0 6px" }} />
                  <div className="sk bar" style={{ width: `${w}%` }} />
                </div>
              ))}
            </div>
          </section>
        ))}
      </div>
    </div>
  );
}

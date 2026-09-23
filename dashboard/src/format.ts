// Every formatter here is total: server JSON is cast, never validated, and a
// throw during render blanks the whole page. A dash is the failure mode.

/** Compact token counts; totals run to billions, so full digits are noise. */
export function tokens(n: number): string {
  if (!Number.isFinite(n)) return "—";
  const sign = n < 0 ? "-" : "";
  const v = Math.abs(n);

  const units: [number, string][] = [
    [1e12, "T"],
    [1e9, "B"],
    [1e6, "M"],
    [1e3, "K"],
  ];
  for (let i = 0; i < units.length; i++) {
    const [scale, suffix] = units[i];
    if (v < scale) continue;
    const scaled = v / scale;
    const digits = scaled >= 10 ? 0 : 1;
    // Rounding can carry past the boundary that chose the unit: 999,999 would
    // print "1000K". Promote to the next unit instead.
    if (Number(scaled.toFixed(digits)) >= 1000 && i > 0) {
      const [bigger, biggerSuffix] = units[i - 1];
      return `${sign}${(v / bigger).toFixed(1)}${biggerSuffix}`;
    }
    return `${sign}${scaled.toFixed(digits)}${suffix}`;
  }
  return `${sign}${Math.round(v)}`;
}

export function usd(n: number): string {
  if (!Number.isFinite(n)) return "—";
  if (n === 0) return "$0";
  // The magnitude, so a small negative stays negative.
  if (Math.abs(n) < 0.01) return n < 0 ? "-<$0.01" : "<$0.01";
  return n.toLocaleString("en-US", {
    style: "currency",
    currency: "USD",
    maximumFractionDigits: n >= 100 ? 0 : 2,
  });
}

export function exact(n: number): string {
  if (!Number.isFinite(n)) return "—";
  return n.toLocaleString("en-US");
}

export function bytes(n: number): string {
  if (!Number.isFinite(n)) return "—";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(i === 0 ? 0 : 1)}${u[i]}`;
}

/** How long since a time in **milliseconds**. `now` is a parameter so render stays pure. */
export function since(ms: number, now: number = Date.now()): string {
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  return `${Math.round(s / 3600)}h ago`;
}

/**
 * How long since a time in unix **seconds**; seconds passed to `since` read as
 * decades. Pass the server's clock as `now`, so a skewed browser cannot age
 * every row. A time ahead of `now` says so rather than passing for fresh.
 */
export function ago(unix: number, now: number = Date.now() / 1000): string {
  if (!Number.isFinite(unix) || !unix) return "never";
  const s = now - unix;
  if (s < -60) return "clock ahead";
  if (s < 3600) return `${Math.max(0, Math.round(s / 60))}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

/**
 * Whether a day axis over this range needs years in its labels. A categorical
 * axis cannot tell 09/22 of one year from 09/22 of another, so over a range
 * crossing a year boundary both would sit at one position under one label.
 */
export function spansYears(from?: string, to?: string): boolean {
  return !!from && !!to && from.slice(0, 4) !== to.slice(0, 4);
}

/** A short axis label for an ISO day; `withYear` comes from `spansYears`. */
export function shortDay(day: string, withYear = false): string {
  if (typeof day !== "string") return "—";
  const [y, m, d] = day.split("-");
  if (!y || !m || !d) return "—";
  return withYear ? `${m}/${d}/${y.slice(2)}` : `${m}/${d}`;
}

/** An offset in minutes east of UTC, as "UTC+4", "UTC+5:30", "UTC−3" or "UTC". */
export function utcOffset(minutes: number): string {
  if (minutes === 0) return "UTC";
  const abs = Math.abs(minutes);
  const m = abs % 60;
  return `UTC${minutes > 0 ? "+" : "−"}${Math.floor(abs / 60)}${m ? `:${String(m).padStart(2, "0")}` : ""}`;
}

/** Where a UTC day begins on a clock `minutes` east of UTC: "04:00" at UTC+4. */
export function utcMidnightAt(minutes: number): string {
  const m = ((minutes % 1440) + 1440) % 1440;
  return `${String(Math.floor(m / 60)).padStart(2, "0")}:${String(m % 60).padStart(2, "0")}`;
}

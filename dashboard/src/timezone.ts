/** The viewer's browser/OS setting, without location access or a network lookup. */
export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

const formatters = new Map<string, Intl.DateTimeFormat | null>();
function formatter(zone: string): Intl.DateTimeFormat | null {
  if (formatters.has(zone)) return formatters.get(zone)!;
  let result: Intl.DateTimeFormat;
  try {
    result = new Intl.DateTimeFormat("en-GB", {
      timeZone: zone,
      month: "2-digit",
      day: "2-digit",
      year: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
      timeZoneName: "longOffset",
    });
  } catch {
    const fallback = zone === "UTC" ? null : formatter("UTC");
    formatters.set(zone, fallback);
    return fallback;
  }
  formatters.set(zone, result);
  return result;
}

function parts(at: number, zone: string): Record<string, string> {
  const fmt = formatter(zone);
  if (fmt) return Object.fromEntries(fmt.formatToParts(at).map((p) => [p.type, p.value]));
  const iso = new Date(at).toISOString();
  return {
    year: iso.slice(0, 4),
    month: iso.slice(5, 7),
    day: iso.slice(8, 10),
    hour: iso.slice(11, 13),
    minute: iso.slice(14, 16),
    timeZoneName: "GMT",
  };
}

/** Offset at the actual instant, never today's offset applied to old events. */
export function offsetSeconds(at: number, zone: string): number {
  const name = parts(at, zone).timeZoneName;
  const match = /^GMT([+-])(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(name);
  if (!match) return 0;
  return (match[1] === "+" ? 1 : -1) * (+match[2] * 3600 + +match[3] * 60 + +(match[4] || 0));
}

/** Numeric offsets distinguish repeated clock hours during a fall-back. */
export function localTimestamp(iso: string, zone: string, withYear = false): string {
  const at = Date.parse(iso);
  if (!Number.isFinite(at)) return "—";
  const p = parts(at, zone);
  const offset = p.timeZoneName.replace("GMT", "UTC").replace("UTC+00:00", "UTC");
  return `${withYear ? `${p.year}-` : ""}${p.month}-${p.day} ${p.hour}:${p.minute} ${offset}`;
}

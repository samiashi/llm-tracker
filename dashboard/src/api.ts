import { MODEL_SLOTS } from "@/palette";

/**
 * Billed, rate-card and unknown-basis spend are three kinds of number and are
 * never summed (invariant 3).
 */
export type Totals = {
  events: number;
  total_tokens: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  billed_usd: number;
  /** Priced tokens billed as metered usage: the denominator of an effective rate. */
  billed_tokens: number;
  rate_card_usd: number;
  /** Priced at list rates, but whether it was metered or a seat is unknown. */
  unknown_basis_usd: number;
  unpriced_tokens: number;
};

export type Group = { key: string; totals: Totals };
export type Daily = { day: string; totals: Totals };

/**
 * `history_*_day` are UTC days, like every day the server returns. Under a
 * person filter, `history_first_day` is that person's first recorded day.
 */
export type Summary = {
  totals: Totals;
  history_first_day: string;
  history_last_day: string;
  /** The server's version as a release, "X.Y.Z", or "" for a build that is not one. */
  server_release: string;
};

export type SourceHealth = {
  source: string;
  machine_id: string;
  last_event: number;
  events: number;
};

/** One hour of activity, bucketed in the server's time zone rather than UTC. */
export type HeatCell = {
  day: string;
  weekday: number;
  hour: number;
  tokens: number;
  events: number;
};

type HeatmapResult = {
  cells: HeatCell[];
  /** First UTC day with per-event detail; absent or empty when nothing was pruned. */
  detail_from?: string;
  /** The zone the hours are in, in minutes east of UTC. */
  utc_offset_minutes: number;
};

export type SessionRow = {
  session_id: string;
  source: string;
  model: string;
  effort: string;
  email: string;
  tokens: number;
  billed_usd: number;
  rate_card_usd: number;
  unknown_basis_usd: number;
  /** Tokens on a model with no price, which none of the three figures include. */
  unpriced_tokens: number;
  events: number;
  last_seen: number;
};

export type ModelDay = { day: string; model: string; tokens: number };

/** `col_order` is present for an ordinal dimension such as effort, where the order is the meaning. */
type MatrixResult = { cells: MatrixCell[]; col_order?: string[] };

export type MatrixCell = {
  row: string;
  col: string;
  tokens: number;
  billed_usd: number;
  rate_card_usd: number;
  unknown_basis_usd: number;
  /** Tokens on a model with no price, which none of the three figures include. */
  unpriced_tokens: number;
};

/** Why a detected harness has no adapter. Widened for forward compatibility. */
type UnknownStatus = "todo" | "blocked" | (string & {});

export type UnknownRow = {
  machine_id: string;
  path: string;
  hint: string;
  size_bytes: number;
  last_seen: number;
  /** "todo": readable, nobody has written it. "blocked": investigated, cannot be done. */
  status: UnknownStatus;
  note: string;
};

/** The range every view shares: two UTC days, inclusive. */
export type Filter = { from: string; to: string; person?: string };

/** The prior window of equal length; the current one is the summary's. */
export type Compare = {
  previous: Totals;
  previous_from: string;
  previous_to: string;
};

/**
 * One machine running the collector. `last_sync` is a heartbeat, not activity:
 * the agent pushes every interval with or without data. There is deliberately
 * no "last active" field; this is a usage tracker, not an attendance one.
 */
export type AgentRow = {
  machine_id: string;
  hostname: string;
  person: string;
  agent_version: string;
  /** agent_version as a release, "X.Y.Z", or "" for a build that is not one. */
  release: string;
  last_sync: number;
  events: number;
};

/** The models the matrix asks for, busiest first: the server sends no more than this. */
export const MATRIX_ROWS = 12;

/**
 * Generous, because "All" over a large archive is legitimately slow; bounded,
 * because a request that never answers would hold the page on its skeleton.
 */
const REQUEST_TIMEOUT_MS = 30_000;

/** The server answered, with an error status: not an outage, and not the same failure as one. */
export class HttpError extends Error {
  readonly status: number;
  /** The server's own explanation, from its `{"error": ...}` body. */
  readonly detail: string;

  constructor(path: string, status: number, detail: string) {
    super(`${path}: ${status}${detail ? `: ${detail}` : ""}`);
    this.name = "HttpError";
    this.status = status;
    this.detail = detail;
  }
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  // The caller's signal cancels a superseded filter's requests; the timeout
  // bounds one that never answers. Whichever fires first wins.
  const timeout = AbortSignal.timeout(REQUEST_TIMEOUT_MS);
  const r = await fetch(path, {
    signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
  });
  if (!r.ok) {
    let detail = "";
    try {
      const body = (await r.json()) as { error?: string };
      if (typeof body?.error === "string") detail = body.error;
    } catch {
      // Not JSON; the status alone will have to do.
    }
    throw new HttpError(path, r.status, detail);
  }
  return r.json() as Promise<T>;
}

/**
 * What a failed load means, for the reader: no answer at all, a range the
 * server rejected, or a server that answered with an error. Each sends the
 * reader somewhere different, so they are never reported as one another.
 */
export function describeFailure(e: unknown): { title: string; detail: string } {
  if (e instanceof HttpError) {
    if (e.status === 400) {
      return { title: "That date range isn't valid.", detail: e.detail || "Choose another." };
    }
    // A gateway answering for a server it could not reach.
    if (e.status === 502 || e.status === 504) {
      return { title: "Cannot reach the server.", detail: `A proxy answered ${e.status}.` };
    }
    const detail = e.detail ? `${e.status}: ${e.detail}` : String(e.status);
    return {
      title: e.status >= 500 ? "The server hit an error." : "The server refused the request.",
      detail,
    };
  }
  if (typeof e === "object" && e !== null && (e as { name?: unknown }).name === "TimeoutError") {
    return {
      title: "The server did not answer.",
      detail: `Nothing came back within ${REQUEST_TIMEOUT_MS / 1000} seconds.`,
    };
  }
  // fetch rejects with a TypeError when no response arrives at all.
  if (e instanceof TypeError) return { title: "Cannot reach the server.", detail: e.message };
  return { title: "Could not load the dashboard.", detail: String(e) };
}

function qs(f: Filter, extra: Record<string, string | number> = {}): string {
  const p = new URLSearchParams({ from: f.from, to: f.to });
  if (f.person) p.set("person", f.person);
  for (const [k, v] of Object.entries(extra)) p.set(k, String(v));
  return p.toString();
}

export const api = {
  summary: (f: Filter, s?: AbortSignal) => get<Summary>(`/v1/summary?${qs(f)}`, s),
  daily: (f: Filter, s?: AbortSignal) => get<{ days: Daily[] }>(`/v1/daily?${qs(f)}`, s),
  breakdown: (by: string, f: Filter, s?: AbortSignal) =>
    get<{ groups: Group[] }>(`/v1/breakdown?${qs(f, { by })}`, s),
  heatmap: (f: Filter, s?: AbortSignal) => get<HeatmapResult>(`/v1/heatmap?${qs(f)}`, s),
  // Sessions and the matrix fetch more rows than their cards show, so an
  // expanded card has them without a second request: the grid shows the first.
  topSessions: (f: Filter, s?: AbortSignal) =>
    get<{ sessions: SessionRow[] }>(`/v1/sessions/top?${qs(f, { limit: 50 })}`, s),
  dailyByModel: (f: Filter, s?: AbortSignal) =>
    get<{ points: ModelDay[] }>(`/v1/daily/model?${qs(f, { top: MODEL_SLOTS.length })}`, s),
  health: (s?: AbortSignal) => get<{ sources: SourceHealth[] }>(`/v1/health/sources`, s),
  agents: (s?: AbortSignal) =>
    get<{ agents: AgentRow[]; now: number; server_version: string }>(`/v1/agents`, s),
  unknown: (s?: AbortSignal) => get<{ unknown: UnknownRow[] }>(`/v1/unknown`, s),
  compare: (f: Filter, s?: AbortSignal) => get<Compare>(`/v1/compare?${qs(f)}`, s),
  matrix: (f: Filter, rows: string, cols: string, s?: AbortSignal) =>
    get<MatrixResult>(`/v1/matrix?${qs(f, { rows, cols, limit: MATRIX_ROWS })}`, s),
  /** CSV download URL for the current filter, used as an href rather than fetched. */
  exportURL: (f: Filter) => `/v1/export.csv?${qs(f)}`,
};

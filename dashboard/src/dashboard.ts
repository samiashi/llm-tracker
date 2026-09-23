import { api, UnauthorizedError } from "@/api";
import type { Version } from "@/fleet";
import type {
  AgentRow,
  Compare,
  Daily,
  Filter,
  Group,
  HeatCell,
  MatrixCell,
  ModelDay,
  SessionRow,
  SourceHealth,
  Summary,
  UnknownRow,
} from "@/api";

/** A change against the prior window, or a note on why there is no honest percentage. */
export type Change = { window: string } & ({ pct: number } | { note: string });

/**
 * Change against the prior window of equal length, or null when there is none
 * to show. A prior window that starts before the recorded history is only
 * partly measured, and its percentage would report the install date as growth.
 */
export function change(
  current: number,
  previous: number,
  compare: Compare,
  firstDay: string,
): Change | null {
  // No window means the comparison failed to load; "flat" would be a claim.
  if (!compare.previous_from) return null;
  const window = `${compare.previous_from} → ${compare.previous_to}`;
  if (firstDay && compare.previous_from < firstDay) {
    const note =
      compare.previous_to < firstDay
        ? "no prior period recorded"
        : "prior period only partly recorded";
    return { window, note };
  }
  if (previous <= 0) return null;
  return { window, pct: ((current - previous) / previous) * 100 };
}

/**
 * Billed dollars per million billed tokens. Dividing by all tokens would fold
 * in seat usage, which has no marginal cost; with no billed tokens to divide
 * by there is no rate, not a wrong one.
 */
export function ratePerMillion(g: Group): number | null {
  const { billed_tokens: billedTokens, billed_usd: billedUSD } = g.totals;
  return billedTokens > 0 ? (billedUSD / billedTokens) * 1e6 : null;
}

/** Promise.allSettled over a record, so every result keeps the name it was requested under. */
async function settleAll<T extends Record<string, Promise<unknown>>>(
  requests: T,
): Promise<{ [K in keyof T]: PromiseSettledResult<Awaited<T[K]>> }> {
  const keys = Object.keys(requests) as (keyof T)[];
  const results = await Promise.allSettled(keys.map((k) => requests[k]));
  return Object.fromEntries(keys.map((k, i) => [k, results[i]])) as {
    [K in keyof T]: PromiseSettledResult<Awaited<T[K]>>;
  };
}

/** The value a request produced, or the empty shape its card renders. */
function valueOr<T>(res: PromiseSettledResult<T>, fallback: T): T {
  return res.status === "fulfilled" ? res.value : fallback;
}

export type Data = {
  /**
   * The range these figures were fetched for. The charts draw on it rather
   * than on the range just picked, or old data would be redrawn over the new
   * range until the new data arrived.
   */
  range: { from: string; to: string };
  /** The settleAll keys of the requests that failed, so each card can say so. */
  failed: RequestName[];
  summary: Summary;
  days: Daily[];
  modelDays: ModelDay[];
  people: Group[];
  models: Group[];
  harnesses: Group[];
  surfaces: Group[];
  origins: Group[];
  modelEffort: MatrixCell[];
  modelEffortOrder?: string[];
  compare: Compare;
  heat: HeatCell[];
  /** First UTC day with hourly detail; empty when nothing was pruned. */
  detailFrom: string;
  /** The heatmap's zone, in minutes east of UTC. */
  heatOffset: number;
  sessions: SessionRow[];
  health: SourceHealth[];
  unknown: UnknownRow[];
  agents: AgentRow[];
  /** The build the team should be on: both halves ship from one tag. */
  server: Version;
  /** The server's clock, in unix seconds, so an unsynced browser cannot age every row. */
  agentsNow: number;
};

/** Every request the page makes, keyed by the name a failure is reported under. */
function requestsFor(filter: Filter, signal: AbortSignal) {
  return {
    summary: api.summary(filter, signal),
    daily: api.daily(filter, signal),
    "daily/model": api.dailyByModel(filter, signal),
    "breakdown:person": api.breakdown("person", filter, signal),
    "breakdown:model": api.breakdown("model", filter, signal),
    "breakdown:source": api.breakdown("source", filter, signal),
    "breakdown:surface": api.breakdown("surface", filter, signal),
    heatmap: api.heatmap(filter, signal),
    sessions: api.topSessions(filter, signal),
    health: api.health(signal),
    unknown: api.unknown(signal),
    "breakdown:origin": api.breakdown("origin", filter, signal),
    compare: api.compare(filter, signal),
    matrix: api.matrix(filter, "model", "effort", signal),
    agents: api.agents(signal),
  };
}

/** A request's name, as `Data.failed` lists it and a card checks it. */
export type RequestName = keyof ReturnType<typeof requestsFor>;

/**
 * Loads the page. Settled, not all: one failing endpoint costs its own card,
 * not the page.
 */
export async function loadDashboard(filter: Filter, signal: AbortSignal): Promise<Data> {
  const r = await settleAll(requestsFor(filter, signal));

  // The summary is the one request the page cannot be assembled without.
  if (r.summary.status === "rejected") throw r.summary.reason;
  const summary = r.summary.value;

  // A 401 anywhere means the whole page is unauthenticated; empty cards
  // would render a healthy server as a team that did no work.
  for (const res of Object.values(r)) {
    if (res.status === "rejected" && res.reason instanceof UnauthorizedError) {
      throw res.reason;
    }
  }

  const failed = Object.entries(r)
    .filter(([, res]) => res.status === "rejected")
    .map(([name]) => name as RequestName);
  const empty = { groups: [] as Group[] };
  const agents = valueOr(r.agents, { agents: [], now: 0, server_version: "" });
  const matrix = valueOr(r.matrix, { cells: [] as MatrixCell[], col_order: undefined });
  const heat = valueOr(r.heatmap, {
    cells: [] as HeatCell[],
    detail_from: "",
    utc_offset_minutes: 0,
  });

  return {
    range: { from: filter.from, to: filter.to },
    summary,
    days: valueOr(r.daily, { days: [] }).days,
    modelDays: valueOr(r["daily/model"], { points: [] }).points,
    people: valueOr(r["breakdown:person"], empty).groups,
    models: valueOr(r["breakdown:model"], empty).groups,
    harnesses: valueOr(r["breakdown:source"], empty).groups,
    surfaces: valueOr(r["breakdown:surface"], empty).groups,
    heat: heat.cells,
    detailFrom: heat.detail_from ?? "",
    heatOffset: heat.utc_offset_minutes,
    sessions: valueOr(r.sessions, { sessions: [] }).sessions,
    health: valueOr(r.health, { sources: [] }).sources,
    unknown: valueOr(r.unknown, { unknown: [] }).unknown,
    agents: agents.agents,
    server: { version: agents.server_version, release: summary.server_release },
    agentsNow: agents.now || Math.floor(Date.now() / 1000),
    origins: valueOr(r["breakdown:origin"], empty).groups,
    modelEffort: matrix.cells,
    modelEffortOrder: matrix.col_order,
    compare: valueOr(r.compare, {
      previous: summary.totals,
      previous_from: "",
      previous_to: "",
    }),
    failed,
  };
}

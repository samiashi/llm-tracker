import { useCallback, useEffect, useId, useState } from "react";
import { api, describeFailure, UnauthorizedError } from "@/api";
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
import { exact, since, tokens, usd, utcMidnightAt, utcOffset } from "@/format";
import { BarList, Card, Tile } from "@/components/parts";
import type { Change } from "@/components/parts";
import { TokenChart } from "@/components/TokenChart";
import { ModelChart } from "@/components/ModelChart";
import { Heatmap } from "@/components/Heatmap";
import { MatrixBars } from "@/components/MatrixBars";
import { Composition } from "@/components/Composition";
import { Skeleton } from "@/components/Skeleton";
import { AgentsTable, HealthTable, SessionsTable, UnknownTable } from "@/components/tables";
import { SLOTS, TOKEN_KIND } from "@/palette";
import { STALE_MS, useLiveData } from "@/useLiveData";
import { intentFromURL, isoAt, MAX_DAYS, resolve, writeIntentToURL } from "@/window";
import type { Intent } from "@/window";

const PRESETS = [
  { label: "7d", days: 7 },
  { label: "30d", days: 30 },
  { label: "90d", days: 90 },
  { label: "All", days: MAX_DAYS },
];

/**
 * Change against the prior window of equal length, or null when there is none
 * to show. A prior window that starts before the recorded history is only
 * partly measured, and its percentage would report the install date as growth.
 */
function change(
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
 * in seat usage, which has no marginal cost; with no billed token count to
 * divide by (an older server sends none) there is no rate, not a wrong one.
 */
function ratePerMillion(g: Group): number | null {
  const billedTokens = g.totals.billed_tokens ?? 0;
  return billedTokens > 0 ? (g.totals.billed_usd / billedTokens) * 1e6 : null;
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

type Data = {
  /**
   * The range these figures were fetched for. The charts draw on it rather
   * than on the range just picked, or old data would be redrawn over the new
   * range until the new data arrived.
   */
  range: { from: string; to: string };
  /** The settleAll keys of the requests that failed, so each card can say so. */
  failed: string[];
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
  /** The heatmap's zone, in minutes east of UTC; undefined from an older server. */
  heatOffset?: number;
  sessions: SessionRow[];
  health: SourceHealth[];
  unknown: UnknownRow[];
  agents: AgentRow[];
  /** The release the team should be on: both halves ship from one tag. */
  serverVersion: string;
  /** The server's clock, in unix seconds, so an unsynced browser cannot age every row. */
  agentsNow: number;
};

export default function App() {
  const [intent, setIntent] = useState<Intent>(() => intentFromURL());
  const [clock, setClock] = useState(() => Date.now());
  const tzId = useId();

  // Ticks so a relative window rolls over shortly after UTC midnight.
  useEffect(() => {
    const t = window.setInterval(() => setClock(Date.now()), 10_000);
    return () => window.clearInterval(t);
  }, []);

  const filter = resolve(intent, clock);
  // The viewer's offset, for saying where a UTC day starts on their clock.
  const localOffset = -new Date(clock).getTimezoneOffset();
  const dayStart = utcMidnightAt(localOffset);

  useEffect(() => {
    writeIntentToURL(intent);
    // Back and Forward only move between the page and an expanded card. The
    // range and person belong to the page, so whichever entry the browser
    // lands on is brought up to date with them.
    const onPop = () => writeIntentToURL(intent);
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, [intent]);

  const load = useCallback(
    async (signal: AbortSignal): Promise<Data> => {
      // Settled, not all: one failing endpoint costs its own card, not the
      // page. Keyed, so each failure is named once, under its request.
      const r = await settleAll({
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
      });

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
        .map(([name]) => name);
      const empty = { groups: [] as Group[] };
      const agents = valueOr(r.agents, { agents: [], now: 0, server_version: "" });
      const matrix = valueOr(r.matrix, { cells: [] as MatrixCell[], col_order: undefined });
      const heat = valueOr(r.heatmap, { cells: [] as HeatCell[], detail_from: "" });

      return {
        range: { from: filter.from, to: filter.to },
        summary,
        days: valueOr(r.daily, { days: [] }).days ?? [],
        modelDays: valueOr(r["daily/model"], { points: [] }).points ?? [],
        people: valueOr(r["breakdown:person"], empty).groups ?? [],
        models: valueOr(r["breakdown:model"], empty).groups ?? [],
        harnesses: valueOr(r["breakdown:source"], empty).groups ?? [],
        surfaces: valueOr(r["breakdown:surface"], empty).groups ?? [],
        heat: heat.cells ?? [],
        detailFrom: heat.detail_from ?? "",
        heatOffset: heat.utc_offset_minutes,
        sessions: valueOr(r.sessions, { sessions: [] }).sessions ?? [],
        health: valueOr(r.health, { sources: [] }).sources ?? [],
        unknown: valueOr(r.unknown, { unknown: [] }).unknown ?? [],
        agents: agents.agents ?? [],
        serverVersion: agents.server_version ?? "",
        agentsNow: agents.now || Math.floor(Date.now() / 1000),
        origins: valueOr(r["breakdown:origin"], empty).groups ?? [],
        modelEffort: matrix.cells ?? [],
        modelEffortOrder: matrix.col_order,
        compare: valueOr(r.compare, {
          current: summary.totals,
          previous: summary.totals,
          previous_from: "",
          previous_to: "",
        }),
        failed,
      };
    },
    [filter],
  );

  const live = useLiveData<Data>(load, `${filter.from}|${filter.to}|${filter.person ?? ""}`);
  const { data, loading, refreshing, updatedAt, now } = live;
  const loadFailed = live.error !== null;

  if (live.unauthorized) {
    const next = encodeURIComponent(window.location.pathname + window.location.search);
    return (
      <div className="wrap">
        <div className="banner">
          <b>Your session has expired.</b> <a href={`/auth/login?next=${next}`}>Sign in again</a> to
          keep going.
        </div>
      </div>
    );
  }
  // Only the first load shows a skeleton. Every later state keeps the header
  // and the range controls, so a failed range can always be changed or retried.
  if (!data && !loadFailed) return <Skeleton />;

  const stale = updatedAt !== null && now - updatedAt > STALE_MS;
  const failed = data?.failed ?? [];
  const setPerson = (person?: string) => setIntent((i) => ({ ...i, person }));
  // Typed dates go through resolve, as a link's do.
  const setRange = (from: string, to: string) => {
    if (!from || !to) return; // a cleared box is not a new range
    const r = resolve({ from, to }, clock);
    setIntent((i) => ({ person: i.person, from: r.from, to: r.to }));
  };

  return (
    <div className="wrap">
      {/* A new range keeps the old figures on screen while it loads; this says they are old. */}
      {loading && <div className="loadbar" role="status" aria-label="Loading" />}

      <header className="top">
        <h1>Team token usage</h1>
        <span className="sub">
          history {data?.summary.history_first_day || "—"} → {data?.summary.history_last_day || "—"}
        </span>

        <span className="right">
          <span className="freshness">
            <span
              className={`pulse${loadFailed ? " error" : stale ? " stale" : ""}${refreshing || loading ? " live" : ""}`}
            />
            {loadFailed ? (
              <span title={describeFailure(live.error).title}>
                {data ? "refresh failed — showing last good data" : "not loaded"}
              </span>
            ) : failed.length > 0 ? (
              <span title={`Failed: ${failed.join(", ")}`}>
                {failed.length} panel{failed.length > 1 ? "s" : ""} could not load
              </span>
            ) : (
              <span>updated {updatedAt ? since(updatedAt, now) : "—"}</span>
            )}
            <button onClick={live.refresh} disabled={refreshing}>
              {refreshing ? "refreshing…" : "refresh"}
            </button>
            <span className="cadence">auto 60s · agents 5m</span>
          </span>
          {/* An action on the range, not an input to it, so it sits with refresh. */}
          <a className="export" href={api.exportURL(filter)} download>
            export CSV
          </a>
        </span>
      </header>

      <p className="sub" style={{ marginTop: 6 }}>
        Counts only — no prompts, code or file contents leave any machine.
      </p>

      <div className="filters">
        {PRESETS.map((p) => (
          <button
            key={p.days}
            aria-pressed={intent.preset === p.days}
            onClick={() => setIntent((i) => ({ person: i.person, preset: p.days }))}
          >
            {p.label}
          </button>
        ))}
        <input
          type="date"
          value={filter.from}
          min={isoAt(clock, MAX_DAYS - 1)}
          max={filter.to}
          aria-label="From date"
          aria-describedby={tzId}
          onChange={(e) => setRange(e.target.value, filter.to)}
        />
        <span className="sep">→</span>
        <input
          type="date"
          value={filter.to}
          min={filter.from}
          max={isoAt(clock)}
          aria-label="To date"
          aria-describedby={tzId}
          onChange={(e) => setRange(filter.from, e.target.value)}
        />
        <span
          className="tz"
          id={tzId}
          title={`Every card but the hourly grid counts UTC days, as the server stores them${localOffset ? `: in your time zone (${utcOffset(localOffset)}) each runs ${dayStart}–${dayStart}` : ""}.`}
        >
          <span className="tz-pill">UTC</span>
          {localOffset !== 0 && (
            <span className="tz-local">
              days run {dayStart}–{dayStart} your time
            </span>
          )}
        </span>
        {filter.person && (
          <span className="chip">
            {filter.person}
            <button onClick={() => setPerson(undefined)} aria-label="Clear person filter">
              ×
            </button>
          </span>
        )}
      </div>

      <div className={loading ? "stale-data" : undefined} aria-busy={loading}>
        {data ? (
          <Dashboard data={data} filter={filter} setPerson={setPerson} localOffset={localOffset} />
        ) : (
          <LoadFailure error={live.error} retry={live.refresh} retrying={refreshing} />
        )}
      </div>
    </div>
  );
}

/** In place of the cards when a load fails with nothing to show: what went wrong, and a way back. */
function LoadFailure({
  error,
  retry,
  retrying,
}: {
  error: unknown;
  retry: () => void;
  retrying: boolean;
}) {
  const { title, detail } = describeFailure(error);
  return (
    <div className="banner loadfail" role="alert">
      <b>{title}</b> {detail}{" "}
      <button onClick={retry} disabled={retrying}>
        {retrying ? "Retrying…" : "Retry"}
      </button>
    </div>
  );
}

function Dashboard({
  data,
  filter,
  setPerson,
  localOffset,
}: {
  data: Data;
  filter: Filter;
  setPerson: (person?: string) => void;
  /** The viewer's offset from UTC, in minutes. */
  localOffset: number;
}) {
  const down = new Set(data.failed);
  const { summary, compare } = data;
  const t = summary.totals;
  const prev = compare.previous;
  const firstDay = summary.history_first_day;
  const delta = (current: number, previous: number) => change(current, previous, compare, firstDay);

  const cacheable = t.cache_read_tokens + t.input_tokens;
  const cacheHitRate = cacheable ? (t.cache_read_tokens / cacheable) * 100 : 0;
  const unpricedShare = t.total_tokens ? (t.unpriced_tokens / t.total_tokens) * 100 : 0;
  const subagentTokens = data.origins.find((g) => g.key === "subagent")?.totals.total_tokens ?? 0;
  const subagentShare = t.total_tokens ? (subagentTokens / t.total_tokens) * 100 : 0;
  const unknownBasis = t.unknown_basis_usd ?? 0;

  // The chart draws the busiest models; the breakdown is the complete list.
  const charted = new Set(data.modelDays.map((p) => p.model));
  const otherModels = data.models.filter((g) => !charted.has(g.key)).length;

  // The range the figures on screen were fetched for, clamped to the first
  // recorded day so "All" does not draw ten years against a week of data.
  const range = {
    from: firstDay && firstDay > data.range.from ? firstDay : data.range.from,
    to: data.range.to,
  };

  const rated = data.models
    .filter((g) => g.totals.billed_usd > 0)
    .sort((a, b) => (ratePerMillion(b) ?? -1) - (ratePerMillion(a) ?? -1));

  return (
    <>
      {unpricedShare > 1 && (
        <div className="banner">
          <b>{unpricedShare.toFixed(1)}% of tokens are unpriced.</b> {tokens(t.unpriced_tokens)}{" "}
          tokens use a model with no entry in the price table. They are counted but deliberately not
          costed — an unrecognised model shows here rather than quietly reading as free.
        </div>
      )}

      <div className="grid tiles" style={{ marginBottom: 14 }}>
        <Tile
          label="Total tokens"
          value={tokens(t.total_tokens)}
          delta={delta(t.total_tokens, prev.total_tokens)}
          hint={`${exact(t.events)} responses`}
        />
        <Tile
          label="Billed spend"
          value={usd(t.billed_usd)}
          delta={delta(t.billed_usd, prev.billed_usd)}
          hint="Real money: metered API keys"
        />
        <Tile
          label="Rate-card equivalent"
          value={usd(t.rate_card_usd)}
          delta={delta(t.rate_card_usd, prev.rate_card_usd)}
          hint="Subscription seats — what this would have cost on the API. Not spend, and never added to it."
        />
        {unknownBasis > 0 && (
          <Tile
            label="Basis unknown — at list prices"
            value={usd(unknownBasis)}
            delta={delta(unknownBasis, prev.unknown_basis_usd ?? 0)}
            hint="Priced usage that may be metered or a seat: Copilot, or an API key with no detected account. Never added to either figure."
          />
        )}
        <Tile
          label="Subagent share"
          value={`${subagentShare.toFixed(0)}%`}
          failed={down.has("breakdown:origin")}
          hint={`${tokens(subagentTokens)} of tokens come from agent fan-out rather than the main thread`}
        />
        <Tile
          label="Cache hit rate"
          value={`${cacheHitRate.toFixed(1)}%`}
          hint={`${tokens(t.cache_read_tokens)} served from cache at a fraction of input price`}
        />
      </div>

      <div className="grid two" style={{ marginBottom: 14 }}>
        <Card
          title="Billable input & output"
          failed={down.has("daily")}
          note="Input and output tokens per day, the ones that drive cost. Lines overlap rather than stack; cache traffic has its own chart because it runs about 40× larger."
        >
          {(v) => (
            <TokenChart
              range={range}
              days={data.days}
              height={v.chartHeight}
              series={[
                { key: "input_tokens", name: "Input", color: TOKEN_KIND.input },
                { key: "output_tokens", name: "Output", color: TOKEN_KIND.output },
              ]}
            />
          )}
        </Card>
        <Card
          title="Cache traffic"
          failed={down.has("daily")}
          note="Cache reads and writes per day. Lines overlap rather than stack. Reads usually dwarf all other tokens but cost about a tenth of the input price."
        >
          {(v) => (
            <TokenChart
              range={range}
              days={data.days}
              height={v.chartHeight}
              series={[
                { key: "cache_read_tokens", name: "Cache read", color: TOKEN_KIND.cacheRead },
                { key: "cache_write_tokens", name: "Cache write", color: TOKEN_KIND.cacheWrite },
              ]}
            />
          )}
        </Card>
      </div>

      <div className="grid" style={{ marginBottom: 14 }}>
        <Card
          title="Model mix over time"
          failed={down.has("daily/model")}
          note={`Tokens per day for the ${charted.size} busiest models${otherModels > 0 ? `; the other ${otherModels} are under By model` : ""}. Lines overlap rather than stack, and each model keeps one colour for the whole range.`}
        >
          {(v) => <ModelChart points={data.modelDays} range={range} height={v.chartHeight} />}
        </Card>
      </div>

      <div className="grid two" style={{ marginBottom: 14 }}>
        <Card
          title="By person"
          failed={down.has("breakdown:person")}
          note="Tokens per person, with all their accounts combined. Click a name to filter the page to that person."
        >
          {(v) => (
            <BarList
              groups={data.people}
              color={SLOTS[0]}
              value={(g) => g.totals.total_tokens}
              format={tokens}
              max={v.expanded ? Infinity : 6}
              onMore={v.open}
              // The filter applies to the whole page, so show the page.
              onSelect={(k) => {
                v.close();
                setPerson(k === filter.person ? undefined : k);
              }}
              selected={filter.person}
            />
          )}
        </Card>
        <Card title="By model" failed={down.has("breakdown:model")}>
          {(v) => (
            <BarList
              groups={data.models}
              color={SLOTS[1]}
              value={(g) => g.totals.total_tokens}
              format={tokens}
              max={v.expanded ? Infinity : undefined}
              onMore={v.open}
            />
          )}
        </Card>
        <Card
          title="By harness"
          failed={down.has("breakdown:source")}
          note="Tokens per coding tool, such as Claude Code or Codex."
        >
          {(v) => (
            <BarList
              groups={data.harnesses}
              color={SLOTS[2]}
              value={(g) => g.totals.total_tokens}
              format={tokens}
              max={v.expanded ? Infinity : undefined}
              onMore={v.open}
            />
          )}
        </Card>
        <Card
          title="By surface"
          failed={down.has("breakdown:surface")}
          note="Tokens by where the tool ran: terminal, desktop app or IDE."
        >
          {(v) => (
            <BarList
              groups={data.surfaces}
              color={SLOTS[3]}
              value={(g) => g.totals.total_tokens}
              format={tokens}
              max={v.expanded ? Infinity : undefined}
              onMore={v.open}
            />
          )}
        </Card>
      </div>

      <div className="grid two" style={{ marginBottom: 14 }}>
        <Card
          title="Reasoning effort by model"
          failed={down.has("matrix")}
          note="Each model's tokens split by reasoning effort. Bar length compares models; segments show each effort level's share."
        >
          {(v) => (
            <MatrixBars
              cells={data.modelEffort}
              colOrder={data.modelEffortOrder}
              max={v.expanded ? Infinity : undefined}
              onMore={v.open}
            />
          )}
        </Card>
        <Card
          title="Effective rate per 1M tokens"
          failed={down.has("breakdown:model")}
          note="What pay-as-you-go usage actually cost per million tokens, after cache discounts. Subscription seats are left out: they have no per-token price."
        >
          {(v) => (
            <BarList
              groups={rated}
              color={SLOTS[1]}
              max={v.expanded ? Infinity : 6}
              onMore={v.open}
              value={ratePerMillion}
              format={(rate) => `$${rate.toFixed(2)}`}
              summable={false}
              emptyNote="No metered usage in this range."
            />
          )}
        </Card>
      </div>

      <div className="grid" style={{ marginBottom: 14 }}>
        <Card
          title="When the work happens"
          failed={down.has("heatmap")}
          note="Tokens by day and hour. Darker is busier; shades are quantiles, so one runaway hour cannot wash out the rest. Activity overnight is usually an agent left running."
        >
          {() => (
            <div className="scroll">
              <Heatmap
                cells={data.heat}
                range={range}
                detailFrom={data.detailFrom}
                serverOffset={data.heatOffset}
                localOffset={localOffset}
              />
            </div>
          )}
        </Card>
      </div>

      <div className="grid" style={{ marginBottom: 14 }}>
        <Card
          title="Most expensive sessions"
          failed={down.has("sessions")}
          note="The costliest sessions in this range. A session far above the rest is usually worth a look."
        >
          {(v) => (
            <SessionsTable
              sessions={data.sessions}
              now={data.agentsNow}
              max={v.expanded ? Infinity : undefined}
              onMore={v.open}
            />
          )}
        </Card>
      </div>

      <div className="grid two">
        <Card title="Where the tokens go" note="All tokens in this range, split by type.">
          <Composition totals={t} />
        </Card>

        <Card
          title="Collector health"
          failed={down.has("health")}
          note="When each tool last produced usage. If a tool you still use stops updating, its log format may have changed."
        >
          <HealthTable sources={data.health} now={data.agentsNow} />
        </Card>
      </div>

      {(data.agents.length > 0 || down.has("agents")) && (
        <div className="grid" style={{ marginTop: 14 }}>
          <Card
            title="Agents"
            failed={down.has("agents")}
            note="Every machine running the collector. Last sync is a heartbeat, not when anyone worked: if it is old, that machine's usage is missing from the totals."
          >
            <AgentsTable
              agents={data.agents}
              now={data.agentsNow}
              serverVersion={data.serverVersion}
            />
          </Card>
        </div>
      )}

      {(data.unknown.length > 0 || down.has("unknown")) && (
        <div className="grid" style={{ marginTop: 14 }}>
          <Card
            collapsed
            failed={down.has("unknown")}
            title={`Detected but unsupported${down.has("unknown") ? "" : ` (${data.unknown.length})`}`}
            note="Coding tools found on a machine that the collector cannot read. todo: support could be added. blocked: investigated and not readable; the reason is shown."
          >
            <UnknownTable rows={data.unknown} now={data.agentsNow} />
          </Card>
        </div>
      )}
    </>
  );
}

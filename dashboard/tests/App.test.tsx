import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import App from "@/App";
import type { Totals } from "@/api";
import { utcMidnightAt } from "@/format";

// Rendering the whole page catches what the pure-function tests cannot: a
// runtime error that leaves a blank page while every asset still returns 200.

const totals: Totals = {
  events: 10,
  total_tokens: 1000,
  input_tokens: 100,
  output_tokens: 200,
  cache_read_tokens: 600,
  cache_write_tokens: 100,
  billed_usd: 1.5,
  billed_tokens: 0,
  rate_card_usd: 20,
  unknown_basis_usd: 0,
  unpriced_tokens: 0,
};

const group = (key: string, t: object = totals) => ({ key, totals: t });

const session = {
  session_id: "s1",
  source: "claude_code",
  model: "claude-opus-5",
  effort: "high",
  email: "a@b.c",
  tokens: 500,
  billed_usd: 1,
  rate_card_usd: 2,
  events: 5,
  last_seen: 1,
};

/** RequestInfo is Request | string, and Request has no useful toString. */
const urlOf = (input: RequestInfo | URL): string =>
  typeof input === "string" ? input : input instanceof URL ? input.href : input.url;

const ok = (v: unknown) =>
  Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(v) } as Response);
/** An error status, with the server's `{"error": ...}` body when `error` is given. */
const fail = (status: number, error?: string) =>
  Promise.resolve({
    ok: false,
    status,
    json: () =>
      error === undefined ? Promise.reject(new SyntaxError()) : Promise.resolve({ error }),
  } as Response);

type Reply = (url: string) => Promise<Response>;

/** Answers every endpoint; a URL containing a key of `over` gets that reply instead. */
function mockFetch(over: Record<string, Reply> = {}) {
  return vi.fn((input: RequestInfo | URL) => {
    const url = urlOf(input);
    for (const [part, reply] of Object.entries(over)) if (url.includes(part)) return reply(url);

    if (url.includes("/v1/summary"))
      return ok({ totals, history_first_day: "2026-09-01", history_last_day: "2026-09-22" });
    if (url.includes("/v1/compare"))
      return ok({
        previous: totals,
        previous_from: "2026-08-01",
        previous_to: "2026-08-31",
      });
    if (url.includes("/v1/daily/model"))
      return ok({ points: [{ day: "2026-09-21", model: "claude-opus-5", tokens: 500 }] });
    if (url.includes("/v1/daily")) return ok({ days: [{ day: "2026-09-21", totals }] });
    if (url.includes("/v1/breakdown")) return ok({ groups: [group("claude-opus-5")] });
    if (url.includes("/v1/matrix"))
      return ok({
        cells: [
          {
            row: "claude-opus-5",
            col: "high",
            tokens: 500,
            billed_usd: 1,
            rate_card_usd: 2,
            unknown_basis_usd: 0,
          },
        ],
        col_order: ["low", "medium", "high"],
      });
    if (url.includes("/v1/heatmap"))
      return ok({ cells: [{ day: "2026-09-21", weekday: 1, hour: 14, tokens: 500, events: 5 }] });
    if (url.includes("/v1/sessions/top")) return ok({ sessions: [session] });
    if (url.includes("/v1/health")) return ok({ sources: [] });
    if (url.includes("/v1/unknown")) return ok({ unknown: [] });
    if (url.includes("/v1/agents"))
      return ok({
        agents: [
          {
            machine_id: "m1",
            hostname: "mac",
            person: "a@b.c",
            agent_version: "v1.0.0",
            last_sync: 1,
            events: 3,
          },
        ],
        now: 0,
        server_version: "",
      });
    return ok({});
  });
}

/** The card whose heading is `title`. */
const card = (title: string) =>
  screen.getByRole("heading", { name: title }).closest<HTMLElement>(".card")!;

const loaded = () => waitFor(() => expect(screen.getByText("Total tokens")).toBeTruthy());

// A fixed clock, so "today" and every preset are the same on every run. Only
// Date is faked: real timers keep waitFor and the request timeout working.
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-09-23T12:00:00Z"));
});

// Without cleanup each render accumulates in one document, and getByText then
// fails on multiple matches rather than on the thing being tested.
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.replaceState(null, "", "/");
});

describe("App", () => {
  it("renders the dashboard without throwing", async () => {
    vi.stubGlobal("fetch", mockFetch());
    render(<App />);
    await loaded();
    expect(screen.getByText("Where the tokens go")).toBeTruthy();
    expect(screen.getByText("Reasoning effort by model")).toBeTruthy();
  });

  it("expands By model to every model the grid leaves out", async () => {
    const many = Array.from({ length: 12 }, (_, i) => group(`model-${i + 1}`));
    vi.stubGlobal("fetch", mockFetch({ "by=model": () => ok({ groups: many }) }));
    render(<App />);
    await loaded();
    expect(within(card("By model")).queryByText("model-12")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Expand By model" }));
    const dialog = await screen.findByRole("dialog", { name: "By model" });
    expect(within(dialog).getByText("model-12")).toBeTruthy();
  });

  // Charts drew the old data over the newly picked range until the new data
  // arrived, so they changed shape twice. The heatmap has a row per day of
  // its range, which makes the range it is drawing on countable.
  it("keeps drawing the old figures on their own range until the new ones arrive", async () => {
    let release = () => {};
    const gate = new Promise<void>((r) => (release = r));
    let held = false;
    const answer = mockFetch();
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        if (held) await gate;
        return answer(input);
      }),
    );
    render(<App />);
    await loaded();
    const rows = () => within(card("When the work happens")).getAllByRole("rowheader").length;
    const before = rows();
    held = true;
    fireEvent.click(screen.getByRole("button", { name: "7d" }));
    expect(rows()).toBe(before);
    release();
    await waitFor(() => expect(rows()).toBeLessThan(before));
  });

  it("says where a UTC day starts on the viewer's clock", async () => {
    vi.stubGlobal("fetch", mockFetch());
    render(<App />);
    await loaded();
    const offset = -new Date().getTimezoneOffset();
    const chip = document.querySelector(".tz")!;
    if (offset === 0) expect(chip.textContent).toBe("UTC");
    else {
      const start = utcMidnightAt(offset);
      expect(chip.textContent).toContain(`days run ${start}–${start} your time`);
    }
  });

  it("filters by a person picked in the expanded list, keeping the filter in the URL", async () => {
    const people = [group("a@b.c"), group("d@e.f")];
    vi.stubGlobal("fetch", mockFetch({ "by=person": () => ok({ groups: people }) }));
    render(<App />);
    await loaded();
    fireEvent.click(screen.getByRole("button", { name: "Expand By person" }));
    const dialog = await screen.findByRole("dialog", { name: "By person" });
    // Closing pops the card's history entry, and the filter must survive it.
    fireEvent.click(within(dialog).getByRole("button", { name: /d@e\.f/ }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => {
      const q = new URLSearchParams(window.location.search);
      expect(q.get("person")).toBe("d@e.f");
      expect(q.get("card")).toBeNull();
    });
  });

  // The preset form is what the app writes for itself, so most links carry it.
  it("renders when the URL carries a preset", async () => {
    window.history.replaceState(null, "", "/?preset=30");
    vi.stubGlobal("fetch", mockFetch());
    render(<App />);
    await loaded();
  });

  it("offers the export from the header, scoped to the current filter", async () => {
    window.history.replaceState(null, "", "/?from=2026-09-01&to=2026-09-07");
    vi.stubGlobal("fetch", mockFetch());
    render(<App />);
    await loaded();

    const link = screen.getByRole("link", { name: /export csv/i });
    expect(link.getAttribute("href")).toContain("from=2026-09-01");
    expect(link.getAttribute("href")).toContain("to=2026-09-07");
    expect(link.closest("header")).not.toBeNull();
  });

  // A failure counts once per endpoint, however many fields read it: /v1/agents feeds three.
  it.each(["/v1/agents", "/v1/matrix", "/v1/breakdown"])(
    "names each failing endpoint once (%s)",
    async (failing) => {
      vi.stubGlobal("fetch", mockFetch({ [failing]: () => fail(500) }));
      render(<App />);
      const n = failing === "/v1/breakdown" ? 5 : 1;
      await waitFor(() =>
        expect(screen.getByText(`${n} panel${n > 1 ? "s" : ""} could not load`)).toBeTruthy(),
      );
    },
  );
});

describe("the dashboard counts UTC days, as the server does", () => {
  // 22:30 on 22 September in Santiago: in UTC it is already the 23rd, and the
  // events of the last hour and a half are stored under the 23rd.
  it("asks for the UTC day it is now, and offers it in the To box", async () => {
    vi.setSystemTime(new Date("2026-09-23T01:30:00Z"));
    window.history.replaceState(null, "", "/?preset=7");
    const fetch = mockFetch();
    vi.stubGlobal("fetch", fetch);
    render(<App />);
    await loaded();

    const summary = fetch.mock.calls.map(([u]) => urlOf(u)).find((u) => u.includes("summary"));
    expect(summary).toContain("from=2026-09-17&to=2026-09-23");
    expect(screen.getByLabelText("To date").getAttribute("max")).toBe("2026-09-23");
    expect(screen.getByText("UTC")).toBeTruthy();
  });
});

describe("spend", () => {
  // 1M metered tokens that cost $10, plus 3M seat tokens: the metered rate is
  // $10 per million, where dividing by all 4M would say $2.50.
  it("divides billed dollars by billed tokens for the effective rate", async () => {
    const opus = { ...totals, total_tokens: 4e6, billed_tokens: 1e6, billed_usd: 10 };
    vi.stubGlobal("fetch", mockFetch({ "by=model": () => ok({ groups: [group("opus", opus)] }) }));
    render(<App />);
    await loaded();
    const rate = card("Effective rate per 1M tokens").querySelector(".barrow .val");
    expect(rate?.textContent).toBe("$10.00");
  });

  it("shows no rate, rather than a wrong one, without billed tokens to divide by", async () => {
    const seat = { ...totals, total_tokens: 4e6, billed_tokens: 0, billed_usd: 10 };
    vi.stubGlobal("fetch", mockFetch({ "by=model": () => ok({ groups: [group("opus", seat)] }) }));
    render(<App />);
    await loaded();
    const rate = card("Effective rate per 1M tokens").querySelector(".barrow .val");
    expect(rate?.textContent).toBe("—");
  });

  it("shows unknown-basis spend as its own figure, never inside billed or rate card", async () => {
    const t = { ...totals, unknown_basis_usd: 5 };
    vi.stubGlobal(
      "fetch",
      mockFetch({
        "/v1/summary": () =>
          ok({ totals: t, history_first_day: "2026-09-01", history_last_day: "2026-09-22" }),
        "/v1/sessions/top": () => ok({ sessions: [{ ...session, unknown_basis_usd: 0.75 }] }),
      }),
    );
    render(<App />);
    await loaded();

    const tile = (label: string) => screen.getByText(label).closest(".tile")!;
    expect(tile("Basis unknown — at list prices").querySelector(".value")?.textContent).toBe(
      "$5.00",
    );
    expect(tile("Billed spend").querySelector(".value")?.textContent).toBe("$1.50");
    expect(tile("Rate-card equivalent").querySelector(".value")?.textContent).toBe("$20.00");

    const sessions = card("Most expensive sessions");
    const headers = [...sessions.querySelectorAll("th")].map((th) => th.textContent);
    expect(headers).toEqual(expect.arrayContaining(["Billed", "Rate card", "Basis unknown"]));
    expect(sessions.textContent).toContain("$0.75");
  });

  it("hides unknown-basis spend when there is none", async () => {
    vi.stubGlobal("fetch", mockFetch());
    render(<App />);
    await loaded();
    expect(screen.queryByText(/basis unknown/i)).toBeNull();
  });
});

describe("the change against the prior period", () => {
  // History begins 10 August; the prior window for 23 Aug–22 Sep starts 24 July,
  // so half of it predates any data, and a percentage would read as growth.
  it("says the prior period is only partly recorded instead of a percentage", async () => {
    const half = { ...totals, total_tokens: 500, billed_usd: 0.75, rate_card_usd: 10 };
    vi.stubGlobal(
      "fetch",
      mockFetch({
        "/v1/summary": () =>
          ok({ totals, history_first_day: "2026-08-10", history_last_day: "2026-09-22" }),
        "/v1/compare": () =>
          ok({
            previous: half,
            previous_from: "2026-07-24",
            previous_to: "2026-08-22",
          }),
      }),
    );
    window.history.replaceState(null, "", "/?from=2026-08-23&to=2026-09-22");
    render(<App />);
    await loaded();

    const deltas = [...document.querySelectorAll(".delta")].map((d) => d.textContent);
    expect(deltas.length).toBeGreaterThan(0);
    expect(deltas.every((d) => d === "prior period only partly recorded")).toBe(true);
  });
});

describe("a failed request is never shown as an empty range", () => {
  it("says the card could not load instead of 'no activity'", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch({ "/v1/heatmap": () => fail(500), "/v1/matrix": () => fail(500) }),
    );
    render(<App />);
    await loaded();
    expect(screen.queryByText("No activity in this range.")).toBeNull();
    expect(screen.queryByText("No data in this range.")).toBeNull();
    expect(card("When the work happens").textContent).toContain("Could not load");
    expect(card("Reasoning effort by model").textContent).toContain("Could not load");
  });

  it("draws no zero line over a failed daily request", async () => {
    vi.stubGlobal("fetch", mockFetch({ "/v1/daily?": () => fail(500) }));
    window.history.replaceState(null, "", "/?from=2026-09-01&to=2026-09-22");
    render(<App />);
    await loaded();
    expect(document.querySelectorAll('[aria-label*="per day"]')).toHaveLength(0);
    expect(card("Billable input & output").textContent).toContain("Could not load");
  });

  it("shows subagent share as unknown, not 0%, when its request failed", async () => {
    vi.stubGlobal("fetch", mockFetch({ "by=origin": () => fail(500) }));
    render(<App />);
    await loaded();
    const tile = screen.getByText("Subagent share").closest(".tile")!;
    expect(tile.querySelector(".value")?.textContent).toBe("—");
    expect(tile.textContent).toContain("Could not load");
  });

  it("keeps the Agents and health cards, saying they could not load", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch({ "/v1/agents": () => fail(500), "/v1/health": () => fail(500) }),
    );
    render(<App />);
    await loaded();
    expect(card("Agents").textContent).toContain("Could not load");
    expect(card("Collector health").textContent).toContain("Could not load");
  });
});

describe("a load that fails keeps the way back", () => {
  it("keeps the range controls and a retry when a new range fails", async () => {
    let broken = false;
    const good = mockFetch();
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) =>
        broken && urlOf(input).includes("/v1/summary")
          ? fail(500, "database is locked")
          : good(input),
      ),
    );
    render(<App />);
    await loaded();

    broken = true;
    fireEvent.click(screen.getByRole("button", { name: "7d" }));
    await waitFor(() => expect(screen.getByText("The server hit an error.")).toBeTruthy());
    expect(screen.getByText(/database is locked/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "30d" })).toBeTruthy();
    expect(screen.getByLabelText("From date")).toBeTruthy();

    broken = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await loaded();
  });

  it("offers the controls and a retry when the first load cannot reach the server", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.reject(new TypeError("Failed to fetch"))),
    );
    render(<App />);
    await waitFor(() => expect(screen.getByText("Cannot reach the server.")).toBeTruthy());
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "7d" })).toBeTruthy();
    expect(screen.getByLabelText("To date")).toBeTruthy();
  });

  it("reports a rejected range as invalid, not as an outage, and lets another be chosen", async () => {
    const good = mockFetch();
    vi.stubGlobal(
      "fetch",
      vi.fn((input: RequestInfo | URL) =>
        urlOf(input).includes("from=2026-09-01&to=2026-09-10")
          ? fail(400, "from is not a calendar day")
          : good(input),
      ),
    );
    window.history.replaceState(null, "", "/?from=2026-09-01&to=2026-09-10");
    render(<App />);
    await waitFor(() => expect(screen.getByText("That date range isn't valid.")).toBeTruthy());
    expect(screen.getByText(/from is not a calendar day/)).toBeTruthy();
    expect(screen.queryByText("Cannot reach the server.")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "7d" }));
    await loaded();
  });
});

describe("the range sent to the server", () => {
  it("puts a typed From after To in order before sending it", async () => {
    const fetch = mockFetch();
    vi.stubGlobal("fetch", fetch);
    window.history.replaceState(null, "", "/?from=2026-09-01&to=2026-09-10");
    render(<App />);
    await loaded();

    fetch.mockClear();
    fireEvent.change(screen.getByLabelText("From date"), { target: { value: "2026-09-20" } });
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    const sent = fetch.mock.calls.map(([u]) => urlOf(u)).find((u) => u.includes("summary"));
    expect(sent).toContain("from=2026-09-10&to=2026-09-20");
    expect(window.location.search).toBe("?from=2026-09-10&to=2026-09-20");
  });
});

describe("relative times", () => {
  // The browser runs five minutes slow: on its own clock an event a minute
  // old on the server's is four minutes in the future.
  it("reads the sessions table's Last seen on the server clock", async () => {
    const serverNow = 1_790_000_000;
    vi.setSystemTime((serverNow - 300) * 1000);
    vi.stubGlobal(
      "fetch",
      mockFetch({
        "/v1/agents": () => ok({ agents: [], now: serverNow, server_version: "" }),
        "/v1/sessions/top": () => ok({ sessions: [{ ...session, last_seen: serverNow - 60 }] }),
      }),
    );
    render(<App />);
    await loaded();
    const lastSeen = card("Most expensive sessions").querySelector("tbody td:last-child");
    expect(lastSeen?.textContent).toBe("1m ago");
  });
});

import { afterEach, describe, expect, it } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Heatmap } from "@/components/Heatmap";
import type { HeatCell } from "@/api";

afterEach(cleanup);

const cell = (day: string, hour: number, tokens = 1_000): HeatCell => ({
  day,
  weekday: new Date(`${day}T00:00:00Z`).getUTCDay(),
  hour,
  tokens,
  events: 1,
});

const rowLabels = () => [...document.querySelectorAll(".rowlabel")].map((e) => e.textContent);
const tabStops = () => document.querySelectorAll('.hbody [tabindex="0"]');
const focused = () => document.activeElement?.getAttribute("aria-label") ?? "";

describe("the zone its hours are in", () => {
  const draw = (serverOffset?: number, localOffset?: number) =>
    render(
      <Heatmap
        range={{ from: "2026-09-10", to: "2026-09-11" }}
        cells={[cell("2026-09-10", 10)]}
        serverOffset={serverOffset}
        localOffset={localOffset}
      />,
    );

  it("says the hours are the viewer's own when the zones match", () => {
    draw(240, 240);
    expect(screen.getByText(/Hours are your local time \(UTC\+4\)/)).toBeTruthy();
  });

  // A container left on UTC shifts every hour by the team's offset.
  it("says so when the server's zone is not the viewer's", () => {
    draw(0, 240);
    expect(
      screen.getByText(/Hours are the server's time \(UTC\), not yours \(UTC\+4\)/),
    ).toBeTruthy();
  });

  it("falls back to the server's zone, unnamed, for an older server", () => {
    draw(undefined, 240);
    expect(screen.getByText(/Hours are in the server's time zone/)).toBeTruthy();
  });
});

describe("heatmap rows", () => {
  it("draws the server-local day before the range when the server returns it", () => {
    render(
      <Heatmap
        range={{ from: "2026-09-10", to: "2026-09-12" }}
        cells={[cell("2026-09-09", 22, 9_000_000), cell("2026-09-11", 10)]}
      />,
    );
    expect(rowLabels()).toEqual(["Sat 12 Sep", "Fri 11 Sep", "Thu 10 Sep", "Wed 9 Sep"]);
    expect(screen.getByText(/is behind UTC/)).toBeTruthy();
  });

  it("names days before detail_from as not kept rather than drawing them idle", () => {
    render(
      <Heatmap
        range={{ from: "2026-09-01", to: "2026-09-12" }}
        detailFrom="2026-09-10"
        cells={[cell("2026-09-11", 10)]}
      />,
    );
    expect(rowLabels()).toEqual(["Sat 12 Sep", "Fri 11 Sep", "Thu 10 Sep"]);
    expect(screen.getByText(/hourly detail not kept \(9 days\)/)).toBeTruthy();
  });

  it("says a range wholly before detail_from has no hourly detail, not no activity", () => {
    render(
      <Heatmap
        range={{ from: "2026-08-01", to: "2026-08-31" }}
        detailFrom="2026-09-10"
        cells={[]}
      />,
    );
    expect(screen.queryByText("No activity in this range.")).toBeNull();
    expect(screen.getByText(/not kept before 2026-09-10/)).toBeTruthy();
  });
});

describe("heatmap keyboard access", () => {
  const ninetyDays = () =>
    render(
      <>
        <Heatmap
          range={{ from: "2026-06-26", to: "2026-09-23" }}
          cells={[cell("2026-09-23", 9), cell("2026-09-01", 14)]}
        />
        <button>after</button>
      </>,
    );

  it("is one tab stop however many cells it has", () => {
    ninetyDays();
    expect(document.querySelectorAll(".hbody .cell")).toHaveLength(90 * 24);
    expect(tabStops()).toHaveLength(1);
    expect(screen.getByRole("grid", { name: "Tokens by day and hour" })).toBeTruthy();
  });

  it("moves focus with the arrow keys, Home/End and PageUp/PageDown", () => {
    ninetyDays();
    const start = tabStops()[0] as HTMLElement;
    act(() => start.focus());
    expect(focused()).toBe("Wed 23 Sep 00:00 — no activity");

    const press = (key: string, extra: object = {}) =>
      fireEvent.keyDown(document.activeElement!, { key, ...extra });
    press("ArrowRight");
    expect(focused()).toMatch(/^Wed 23 Sep 01:00/);
    press("End");
    expect(focused()).toMatch(/^Wed 23 Sep 23:00/);
    press("ArrowDown");
    expect(focused()).toMatch(/^Tue 22 Sep 23:00/);
    press("PageDown");
    expect(focused()).toMatch(/^Tue 15 Sep 23:00/);
    press("Home");
    expect(focused()).toMatch(/^Tue 15 Sep 00:00/);
    press("End", { ctrlKey: true });
    expect(focused()).toMatch(/^Fri 26 Jun 23:00/);
    press("PageUp");
    expect(focused()).toMatch(/^Fri 3 Jul 23:00/);

    // The tab stop follows focus, so Tab returns to the last cell visited.
    expect(tabStops()).toHaveLength(1);
    expect(tabStops()[0]).toBe(document.activeElement);
  });

  it("shows the tooltip on focus and drops it when focus leaves the grid", () => {
    ninetyDays();
    const start = tabStops()[0] as HTMLElement;
    act(() => start.focus());
    expect(screen.getByRole("tooltip").textContent).toContain("Wed 23 Sep");
    act(() => screen.getByText("after").focus());
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  // A hover restyles no cell: on a long range, per-cell hover state is
  // thousands of DOM writes for every mouse move.
  it("changes no cell when the pointer moves across the grid", () => {
    ninetyDays();
    const body = document.querySelector(".hbody")!;
    const seen = new MutationObserver(() => {});
    seen.observe(body, { attributes: true, childList: true, subtree: true });
    const cells = body.querySelectorAll(".cell");
    fireEvent.mouseOver(cells[30]);
    fireEvent.mouseOver(cells[500]);
    expect(screen.getByRole("tooltip")).toBeTruthy();
    expect(seen.takeRecords()).toHaveLength(0);
    seen.disconnect();
  });
});

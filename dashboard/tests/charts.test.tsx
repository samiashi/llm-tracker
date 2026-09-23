import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { Totals } from "@/api";
import { ModelChart } from "@/components/ModelChart";
import { TokenChart } from "@/components/TokenChart";
import { MODEL_SLOTS } from "@/palette";
import { drawn } from "./recharts";

vi.mock("recharts", async (real) => (await import("./recharts")).rechartsDouble(real));

const zero: Totals = {
  events: 0,
  total_tokens: 0,
  input_tokens: 0,
  output_tokens: 0,
  cache_read_tokens: 0,
  cache_write_tokens: 0,
  billed_usd: 0,
  billed_tokens: 0,
  rate_card_usd: 0,
  unknown_basis_usd: 0,
  unpriced_tokens: 0,
};
const range = { from: "2026-09-08", to: "2026-09-14" }; // seven days
const series = [
  { key: "input_tokens" as const, name: "Input", color: "var(--series-1)" },
  { key: "output_tokens" as const, name: "Output", color: "var(--series-2)" },
];

function reducedMotion(on: boolean) {
  vi.stubGlobal("matchMedia", () => ({
    matches: on,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

beforeEach(() => {
  drawn.length = 0;
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const tokenChart = () =>
  render(
    <TokenChart
      range={range}
      series={series}
      days={[
        { day: "2026-09-10", totals: { ...zero, input_tokens: 5, output_tokens: 1 } },
        { day: "2026-09-12", totals: { ...zero, input_tokens: 7, output_tokens: 2 } },
      ]}
    />,
  );

// "a" is the heavier model over the range, 120 to 90; "b" is the heavier on
// the last day either appears, 80 to 60.
const modelChart = () =>
  render(
    <ModelChart
      range={range}
      points={[
        { day: "2026-09-09", model: "a", tokens: 60 },
        { day: "2026-09-09", model: "b", tokens: 10 },
        { day: "2026-09-10", model: "a", tokens: 60 },
        { day: "2026-09-13", model: "b", tokens: 80 },
      ]}
    />,
  );

describe("time-series charts", () => {
  // Invariant 10: on a categorical axis, a missing day would draw its
  // neighbours as adjacent.
  it("draw every day of the range, an empty one as zero", () => {
    tokenChart();
    modelChart();
    const [tokens, models] = drawn;
    expect(tokens.data.map((r) => r.input_tokens)).toEqual([0, 0, 5, 0, 7, 0, 0]);
    expect(models.data.map((r) => r.b)).toEqual([0, 10, 0, 0, 0, 80, 0]);
  });

  // Stacked, the top line is a running total no series ever reached.
  it("never stack a series on another", () => {
    tokenChart();
    modelChart();
    const all = drawn.flatMap((c) => c.series);
    expect(all).toHaveLength(4);
    for (const s of all) expect(s.stackId).toBeUndefined();
  });

  // Ranked per point, a colour would change which model it means partway along.
  it("give each model the slot of its rank over the whole range", () => {
    modelChart();
    expect(drawn[0].series.map((s) => [s.dataKey, s.stroke, s.fill])).toEqual([
      ["a", MODEL_SLOTS[0], MODEL_SLOTS[0]],
      ["b", MODEL_SLOTS[1], MODEL_SLOTS[1]],
    ]);
  });

  // They redraw on every poll, so an entry animation would replay once a minute.
  it.each([true, false])("animate only without prefers-reduced-motion (reduced: %s)", (on) => {
    reducedMotion(on);
    tokenChart();
    modelChart();
    const all = drawn.flatMap((c) => c.series);
    expect(all).toHaveLength(4);
    for (const s of all) expect(s.isAnimationActive).toBe(!on);
  });

  // Gap-filling an empty list would draw a confident zero across the range.
  it("draw nothing, not a line at zero, for a range with no days", () => {
    render(<TokenChart days={[]} range={range} series={series} />);
    render(<ModelChart points={[]} range={range} />);
    expect(screen.getAllByText("No activity in this range.")).toHaveLength(2);
    expect(drawn).toHaveLength(0);
  });
});

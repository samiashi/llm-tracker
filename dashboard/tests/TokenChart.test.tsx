import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { TokenChart } from "@/components/TokenChart";

afterEach(cleanup);

describe("TokenChart", () => {
  it("draws no line for an empty day list", () => {
    render(
      <TokenChart
        days={[]}
        range={{ from: "2026-09-01", to: "2026-09-22" }}
        series={[{ key: "input_tokens", name: "Input", color: "var(--series-1)" }]}
      />,
    );
    expect(screen.getByText("No activity in this range.")).toBeTruthy();
    expect(document.querySelector('[aria-label*="per day"]')).toBeNull();
  });
});

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MatrixBars } from "@/components/MatrixBars";
import type { MatrixCell } from "@/api";

afterEach(cleanup);

const ORDER = ["default", "minimal", "low", "medium", "high", "xhigh", "ultracode", "max", "ultra"];

const cell = (row: string, col: string, tokens: number, extra: Partial<MatrixCell> = {}) => ({
  row,
  col,
  tokens,
  billed_usd: 1,
  rate_card_usd: 2,
  unknown_basis_usd: 0,
  unpriced_tokens: 0,
  ...extra,
});

const legend = () => [...document.querySelectorAll(".legend .pill")].map((e) => e.textContent);

describe("MatrixBars", () => {
  it("ranks an effort level where the scale puts it, whatever its case or spacing", () => {
    render(
      <MatrixBars
        colOrder={ORDER}
        cells={[cell("m", "low", 10), cell("m", "High", 10), cell("m", " ultra ", 10)]}
      />,
    );
    expect(legend()).toEqual(["low", "high", "ultra"]);
  });

  it("counts one level spelled two ways as one segment", () => {
    render(<MatrixBars colOrder={ORDER} cells={[cell("m", "high", 30), cell("m", "High", 10)]} />);
    expect(legend()).toEqual(["high"]);
    expect(document.querySelectorAll(".mseg")).toHaveLength(1);
  });

  it("states each model's split in text, with no segment to tab through", () => {
    render(
      <MatrixBars
        colOrder={ORDER}
        cells={[cell("opus", "high", 75), cell("opus", "low", 25), cell("sonnet", "medium", 40)]}
      />,
    );
    expect(screen.queryByRole("grid")).toBeNull();
    expect(document.querySelectorAll(".matrix [tabindex]")).toHaveLength(0);
    const rows = screen.getAllByRole("listitem");
    expect(rows.map((r) => r.querySelector(".msum")?.textContent)).toEqual([
      "low 25% · high 75%",
      "medium 100%",
    ]);
  });

  // A model missing from the price table is not free (invariant 8).
  it("says a model with no price is unpriced, not $0 billed", () => {
    const free = { billed_usd: 0, rate_card_usd: 0 };
    render(
      <MatrixBars
        colOrder={ORDER}
        cells={[
          cell("priced", "high", 10),
          cell("new-model", "high", 30, { ...free, unpriced_tokens: 20 }),
          cell("new-model", "low", 10, { ...free, unpriced_tokens: 10 }),
        ]}
      />,
    );
    const heads = [...document.querySelectorAll(".mrow .cost")].map((c) => c.textContent);
    expect(heads).toEqual(["unpriced", "$1.00 billed"]);
    const tip = document.querySelector(".mrow .mseg")?.getAttribute("title") ?? "";
    expect(tip).toContain("unpriced");
    expect(tip).not.toContain("$0");
  });

  it("keeps unknown-basis spend its own figure in the tooltip, and omits it when zero", () => {
    render(
      <MatrixBars
        colOrder={ORDER}
        cells={[cell("m", "high", 10, { unknown_basis_usd: 3 }), cell("m", "low", 10)]}
      />,
    );
    const title = (col: string) =>
      [...document.querySelectorAll(".mseg")]
        .map((s) => s.getAttribute("title") ?? "")
        .find((t) => t.includes(` ${col} `))!;
    expect(title("high")).toContain("$1.00 billed, $2.00 rate card, $3.00 basis unknown");
    expect(title("low")).not.toContain("basis unknown");
  });
});

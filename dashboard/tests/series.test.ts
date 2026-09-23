import { describe, expect, it } from "vitest";
import { calendarDays, fillDays } from "@/series";

describe("calendarDays", () => {
  it("includes both endpoints", () => {
    expect(calendarDays("2026-09-10", "2026-09-13")).toEqual([
      "2026-09-10",
      "2026-09-11",
      "2026-09-12",
      "2026-09-13",
    ]);
  });

  it("returns a single day when from equals to", () => {
    expect(calendarDays("2026-09-10", "2026-09-10")).toEqual(["2026-09-10"]);
  });

  it("crosses month and year boundaries", () => {
    expect(calendarDays("2026-12-30", "2027-01-02")).toEqual([
      "2026-12-30",
      "2026-12-31",
      "2027-01-01",
      "2027-01-02",
    ]);
  });

  it("handles a leap day", () => {
    expect(calendarDays("2028-02-27", "2028-03-01")).toEqual([
      "2028-02-27",
      "2028-02-28",
      "2028-02-29",
      "2028-03-01",
    ]);
  });

  // The Santiago cases bite only in that zone, which is why the suite runs there;
  // a US transition at 02:00 passes in every timezone.
  it("neither skips nor repeats a day across a midnight DST jump", () => {
    for (const [from, to, want] of [
      ["2026-09-05", "2026-09-07", 3], // Santiago, DST starts at 00:00
      ["2026-04-04", "2026-04-06", 3], // Santiago, DST ends at 00:00
      ["2026-09-01", "2026-09-10", 10],
      ["2027-03-12", "2027-03-16", 5], // US spring forward, 02:00
      ["2027-11-05", "2027-11-09", 5], // US fall back, 02:00
    ] as [string, string, number][]) {
      const days = calendarDays(from, to);
      expect(days, `${from}..${to}`).toHaveLength(want);
      expect(new Set(days).size).toBe(want);
      expect(days[days.length - 1]).toBe(to);
    }
  });

  it("is independent of the viewer timezone", () => {
    expect(calendarDays("2026-01-01", "2026-01-01")).toEqual(["2026-01-01"]);
    expect(calendarDays("2026-12-31", "2027-01-01")).toEqual(["2026-12-31", "2027-01-01"]);
  });

  it("returns empty for a malformed date rather than looping", () => {
    expect(calendarDays("not-a-date", "2026-01-02")).toEqual([]);
  });
});

describe("fillDays", () => {
  type Row = { day: string; n: number };
  const blank = (day: string): Row => ({ day, n: 0 });

  it("inserts the days the API omitted", () => {
    const rows: Row[] = [
      { day: "2026-09-10", n: 5 },
      { day: "2026-09-13", n: 7 },
    ];
    expect(fillDays(rows, (r) => r.day, blank)).toEqual([
      { day: "2026-09-10", n: 5 },
      { day: "2026-09-11", n: 0 },
      { day: "2026-09-12", n: 0 },
      { day: "2026-09-13", n: 7 },
    ]);
  });

  it("leaves a complete run untouched", () => {
    const rows: Row[] = [
      { day: "2026-09-10", n: 1 },
      { day: "2026-09-11", n: 2 },
    ];
    expect(fillDays(rows, (r) => r.day, blank)).toEqual(rows);
  });

  it("sorts rows that arrive out of order", () => {
    const rows: Row[] = [
      { day: "2026-09-13", n: 7 },
      { day: "2026-09-10", n: 5 },
    ];
    expect(fillDays(rows, (r) => r.day, blank).map((r) => r.day)).toEqual([
      "2026-09-10",
      "2026-09-11",
      "2026-09-12",
      "2026-09-13",
    ]);
  });

  it("does not pad beyond the data it was given", () => {
    const rows: Row[] = [{ day: "2026-09-10", n: 5 }];
    expect(fillDays(rows, (r) => r.day, blank)).toEqual(rows);
  });

  it("returns empty for empty", () => {
    expect(fillDays([] as Row[], (r) => r.day, blank)).toEqual([]);
  });

  it("does not mutate its input", () => {
    const rows: Row[] = [
      { day: "2026-09-13", n: 7 },
      { day: "2026-09-10", n: 5 },
    ];
    const copy = structuredClone(rows);
    fillDays(rows, (r) => r.day, blank);
    expect(rows).toEqual(copy);
  });
});

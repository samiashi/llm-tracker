import { describe, expect, it } from "vitest";
import { calendarHours, hourLabel } from "@/series";
import { browserTimeZone, localTimestamp } from "@/timezone";
import { change } from "@/dashboard";

describe("viewer timezone", () => {
  it("detects the runtime zone instead of hard-coding UTC", () => {
    expect(browserTimeZone()).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
  });
  it("formats instants in Dubai, including the date across midnight", () => {
    expect(hourLabel("2026-10-27T22:00:00Z", "Asia/Dubai")).toBe("10-28 02:00 UTC+04:00");
    expect(localTimestamp("invalid", "Asia/Dubai")).toBe("—");
  });
  it.each([
    ["Asia/Kolkata", "2026-10-27T07:30:00Z"],
    ["Asia/Kathmandu", "2026-10-27T08:15:00Z"],
  ])("aligns hours in %s", (zone, first) => {
    const start = Date.parse("2026-10-27T08:15:00Z") / 1000;
    expect(calendarHours(start, start + 86400, zone)[0]).toBe(first);
  });
  it("keeps repeated fall-back hours distinct and skips spring's nonexistent hour", () => {
    const fall = Date.parse("2026-11-01T00:00:00Z") / 1000;
    const keys = calendarHours(fall, fall + 86400, "America/New_York");
    const labels = keys.map((key) => hourLabel(key, "America/New_York"));
    expect(labels).toContain("11-01 01:00 UTC-04:00");
    expect(labels).toContain("11-01 01:00 UTC-05:00");
    expect(new Set(labels).size).toBe(labels.length);
    const spring = Date.parse("2026-03-08T00:00:00Z") / 1000;
    expect(
      calendarHours(spring, spring + 86400, "America/New_York")
        .map((key) => hourLabel(key, "America/New_York"))
        .some((label) => label.startsWith("03-08 02:")),
    ).toBe(false);
  });
  it("represents a partial hour at Lord Howe's half-hour jump", () => {
    const start = Date.parse("2026-10-03T15:29:00Z") / 1000;
    const keys = calendarHours(start, start + 86400, "Australia/Lord_Howe");
    expect(keys.slice(0, 3)).toEqual([
      "2026-10-03T14:30:00Z",
      "2026-10-03T15:30:00Z",
      "2026-10-03T16:00:00Z",
    ]);
    expect(hourLabel(keys[1], "Australia/Lord_Howe")).toBe("10-04 02:30 UTC+11:00");
  });
  it("localizes comparison instants without changing the totals calculation", () => {
    const result = change(
      20,
      10,
      {
        previous: {} as never,
        previous_from: "2026-10-25T08:00:00Z",
        previous_to: "2026-10-26T08:00:00Z",
      },
      "2026-01-01",
      "Asia/Dubai",
    );
    expect(result).toEqual({
      pct: 100,
      window: "2026-10-25 12:00 UTC+04:00 → 2026-10-26 12:00 UTC+04:00",
    });
  });
});

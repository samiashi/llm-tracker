import { afterEach, describe, expect, test } from "vitest";
import { calendarDays } from "@/series";
import { intentFromURL, isoAt, MAX_DAYS, resolve } from "@/window";

// These run under America/Santiago and UTC. A Santiago evening is already the
// next UTC day, and its DST changes at midnight, so local day arithmetic fails
// there while passing in UTC.

const utcDay = (ms: number) => new Date(ms).toISOString().slice(0, 10);

describe("isoAt counts UTC days, as the server stores them", () => {
  test("an evening west of UTC is already the next UTC day", () => {
    // 22:30 on 22 September in Santiago.
    expect(isoAt(Date.UTC(2026, 8, 23, 1, 30))).toBe("2026-09-23");
  });

  test("today is the UTC day of now at every hour", () => {
    for (let h = 0; h < 24; h++) {
      expect(isoAt(Date.UTC(2026, 8, 22, h, 30)), `at ${h}:30 UTC`).toBe("2026-09-22");
    }
  });

  test("steps back whole days across a midnight DST change and year ends", () => {
    // Santiago springs forward at local midnight on 6 September 2026.
    expect(isoAt(Date.UTC(2026, 8, 6, 4, 30), 1)).toBe("2026-09-05");
    expect(isoAt(Date.UTC(2026, 8, 7, 3, 30), 2)).toBe("2026-09-05");
    expect(isoAt(Date.UTC(2027, 0, 1, 0, 30), 1)).toBe("2026-12-31");
    expect(isoAt(Date.UTC(2028, 2, 1, 12), 1)).toBe("2028-02-29");
  });
});

describe("resolve", () => {
  test("a preset ends on the UTC day of now at any local hour, and is its width", () => {
    for (let h = 0; h < 24; h++) {
      const now = Date.UTC(2026, 8, 22, h, 30);
      const f = resolve({ preset: 7 }, now);
      expect(f.to, `at ${h}:30 UTC`).toBe(utcDay(now));
      expect(calendarDays(f.from, f.to)).toHaveLength(7);
    }
  });

  // Each of these would reach the date arithmetic and throw during render.
  test("an absurd preset cannot produce an invalid date", () => {
    const now = Date.now();
    for (const preset of [Infinity, 1e21, 200_000_000]) {
      expect(() => resolve({ preset }, now)).not.toThrow();
    }
  });
});

describe("every range is ordered and bounded", () => {
  const now = Date.UTC(2026, 8, 23, 12);

  test("a start in year one is clamped to the widest preset", () => {
    const f = resolve({ from: "0001-01-01", to: "2026-09-22" }, now);
    expect(f.from).toBe(isoAt(now, MAX_DAYS - 1));
    expect(calendarDays(f.from, f.to).length).toBeLessThanOrEqual(MAX_DAYS);
  });

  test("an end in the future is clamped to today", () => {
    expect(resolve({ from: "2026-09-01", to: "2099-01-01" }, now).to).toBe("2026-09-23");
  });

  test("a reversed pair is put in order rather than sent", () => {
    const f = resolve({ from: "2026-09-22", to: "2026-09-01" }, now);
    expect([f.from, f.to]).toEqual(["2026-09-01", "2026-09-22"]);
  });
});

describe("an emptied date box", () => {
  const now = Date.UTC(2026, 8, 22, 12);

  test("falls back to the default window rather than sending a blank", () => {
    const w = resolve({ from: "", to: "" }, now);
    expect(w.to).toBe(isoAt(now));
    expect(w.from).toBe(isoAt(now, 29));
  });

  test("keeps an explicit range untouched", () => {
    const w = resolve({ from: "2026-09-01", to: "2026-09-15" }, now);
    expect([w.from, w.to]).toEqual(["2026-09-01", "2026-09-15"]);
  });
});

describe("a shared link opens a range the page can draw", () => {
  const now = Date.UTC(2026, 8, 23, 12);
  const open = (search: string) => {
    window.history.replaceState(null, "", `/${search}`);
    return intentFromURL(now);
  };
  afterEach(() => window.history.replaceState(null, "", "/"));

  test("a reversed range opens in order, as typing one does", () => {
    expect(open("?from=2026-09-22&to=2026-09-01")).toMatchObject({
      from: "2026-09-01",
      to: "2026-09-22",
    });
  });

  test("a start in year one opens bounded", () => {
    expect(open("?from=0001-01-01&to=2026-09-22").from).toBe(isoAt(now, MAX_DAYS - 1));
  });

  test.each(["2026-04-31", "2026-02-29", "2026-02-30", "2026-13-01"])(
    "a day that does not exist (%s) falls back to the default window",
    (day) => {
      expect(open(`?from=${day}&to=2026-05-05`)).toEqual({ preset: 30, person: undefined });
    },
  );

  test("a leap day that exists is kept", () => {
    expect(open("?from=2024-02-29&to=2024-03-01")).toMatchObject({ from: "2024-02-29" });
  });
});

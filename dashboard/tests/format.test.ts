import { describe, expect, it, test } from "vitest";
import {
  agentHealth,
  ago,
  bytes,
  exact,
  fleetVersion,
  isRelease,
  shortDay,
  since,
  spansYears,
  tokens,
  usd,
  utcMidnightAt,
  utcOffset,
  versionState,
} from "@/format";

describe("tokens", () => {
  it("promotes the unit when rounding crosses a boundary", () => {
    expect(tokens(999_999)).toBe("1.0M");
    expect(tokens(999_500)).toBe("1.0M");
    expect(tokens(999_999_999)).toBe("1.0B");
    expect(tokens(999_999_999_999)).toBe("1.0T");
  });

  it("formats each magnitude", () => {
    expect(tokens(0)).toBe("0");
    expect(tokens(999)).toBe("999");
    expect(tokens(1_000)).toBe("1.0K");
    expect(tokens(12_345)).toBe("12K");
    expect(tokens(1_500_000)).toBe("1.5M");
    expect(tokens(22_500_000_000)).toBe("23B");
  });

  it("never emits a four-digit mantissa", () => {
    for (let e = 3; e <= 14; e++) {
      for (const n of [10 ** e - 1, 10 ** e, 10 ** e + 1]) {
        const m = tokens(n).match(/^-?([\d.]+)/);
        expect(Number(m![1]), `tokens(${n}) = ${tokens(n)}`).toBeLessThan(1000);
      }
    }
  });

  it("handles non-finite and negative input", () => {
    expect(tokens(NaN)).toBe("—");
    expect(tokens(Infinity)).toBe("—");
    expect(tokens(-1_500_000)).toBe("-1.5M");
  });
});

describe("usd", () => {
  it("distinguishes a small negative from a small positive", () => {
    expect(usd(0)).toBe("$0");
    expect(usd(0.004)).toBe("<$0.01");
    expect(usd(-0.004)).toBe("-<$0.01");
  });

  it("does not print NaN as a price", () => {
    expect(usd(NaN)).toBe("—");
  });
});

describe("agentHealth", () => {
  const now = 1_790_000_000;
  const agoMins = (m: number) => now - m * 60;

  // A column that turns yellow on every missed interval is one nobody reads.
  it("tolerates a few missed intervals", () => {
    expect(agentHealth(agoMins(4), now).tone).toBe("ok");
    expect(agentHealth(agoMins(29), now).tone).toBe("ok");
  });

  it("calls an overnight gap quiet, not offline", () => {
    expect(agentHealth(agoMins(45), now).tone).toBe("quiet");
    expect(agentHealth(agoMins(60 * 14), now).tone).toBe("quiet");
  });

  it("calls a day of silence offline", () => {
    expect(agentHealth(agoMins(60 * 25), now).tone).toBe("offline");
  });

  // A machine row can exist before its first push lands.
  it("treats never-synced as offline rather than as just-synced", () => {
    expect(agentHealth(0, now).tone).toBe("offline");
  });
});

describe("ago", () => {
  const now = 1_790_000_000;

  it("reads its argument as seconds, not milliseconds", () => {
    expect(ago(now - 120, now)).toBe("2m ago");
    expect(since(now, now)).toBe("just now");
    // The same value through the wrong helper stays absurd, so the mix-up shows.
    const wrong = since(now, now * 1000);
    expect(wrong).not.toBe("just now");
    expect(Number.parseInt(wrong, 10)).toBeGreaterThan(100_000);
  });

  it("never renders a missing timestamp as a date in 1970", () => {
    expect(ago(0, now)).toBe("never");
  });

  it("scales past a day", () => {
    expect(ago(now - 60 * 60 * 5, now)).toBe("5h ago");
    expect(ago(now - 86400 * 3, now)).toBe("3d ago");
  });
});

describe("fleetVersion", () => {
  it("picks the version most machines are on", () => {
    expect(fleetVersion(["v1.2.0", "v1.2.0", "v1.1.0"])).toBe("v1.2.0");
  });

  // A machine that has never pushed has no version yet. Letting "" win would
  // mark every real agent as the odd one out.
  it("ignores machines with no version reported", () => {
    expect(fleetVersion(["", "", "v1.2.0"])).toBe("v1.2.0");
    expect(fleetVersion([])).toBe("");
  });
});

describe("versionState", () => {
  it("compares release tags numerically, not as strings", () => {
    // "v1.10.0" sorts before "v1.9.0" as text, which would report the newer
    // agent as the stale one.
    expect(versionState("v1.9.0", "v1.10.0")).toBe("behind");
    expect(versionState("v1.10.0", "v1.9.0")).toBe("ok");
    expect(versionState("v1.2.3", "v1.2.3")).toBe("ok");
    expect(versionState("v1.2.3", "v2.0.0")).toBe("behind");
  });

  // An agent ahead of the server is odd but not a problem to chase: it still
  // collects at least as much as the server expects.
  it("does not flag an agent ahead of the server", () => {
    expect(versionState("v2.0.0", "v1.9.9")).toBe("ok");
  });

  it("says differs rather than guessing at non-semver builds", () => {
    expect(versionState("c7707a3-dirty", "v1.2.0")).toBe("differs");
    expect(versionState("v1.2.0", "c7707a3-dirty")).toBe("differs");
  });

  // A machine that has never pushed has no version yet; marking it is noise.
  it("stays quiet when either side is unknown", () => {
    expect(versionState("", "v1.2.0")).toBe("ok");
    expect(versionState("v1.2.0", "")).toBe("ok");
  });
});

describe("isRelease", () => {
  it("accepts release tags", () => {
    expect(isRelease("v1.4.0")).toBe(true);
    expect(isRelease("1.4.0")).toBe(true);
  });

  it("rejects anything git describe produced", () => {
    expect(isRelease("dev")).toBe(false);
    expect(isRelease("6387414-dirty")).toBe(false);
    expect(isRelease("v1.4.0-2-gabc123")).toBe(false);
    expect(isRelease("")).toBe(false);
  });
});

describe("formatters are total", () => {
  test("exact survives anything the server might not send", () => {
    for (const v of [undefined, null, NaN, Infinity, -Infinity]) {
      expect(() => exact(v as unknown as number)).not.toThrow();
      expect(exact(v as unknown as number)).toBe("—");
    }
    expect(exact(1234567)).toBe("1,234,567");
  });

  test("bytes survives the same", () => {
    for (const v of [undefined, null, NaN, Infinity]) {
      expect(() => bytes(v as unknown as number)).not.toThrow();
      expect(bytes(v as unknown as number)).toBe("—");
    }
    expect(bytes(2048)).toBe("2.0KB");
  });

  test("shortDay survives a malformed day", () => {
    for (const v of [undefined, null, "", "garbage"]) {
      expect(() => shortDay(v as unknown as string)).not.toThrow();
      expect(shortDay(v as unknown as string)).toBe("—");
    }
    expect(shortDay("2026-09-22")).toBe("09/22");
  });

  test("shortDay can disambiguate years", () => {
    expect(shortDay("2026-09-22", true)).toBe("09/22/26");
    expect(shortDay("2026-09-22", true)).not.toBe(shortDay("2027-09-22", true));
  });
});

describe("a clock ahead of the server is reported, not smoothed over", () => {
  const now = 1_800_000_000;

  test("ago names a future timestamp", () => {
    expect(ago(now + 600, now)).toBe("clock ahead");
    // Milliseconds where seconds were expected land far in the future.
    expect(ago(now * 1000, now)).toBe("clock ahead");
  });

  test("ago still reads normally just either side of now", () => {
    expect(ago(now, now)).toBe("0m ago");
    expect(ago(now - 120, now)).toBe("2m ago");
  });

  test("agentHealth does not call a skewed clock healthy", () => {
    expect(agentHealth(now + 86_400 * 30, now).tone).toBe("skewed");
    expect(agentHealth(now * 1000, now).tone).toBe("skewed");
    // The real states are unchanged.
    expect(agentHealth(now - 60, now).tone).toBe("ok");
    expect(agentHealth(now - 3600, now).tone).toBe("quiet");
    expect(agentHealth(now - 86_400 * 3, now).tone).toBe("offline");
    expect(agentHealth(0, now).tone).toBe("offline");
  });
});

describe("spansYears", () => {
  test("is true only when the range crosses a year boundary", () => {
    expect(spansYears("2026-01-01", "2026-12-31")).toBe(false);
    expect(spansYears("2025-12-31", "2026-01-01")).toBe(true);
    expect(spansYears("2016-09-25", "2026-09-22")).toBe(true);
  });

  test("is false when either end is missing", () => {
    expect(spansYears(undefined, "2026-09-22")).toBe(false);
    expect(spansYears("2026-09-22", undefined)).toBe(false);
    expect(spansYears(undefined, undefined)).toBe(false);
  });

  test("drives shortDay's year suffix", () => {
    expect(shortDay("2026-09-22")).toBe("09/22");
    expect(shortDay("2026-09-22", true)).toBe("09/22/26");
  });
});

describe("time zones", () => {
  test.each([
    [0, "UTC"],
    [240, "UTC+4"],
    [330, "UTC+5:30"],
    [-180, "UTC−3"],
  ])("labels %i minutes east as %s", (minutes, label) => {
    expect(utcOffset(minutes)).toBe(label);
  });

  // Dubai's day starts at 04:00; west of UTC it starts the evening before.
  test.each([
    [0, "00:00"],
    [240, "04:00"],
    [330, "05:30"],
    [-180, "21:00"],
  ])("puts a UTC midnight %i minutes east at %s", (minutes, time) => {
    expect(utcMidnightAt(minutes)).toBe(time);
  });
});

import { describe, expect, it } from "vitest";
import { describeFailure, HttpError } from "@/api";

describe("describeFailure", () => {
  it("calls a request that got no answer unreachable", () => {
    expect(describeFailure(new TypeError("Failed to fetch")).title).toBe(
      "Cannot reach the server.",
    );
    expect(describeFailure(new HttpError("/v1/summary", 502, "")).title).toBe(
      "Cannot reach the server.",
    );
  });

  it("reports a rejected range with the server's own reason", () => {
    const f = describeFailure(new HttpError("/v1/summary", 400, "from is after to"));
    expect(f.title).toBe("That date range isn't valid.");
    expect(f.detail).toBe("from is after to");
  });

  it("reports a server error as one, not as an outage", () => {
    const f = describeFailure(new HttpError("/v1/summary", 500, "database is locked"));
    expect(f.title).toBe("The server hit an error.");
    expect(f.detail).toBe("500: database is locked");
  });

  it("names a timeout as silence rather than refusal", () => {
    const e = new DOMException("signal timed out", "TimeoutError");
    expect(describeFailure(e).title).toBe("The server did not answer.");
  });
});

import { describe, expect, it } from "vitest";
import type { api } from "@/api";
import agents from "./fixtures/api/agents.json";
import breakdown from "./fixtures/api/breakdown.json";
import compare from "./fixtures/api/compare.json";
import daily from "./fixtures/api/daily.json";
import dailyModel from "./fixtures/api/daily-model.json";
import health from "./fixtures/api/health.json";
import heatmap from "./fixtures/api/heatmap.json";
import matrix from "./fixtures/api/matrix.json";
import sessions from "./fixtures/api/sessions.json";
import summary from "./fixtures/api/summary.json";
import unknown from "./fixtures/api/unknown.json";

/**
 * The dashboard half of the server contract. Each fixture is a real response,
 * written by the Go test TestDashboardContract; `satisfies` checks it against
 * what api.ts promises for that call, so a field api.ts reads that the server
 * does not send -- or sends as another type -- fails `tsc -b`. Vitest strips
 * types, so the assertion below only proves the fixtures load.
 */
type Call = Exclude<keyof typeof api, "exportURL">;
type Reply<K extends Call> = Awaited<ReturnType<(typeof api)[K]>>;

const replies = {
  summary: summary satisfies Reply<"summary">,
  daily: daily satisfies Reply<"daily">,
  dailyByModel: dailyModel satisfies Reply<"dailyByModel">,
  breakdown: breakdown satisfies Reply<"breakdown">,
  heatmap: heatmap satisfies Reply<"heatmap">,
  topSessions: sessions satisfies Reply<"topSessions">,
  health: health satisfies Reply<"health">,
  agents: agents satisfies Reply<"agents">,
  unknown: unknown satisfies Reply<"unknown">,
  compare: compare satisfies Reply<"compare">,
  matrix: matrix satisfies Reply<"matrix">,
  // Keyed by every call api.ts makes, so a new call without a fixture -- and
  // so without the Go test serving it -- fails the type check too.
} satisfies Record<Call, unknown>;

describe("server contract", () => {
  it("has a server-generated fixture for every call the dashboard makes", () => {
    for (const reply of Object.values(replies)) expect(reply).toBeTruthy();
  });
});

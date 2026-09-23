import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import type { AgentRow, SessionRow } from "@/api";
import { AgentsTable, SessionsTable } from "@/components/tables";

afterEach(cleanup);

const agent = (id: string, version: string, release: string): AgentRow => ({
  machine_id: id,
  hostname: id,
  person: "a@b.c",
  agent_version: version,
  release,
  last_sync: 1,
  events: 1,
});

const tags = () =>
  [...document.querySelectorAll("tbody tr")].map((r) => r.querySelector(".tag")?.textContent ?? "");

describe("AgentsTable", () => {
  // Right after a release every agent lags the server; measured against the
  // fleet's own majority instead, none of them would be marked.
  it("marks every agent behind the server's release, even when all are", () => {
    render(
      <AgentsTable
        now={2}
        server={{ version: "v1.4.0", release: "1.4.0" }}
        agents={[agent("m1", "v1.3.0", "1.3.0"), agent("m2", "v1.3.0", "1.3.0")]}
      />,
    );
    expect(tags()).toEqual(["behind", "behind"]);
  });

  // A server built from a working tree is no upgrade target; the machine
  // that differs from the rest of the fleet is the one to look at.
  it("measures against the fleet's majority when the server is not a release", () => {
    render(
      <AgentsTable
        now={2}
        server={{ version: "6387414-dirty", release: "" }}
        agents={[
          agent("m1", "v1.4.0", "1.4.0"),
          agent("m2", "v1.4.0", "1.4.0"),
          agent("m3", "v1.3.0", "1.3.0"),
        ]}
      />,
    );
    expect(tags()).toEqual(["", "", "behind"]);
  });
});

describe("SessionsTable", () => {
  const session = (id: string, extra: Partial<SessionRow>): SessionRow => ({
    session_id: id,
    source: "codex",
    model: "gpt-9",
    effort: "",
    email: "a@b.c",
    tokens: 2_000_000,
    billed_usd: 0,
    rate_card_usd: 0,
    unknown_basis_usd: 0,
    unpriced_tokens: 0,
    events: 1,
    last_seen: 1,
    ...extra,
  });
  const costs = (row: number) =>
    [...document.querySelectorAll("tbody tr")[row].querySelectorAll("td")]
      .slice(5, 7)
      .map((td) => td.textContent);

  // A model missing from the price table is not free (invariant 8).
  it("says a session on a model with no price is unpriced, not $0", () => {
    render(
      <SessionsTable
        now={2}
        sessions={[
          session("priced", { billed_usd: 3 }),
          session("new-model", { unpriced_tokens: 2_000_000 }),
        ]}
      />,
    );
    expect(costs(0)).toEqual(["$3.00", "$0"]);
    expect(costs(1)).toEqual(["unpriced", "unpriced"]);
  });
});

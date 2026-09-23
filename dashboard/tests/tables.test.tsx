import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";
import type { AgentRow } from "@/api";
import { AgentsTable } from "@/components/tables";

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

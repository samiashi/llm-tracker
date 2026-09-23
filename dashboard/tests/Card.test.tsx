import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { Group, SessionRow } from "@/api";
import { BarList } from "@/components/BarList";
import { Card, FAILED } from "@/components/Card";
import { SessionsTable } from "@/components/tables";

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", "/");
});

// Twelve rows, four more than a list shows in the grid.
const models = Array.from({ length: 12 }, (_, i) => ({
  key: `model-${i + 1}`,
  label: "",
  totals: { total_tokens: 1200 - i * 100 },
})) as unknown as Group[];

function ByModel() {
  return (
    <Card title="By model">
      {(v) => (
        <BarList
          groups={models}
          color="red"
          value={(g) => g.totals.total_tokens}
          format={String}
          max={v.expanded ? Infinity : undefined}
          onMore={v.open}
        />
      )}
    </Card>
  );
}

const expanded = () => screen.getByRole("dialog", { name: "By model" });
/** Opening re-renders through the URL store; wait for it rather than assume it. */
const opened = () => screen.findByRole("dialog", { name: "By model" });

describe("expanding a card", () => {
  const gone = () => waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  it("shows every row the grid leaves out", async () => {
    render(<ByModel />);
    expect(screen.queryByText("model-12")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Expand By model" }));
    await opened();
    expect(within(expanded()).getByText("model-12")).toBeTruthy();
    expect(within(expanded()).queryByText(/more/)).toBeNull();
  });

  it("opens from the line that says there is more", async () => {
    render(<ByModel />);
    fireEvent.click(screen.getByRole("button", { name: /^\+4 more/ }));
    expect(within(await opened()).getByText("model-12")).toBeTruthy();
  });

  it("returns focus to the expand button when closed", async () => {
    render(<ByModel />);
    const expand = screen.getByRole("button", { name: "Expand By model" });
    fireEvent.click(expand);
    await opened();
    fireEvent.click(within(expanded()).getByRole("button", { name: "Close" }));
    await gone();
    expect(document.activeElement).toBe(expand);
  });

  it("closes on a click on the backdrop, and only there", async () => {
    render(<ByModel />);
    fireEvent.click(screen.getByRole("button", { name: "Expand By model" }));
    await opened();
    const row = within(expanded()).getByText("model-3");
    fireEvent.pointerDown(row);
    fireEvent.click(row);
    // A selection dragged out of the panel ends in a click on the dialog.
    fireEvent.pointerDown(row);
    fireEvent.click(expanded());
    expect(screen.queryByRole("dialog")).not.toBeNull();
    // The panel fills the dialog, so only the backdrop hits the dialog itself.
    fireEvent.pointerDown(expanded());
    fireEvent.click(expanded());
    await gone();
  });

  it("names the open card in the URL, and takes it out on close", async () => {
    render(<ByModel />);
    fireEvent.click(screen.getByRole("button", { name: "Expand By model" }));
    await opened();
    expect(new URLSearchParams(window.location.search).get("card")).toBe("by-model");
    fireEvent.click(within(expanded()).getByRole("button", { name: "Close" }));
    // Closing pops a history entry, which lands a task later than the dialog
    // stops being visible: wait on the URL itself.
    await waitFor(() => expect(new URLSearchParams(window.location.search).get("card")).toBeNull());
    await gone();
  });

  it("closes on Back, since that is how people leave a full-screen view", async () => {
    render(<ByModel />);
    fireEvent.click(screen.getByRole("button", { name: "Expand By model" }));
    await opened();
    window.history.back();
    await gone();
  });

  it("opens from a link that names the card", async () => {
    window.history.replaceState(null, "", "/?preset=30&card=by-model");
    render(<ByModel />);
    expect(within(await opened()).getByText("model-12")).toBeTruthy();
    // Nothing was pushed to pop, so closing edits the URL in place instead.
    fireEvent.click(within(expanded()).getByRole("button", { name: "Close" }));
    await gone();
    expect(window.location.search).toBe("?preset=30");
  });

  it("is offered only where there is more to show", () => {
    render(<Card title="Where the tokens go">static</Card>);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("is not offered while the card's request has failed", () => {
    render(
      <Card title="By model" failed>
        {() => "rows"}
      </Card>,
    );
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText(FAILED)).toBeTruthy();
  });
});

describe("SessionsTable", () => {
  const sessions = Array.from({ length: 10 }, (_, i) => ({
    session_id: `s${i}`,
    source: "codex",
    model: "gpt-5",
    effort: "",
    email: "",
    tokens: 1000 - i,
    billed_usd: 0,
    rate_card_usd: 1,
    events: 1,
    last_seen: 0,
  })) as SessionRow[];

  it("shows the first rows and says how many more of the top N there are", () => {
    render(<SessionsTable sessions={sessions} now={0} max={8} onMore={() => {}} />);
    // A header row plus eight.
    expect(screen.getAllByRole("row")).toHaveLength(9);
    expect(screen.getByRole("button", { name: "+2 more of the top 10" })).toBeTruthy();
  });

  it("shows them all when not limited", () => {
    render(<SessionsTable sessions={sessions} now={0} max={Infinity} />);
    expect(screen.getAllByRole("row")).toHaveLength(11);
    expect(screen.queryByText(/more of the top/)).toBeNull();
  });
});

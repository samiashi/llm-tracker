import type { AgentRow, SessionRow, SourceHealth, UnknownRow } from "@/api";
import { More } from "@/components/Card";
import { agentHealth, fleetVersion, isRelease, versionState } from "@/fleet";
import { ago, bytes, exact, tokens, usd } from "@/format";

export function SessionsTable({
  sessions,
  now,
  max = 8,
  onMore,
}: {
  sessions: SessionRow[];
  now: number;
  max?: number;
  /** Shows the hidden rows, making the "+N more" line a button. */
  onMore?: () => void;
}) {
  if (sessions.length === 0) return <p className="empty">No sessions in this range.</p>;
  const shown = sessions.slice(0, max);
  const hidden = sessions.length - shown.length;
  // Its own column, shown only when there is any: never folded into billed or rate card.
  const unknownBasis = shown.some((s) => s.unknown_basis_usd > 0);
  return (
    <div className="scroll">
      <table>
        <thead>
          <tr>
            <th>Harness</th>
            <th>Model</th>
            <th>Effort</th>
            <th>Person</th>
            <th className="num">Tokens</th>
            <th className="num" title="Metered spend on a pay-as-you-go key.">
              Billed
            </th>
            <th
              className="num"
              title="What seat usage would have cost at list price. Not spend, and never added to it."
            >
              Rate card
            </th>
            {unknownBasis && (
              <th
                className="num"
                title="Priced at list rates, but whether it was metered or a seat is unknown. Never added to either."
              >
                Basis unknown
              </th>
            )}
            <th className="num">Responses</th>
            <th className="num">Last seen</th>
          </tr>
        </thead>
        <tbody>
          {shown.map((s) => (
            <tr key={s.session_id + s.source}>
              <td>{s.source}</td>
              <td>{s.model || "—"}</td>
              <td>{s.effort ? <span className="tag todo">{s.effort}</span> : "—"}</td>
              <td>{s.email || "—"}</td>
              <td className="num">{tokens(s.tokens)}</td>
              <td className="num">{usd(s.billed_usd)}</td>
              <td className="num">{usd(s.rate_card_usd)}</td>
              {unknownBasis && <td className="num">{usd(s.unknown_basis_usd)}</td>}
              <td className="num">{exact(s.events)}</td>
              <td className="num">{ago(s.last_seen, now)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {/* Of the top N only: the server returns no more than that. */}
      {hidden > 0 && (
        <More onMore={onMore}>
          +{hidden} more of the top {sessions.length}
        </More>
      )}
    </div>
  );
}

export function HealthTable({ sources, now }: { sources: SourceHealth[]; now: number }) {
  if (sources.length === 0) return <p className="empty">No collector has reported yet.</p>;
  return (
    <table>
      <thead>
        <tr>
          <th>Source</th>
          <th className="num">Events</th>
          <th className="num">Last seen</th>
        </tr>
      </thead>
      <tbody>
        {sources.map((h) => (
          <tr key={h.source + h.machine_id}>
            <td>{h.source}</td>
            <td className="num">{exact(h.events)}</td>
            <td className="num">{ago(h.last_event, now)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function AgentsTable({
  agents,
  now,
  serverVersion,
}: {
  agents: AgentRow[];
  now: number;
  serverVersion: string;
}) {
  // Both halves ship from one tag, so the server's version is the upgrade
  // target. A working-tree build is not installable: the fleet's most common
  // version stands in, which flags the odd machine out.
  const serverIsRelease = isRelease(serverVersion);
  const target = serverIsRelease ? serverVersion : fleetVersion(agents.map((a) => a.agent_version));
  const targetLabel = serverIsRelease ? "The server" : "Most of the fleet";
  return (
    <div className="scroll">
      <table>
        <thead>
          <tr>
            <th>Person</th>
            <th>Machine</th>
            <th>Version</th>
            <th className="num">Last sync</th>
            <th className="num">Events</th>
          </tr>
        </thead>
        <tbody>
          {agents.map((a) => {
            const health = agentHealth(a.last_sync, now);
            const state = versionState(a.agent_version, target);
            return (
              <tr key={a.machine_id}>
                <td>
                  <span
                    className={`status-dot ${health.tone}`}
                    role="img"
                    aria-label={health.label}
                    title={health.label}
                  />{" "}
                  {a.person || "unattributed"}
                </td>
                <td>{a.hostname || a.machine_id.slice(0, 12)}</td>
                <td>
                  {a.agent_version || "—"}
                  {state !== "ok" && (
                    <span
                      className="tag todo"
                      title={
                        state === "behind"
                          ? `${targetLabel} is on ${target}; this agent collects with an older adapter set`
                          : `${targetLabel} is on ${target}; this agent is on something else, and which is newer cannot be determined`
                      }
                    >
                      {state}
                    </span>
                  )}
                </td>
                <td className="num" title={health.label}>
                  {ago(a.last_sync, now)}
                </td>
                <td className="num">{exact(a.events)}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function UnknownTable({ rows, now }: { rows: UnknownRow[]; now: number }) {
  return (
    <div className="scroll">
      <table>
        <thead>
          <tr>
            <th>Harness</th>
            <th>Status</th>
            <th>Path</th>
            <th className="num">Size</th>
            <th className="num">Seen</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((u) => (
            <tr key={u.machine_id + u.path}>
              <td>
                {u.hint || "unknown"}
                {u.note && <div className="rownote">{u.note}</div>}
              </td>
              <td>
                <span className={`tag ${u.status === "blocked" ? "blocked" : "todo"}`}>
                  {u.status === "blocked" ? "blocked" : "todo"}
                </span>
              </td>
              <td className="pathcell">{u.path.replace(/^\/Users\/[^/]+/, "~")}</td>
              <td className="num">{bytes(u.size_bytes)}</td>
              <td className="num">{ago(u.last_seen, now)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// The collector fleet: whether each agent is checking in, and whether it runs
// the release it should.

/**
 * How healthy an agent's check-in is. The collector pushes every five minutes:
 * `ok` within 30 minutes (missed intervals mean nothing), `quiet` within a day
 * (a machine asleep), `offline` beyond. A time in the future is `skewed`,
 * checked first so a fast clock never reads as the healthiest row.
 * `now` is the server's clock, so a skewed browser cannot flag the whole team.
 */
export function agentHealth(
  lastSync: number,
  now: number,
): { tone: "ok" | "quiet" | "offline" | "skewed"; label: string } {
  if (!Number.isFinite(lastSync) || !lastSync) {
    return { tone: "offline", label: "has never reported in" };
  }
  const mins = (now - lastSync) / 60;
  if (mins < -1) {
    return {
      tone: "skewed",
      label: "reported a time in the future — this machine's clock is wrong",
    };
  }
  if (mins <= 30) return { tone: "ok", label: "reporting normally" };
  if (mins <= 60 * 24) return { tone: "quiet", label: "quiet — machine asleep or shut down" };
  return { tone: "offline", label: "no check-in for over a day — the collector is not running" };
}

/**
 * A build's version string and the release the server read it as: "X.Y.Z",
 * or "" for a working-tree build ("6387414-dirty", "dev"), which cannot be
 * installed. The server owns that grammar; the page only compares its answers.
 */
export type Version = { version: string; release: string };

/**
 * The version most of the fleet runs: the upgrade target when the server's
 * own build is not a release. "Differs from the rest" is a fact, where
 * ordering `git describe` output would be a guess.
 */
export function fleetVersion(versions: Version[]): Version {
  const counts = new Map<string, { v: Version; n: number }>();
  for (const v of versions) {
    if (!v.version) continue;
    const seen = counts.get(v.version);
    if (seen) seen.n++;
    else counts.set(v.version, { v, n: 1 });
  }
  let best: { v: Version; n: number } = { v: { version: "", release: "" }, n: 0 };
  for (const c of counts.values()) if (c.n > best.n) best = c;
  return best.v;
}

/**
 * An agent's version against the release it should run: compared as
 * releases where both are one, and otherwise "differs", never a guess at
 * which came first.
 */
export function versionState(agent: Version, target: Version): "ok" | "behind" | "differs" {
  if (!agent.version || !target.version || agent.version === target.version) return "ok";
  if (!agent.release || !target.release) return "differs";
  return compareReleases(agent.release, target.release) < 0 ? "behind" : "ok";
}

/** Two "X.Y.Z" releases, part by part as numbers: as text, 1.10.0 sorts before 1.9.0. */
function compareReleases(a: string, b: string): number {
  const x = a.split(".").map(Number);
  const y = b.split(".").map(Number);
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] ?? 0) - (y[i] ?? 0);
    if (d !== 0) return d;
  }
  return 0;
}

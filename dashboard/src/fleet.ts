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
 * The agent version most of the fleet runs: the upgrade target when the
 * server's own version is not a release. "Differs from the rest" is a fact,
 * where ordering `git describe` output would be a guess.
 */
export function fleetVersion(versions: string[]): string {
  const counts = new Map<string, number>();
  for (const v of versions) {
    if (v) counts.set(v, (counts.get(v) ?? 0) + 1);
  }
  let best = "";
  let bestN = 0;
  for (const [v, n] of counts) {
    if (n > bestN) [best, bestN] = [v, n];
  }
  return best;
}

/**
 * An agent's version against the release it should run. Semver is compared
 * numerically where both sides are tags; otherwise the answer is "differs",
 * never a guess at which came first.
 */
export function versionState(agent: string, target: string): "ok" | "behind" | "differs" {
  if (!agent || !target || agent === target) return "ok";
  const a = semver(agent);
  const b = semver(target);
  if (!a || !b) return "differs";
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] < b[i] ? "behind" : "ok";
  }
  return "ok";
}

/** [major, minor, patch] for a plain vX.Y.Z tag, or null for anything else. */
function semver(v: string): [number, number, number] | null {
  const m = /^v?(\d+)\.(\d+)\.(\d+)$/.exec(v.trim());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

/**
 * Whether an agent could be told to upgrade *to* this version. Only a release
 * tag qualifies: a working-tree build ("6387414-dirty", "dev") cannot be installed.
 */
export function isRelease(v: string): boolean {
  return semver(v) !== null;
}

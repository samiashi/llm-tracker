<p align="center">
  <img src="docs/logo.svg" alt="llm-tracker" width="560">
</p>

<p align="center">
  <em>Team-wide token usage across every AI coding agent we use —<br>
  collected locally, counted once, and never carrying a line of code.</em>
</p>

---

A small daemon on each developer's Mac reads the session logs their coding
agents already write and sends token counts to
[llm-tracker.example.com](https://llm-tracker.example.com), where members of the
`your-org` GitHub org sign in to see the dashboard.

<p align="center">
  <img src="docs/images/dashboard-overview.png" alt="Dashboard: headline totals, billable and cache traffic over time, and the model mix" width="900">
</p>

## Install

On your Mac, with the GitHub CLI signed in to your GitHub account:

```bash
brew install gh && gh auth login        # once
gh api repos/samiashi/llm-tracker/contents/install.sh \
  -H "Accept: application/vnd.github.raw" | sh
```

Nothing is asked. The installer downloads the agent and checks it against the
published checksum. Then it enrols the Mac: the tracker checks that your `gh`
account is in the org and gives this machine an upload token of its own. From
then on the agent collects and uploads at login and every 5 minutes.

```bash
llm-tracker-agent status      # running? last upload?
llm-tracker-agent upgrade     # to the latest release
llm-tracker-agent uninstall   # stop; rm -rf ~/.llm-tracker also deletes the local archive
```

If the download fails, `gh` is signed in as an account that cannot see this
repository: check `gh auth status`, then `gh auth login` or `gh auth switch`.

## What is tracked

| Verified on real installs                   | Built from documentation (`llm-tracker-agent probe <name>` to check)                             |
| ------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Claude Code, Claude Cowork, Codex, opencode | Gemini CLI, GitHub Copilot CLI, Kimi Code, DeepSeek Harness, Z.ai ZCode, Cline, Roo Code, Continue |

Models reached through Claude Code (GLM, Kimi, DeepSeek) count with it.
Cursor, Windsurf and the ChatGPT app keep no usable token record on disk, so
they are not tracked. An adapter for them would report confident, wrong
numbers.

Claude Code deletes its transcripts after 30 days. The agent's archive in
`~/.llm-tracker` is the only lasting copy, so keep the agent installed.

## What leaves your Mac

Token counts, with what is needed to break them down:

- model, provider and timestamps;
- session IDs, project paths and git branches;
- the hostname and a hash of the hardware ID;
- the email of each signed-in account.

**Never prompts, responses, code, diffs or tool arguments.** The event type
has no field for them, and a test fails if one is added. At enrolment, your
`gh` token goes to the tracker once for the org check. It is not stored.

## Reading the numbers

- **Three spend figures, never added together.** _Billed_ is metered API
  spend. _Rate-card equivalent_ prices subscription usage at API rates, which
  is what it would have cost. _Basis unknown_ is usage the agent cannot place
  in either.
- **Cache reads have their own chart.** They are most of the tokens and bill
  at a fraction of input, so charted with everything else they flatten it.
- **Days are UTC days**, which start at 04:00 in Dubai. The hourly grid is
  in Dubai time.
- **Unpriced means unknown, not free.** A model missing from the price table
  is counted but not costed.

## Running the tracker

It runs on one Compute Engine VM. `deploy/gcp/deploy.sh` sets it up:

- Caddy in front for TLS;
- the database on a disk snapshotted daily;
- the settings in Secret Manager.

```bash
gcloud config set project <project-id>
deploy/gcp/deploy.sh create                  # once; asks for the OAuth app and an image pull token
deploy/gcp/deploy.sh revoke <github-login>   # someone left the org
deploy/gcp/deploy.sh logs
```

`create` needs:

- a published release;
- a GitHub OAuth app owned by the org, with the callback
  `https://llm-tracker.example.com/auth/callback`;
- a classic GitHub token with only `read:packages`, to pull the private image;
- `gh` signed in as an admin of this repository, to store the settings the
  release workflow deploys with.

It waits until `llm-tracker.example.com` resolves to the IP it reserved.

**Releasing** is a tag:

```bash
git tag v1.5.0 && git push origin v1.5.0
```

The tag builds the agent for both Mac architectures and the server image,
publishes the release, then deploys it with `deploy.sh upgrade`. That
snapshots the disk and starts the new server. The new server migrates its
database, then reprices stored costs to the release's prices. A backport is
published but not deployed. Colleagues upgrade with
`llm-tracker-agent upgrade`, and the dashboard marks whose agent is behind.
AGENTS.md has the release rules.

## Development

```bash
make build        # dashboard, then both binaries into bin/
make test         # Go under two time zones, the price generator, the dashboard
make lint         # must be clean
```

To run it locally, create a GitHub OAuth app of your own with the callback
`http://127.0.0.1:8790/auth/callback`:

```bash
cp .env.example .env                  # fill in the app's keys and a session key
make run-server                       # http://127.0.0.1:8790
cd dashboard && npm run dev           # http://127.0.0.1:5178, after signing in on :8790
./bin/llm-tracker-agent enroll -server http://127.0.0.1:8790
```

[AGENTS.md](AGENTS.md) holds the invariants: the rules that, broken, produce
wrong numbers rather than errors. Read it before changing the adapters, the
schema or the queries.

This is an internal tool, not published or licensed for outside use.

# AGENTS.md

Context for coding agents working in this repository. Read this before
changing anything under `schema/`, `agent/` or `server/internal/db/`.

## What this is

A Go daemon reads AI coding-agent session logs from each developer's Mac and
ships **token counts only** to a self-hosted server with an embedded React
dashboard. Three modules plus a frontend:

```
schema/     event types + generated price table — imported by BOTH binaries
agent/      collector daemon (adapters, local SQLite archive, sync, launchd)
server/     ingest API, queries, embedded dashboard
dashboard/  React 19 + Vite + Recharts
```

## Commands

```bash
make build    # dashboard, then both binaries (dashboard is embedded in the server)
make test     # Go under UTC and America/Santiago (-count=1), the price generator's Python tests, the dashboard suite
make lint     # static analysis, both halves — must be clean
make fmt      # format Go and the dashboard in place
make prices   # regenerate schema/prices.json from LiteLLM
make image    # the server image, stamped with git describe
```

Always run `make lint` and `make test` before finishing. Both are clean on
`main`; leave them that way.

## Invariants

These are not style preferences. Each one was a bug that silently produced
wrong numbers, and the comment above the code says which.

1. **A stored token count must never decrease.** Conflict rules take `MAX`.
   Locally a row moves as a unit, and only to a better reading of its id: the
   payload, total and `collector` column change together (`store.putAllTx`),
   because the payload is what gets shipped and a row whose two halves
   disagree is worse than either. On the server the token split is always one
   reading's, and a merged row's table price is recomputed from the stored row
   (`priceMerged`), never taken from the reading merged into it.
2. **Events and their file cursor commit in one transaction**
   (`store.CommitFile`), with any parser state staged through `c.StageMeta`.
   Advancing a cursor before its rows are durable loses them permanently —
   Claude Code deletes its own transcripts after 30 days.
3. **Billed, rate-card and unknown-basis spend are never summed.** Seat usage
   has no marginal cost; adding its rate-card equivalent to metered spend
   produces a number that means nothing, and unknown basis is usage whose
   adapter cannot say which of the two it was. They are separate columns and
   separate fields.
4. **`schema.Event` has no field for message content** — no prompts,
   completions, file contents, diffs or tool arguments. That is the privacy
   boundary and it is enforced by the type. `agent/internal/sources/privacy_test.go`
   fails if a new field appears on any wire type (`Event`, `Usage`,
   `QuotaSample`, `UnknownSource`, `Batch`, `Account`, `EnrollRequest`) -- it
   reads the _type_, so a field tagged `omitempty` cannot slip past by being
   unset.
5. **Aggregate queries over the reporting window read the `event_daily` view,
   never `event` directly.** The view unions `event_day` -- the live events
   summed per day and rollup key, kept by triggers on `event` -- with pruned-day
   rollups, so retention stays invisible to them and no poll re-adds every
   event. The view must not aggregate at query time, and `event_day` must
   always equal a GROUP BY over `event`: its triggers add signed deltas and drop
   a row only once every column is back to zero, since a re-key trigger can
   delete a row before `event_day` has counted it. Every db test ends by
   checking this, and a migration that rebuilds `event` must recreate the
   triggers. Three queries are exempt because they need something a
   day-level rollup cannot hold — `Heatmap` (hour of day), `TopSessions`
   (session id) and `SourceHealth` (the last event's time) — and so see only
   what has not been rolled up, by design. Anything answering a windowed
   _total_ belongs on the view; adding a new one against `event` silently
   under-reports every pruned day. `EarliestRawDay` and `RollupsBefore` read
   `event` and `daily_rollup` directly to find where rollups end, so a branch
   added to the view must be added there too.
6. **Reads use `d.read`, writes use `d.write`.** One pooled connection makes
   dashboard reads queue behind ingest; that took reads to 30 seconds.
7. **Reasoning effort is ordinal.** Sort by `schema.EffortRank`, never by
   volume or alphabetically. `ultracode` is xhigh plus orchestration, not a
   tier above max.
8. **Prices are generated.** Edit `schema/scripts/gen_prices.py`, not
   `schema/prices.json`. An unrecognised model is reported as _unpriced_, never
   costed at zero — a silent zero hides the models most worth noticing. A local
   runtime (`localRuntimes` in `schema/pricing.go`) is a deliberate zero, and
   counts as priced.
9. **Bump `sources.CollectorVersion`** whenever an adapter starts capturing
   something new or corrects what it captured, and list the sources it changes
   in `backfillOnUpgrade` — the bump alone changes only conflict resolution;
   the list is what re-reads them. If the same usage now arrives under new ids,
   the old rows must also go, or they double every token they hold: list the
   source in `dedupeOnUpgrade`, with a pairwise rule in `sources.Superseded`
   that names an old row only where its replacement exists; where no old row
   can be matched, in `purgeOnUpgrade` instead, and only for an adapter that
   `KeepsHistory`, since a purge deletes rows before the re-read. Either way,
   add the same rule server-side as a migration (00016 is the model): the
   server holds its own copy of the old rows, and nothing the agent does
   locally reaches it.
10. **Time series must be gap-filled.** The API returns only days with
    activity; a categorical x-axis then draws non-consecutive days as adjacent.
    Use `series.calendarDays`.

## Adding a harness adapter

A `schema.Source` constant in `schema/event.go`, then one self-registering file
in `agent/internal/sources/`:

```go
func init() { Register(MyHarness{}) }

type MyHarness struct{}

func (MyHarness) Name() schema.Source { return schema.SourceMyHarness }
func (MyHarness) Roots() []string     { return []string{".myharness/sessions"} }

func (a MyHarness) Collect(ctx context.Context, c *Ctx) (Result, error) {
	return walkJSONL(ctx, c, AbsRoots(a, c), hasExt(".jsonl"), func(path string, at int64, line []byte) {
		// parse, then c.emit(schema.Event{...})
	})
}
```

`walkJSONL` handles incremental reads, partial trailing lines, rotation and the
atomic commit. `at` is the line's byte offset, and it is usually what an event
should be keyed on: for a format with no id of its own it is the only stable
identifier, and a key built on something that moves — an array index, an
absolute path — re-keys every event when it does. A walk of your own goes
through `walkRoot`, which also descends a root that is itself a symlink.
`openAIUsage.normalise()` folds the spellings every OpenAI-compatible provider
uses.

`Roots()`, relative to home, also scopes `rewind` and `resync`:
`sources.ScopeOf` clears the cursors under each root and the meta keys
prefixed `<source>:`. Key every watermark and piece of parser state that way;
Codex's `codexctx:` predates the convention and is declared through
`MetaPrefixes()`.

For a source that rewrites whole files rather than appending — a JSON array, a
SQLite table — there is no byte offset to resume from. Keep a watermark keyed
`<source>:`, as `opencode.go` and `cline.go` do, and set it with
`c.StageMeta` before `c.CommitPending`: it then commits in the same
transaction as the rows it covers, which is invariant 2 for a source with no
cursor.

Three more declarations matter downstream:

- **`KeepsHistory()`** — implement it, returning true, only for a harness that
  never drops a record it wrote (Codex, opencode). It is what permits a purge
  on upgrade (invariant 9), and assuming it wrongly is permanent.
- **`Event.coversOneRequest`** in `schema/pricing.go` — add the source if its
  events are session or turn totals, as Gemini's, Kimi's and Copilot's are, or
  long-context tiers are applied to sums no single request reached.
- **`localRuntimes`** in `schema/pricing.go` — an event from a local model must
  carry one of these endpoint ids to price at $0; any other endpoint is priced
  as a provider's.

`Collect` commits as it goes, so `c.Drain()` is empty once it returns. A test
that asserts on emitted events must either call the parsing step directly or
read back from the store.

Declaring `Roots()` removes the harness from the "detected but unsupported"
list, and a test enforces that no `todo` candidate is left listed once an
adapter covers it. A `blocked` candidate is exempt: it records why a harness
_cannot_ be read, which stays true even when an adapter reads a different
tool's data from under the same directory.

Verify against real data with `llm-tracker-agent probe <name>` before
trusting it, and say so in the doc comment when you could not — the
`UNVERIFIED:` banner on `kimi.go` and `cline.go` is the house form. These
formats are undocumented and change without notice.

When a harness keeps no usable local record, do not approximate one. Add it as
a `blocked` candidate in `scan.go` with the fields you checked and why each was
wrong. Cursor is the worked example: it has local token counters, they are
context-window occupancy rather than billed usage, and an adapter reading them
would produce confident wrong numbers — the one outcome this project treats as
worse than a gap.

## Charts

- **Stack only for part-to-whole of one entity.** `MatrixBars` and
  `Composition` stack, because a segment is a share of the bar it sits in and
  the total is the point. The time-series charts do not: stacked, the top line
  is a running total no series ever reached, and every reader takes the highest
  line for the biggest series.
- **Rank over the whole range, never per point.** Otherwise a colour changes
  which entity it means partway along, and the series reads as a trend when it
  is two things spliced together.
- **Never generate a hue.** Categorical slots are assigned by position and the
  server returns at most as many series as there are slots.
- **Chart animation follows `prefers-reduced-motion`.** Recharts animates in
  JavaScript, so CSS cannot reach it -- `usePrefersReducedMotion` passes
  `isAnimationActive`. These charts redraw on every poll, so the entry
  animation runs once a minute for as long as the page is open.

## Versions

Three, and conflating them is the trap:

- **Release tag** (`v1.4.0`) — which build is installed, set from
  `git describe` at build time. `schema.ReleaseVersion` parses `X.Y.Z`, with or
  without the `v`, and nothing else: `dev` and `<sha>-dirty` are not releases.
  One tag builds both halves (see Releasing).
- **`schema.Version`** — the wire format. Old agents stay installed on
  teammates' machines for months, so the server keeps accepting older values
  rather than rejecting them.
- **`CollectorVersion`** — what the collector knows how to read. Both stores
  keep it per row, and a newer collector's reading of an id replaces an older
  one's token split and descriptive fields when it holds at least as many
  tokens, so a corrected adapter reaches rows already stored and uploaded with
  no delete step. Invariant 9 says what a bump needs.

A stale agent is flagged, never refused. It warns once, only when the server is
on a newer release, and the dashboard marks it against the server's release. It
collects with an older adapter set, which reads as smaller numbers rather than
as an error — but an agent that stopped uploading because it was out of date
would turn that into missing data.

## Releasing

A release is a tag; `.github/workflows/release.yml` does the rest.

```bash
make lint && make test          # both clean, or do not tag
git switch main && git pull     # and CI green on this commit, darwin job included
git tag v1.4.0 && git push origin v1.4.0
```

1. **`verify`** checks the tag is exactly `vX.Y.Z`, refuses a tag whose release
   is already published (failing closed on anything but a clear 404), and runs
   `make lint-go test-go lint-dashboard test-dashboard`, the recipes `make lint`
   and `make test` run locally. It does not run CI's darwin job, so tag only a
   commit whose CI is green.
2. **`agent`, `server` and `image`** cross-compile from Linux: `CGO_ENABLED=0`
   and a pure-Go SQLite driver mean no macOS runner, which bills at ten times
   the rate. `agent` and `server` hold read-only tokens and hand their files to
   `publish` as workflow artifacts, kept 7 days; `image` pushes both
   architectures untagged, by digest.
3. **`publish`**, the only job with `contents: write`, checks each `.sha256`
   against its binary, creates the draft release or reuses its own from an
   earlier attempt (recognised by `Built from <sha>.` in its body), uploads,
   checks that exactly the promised assets are attached and that the tag still
   points at the commit, tags the image, and only then makes the release
   visible — so nobody tracking the latest release or `:latest` gets one whose
   other half never built. A backport — a tag below the highest published
   release — ships with `--latest=false` and leaves `:latest` alone.
4. **`deploy`** runs `deploy/gcp/deploy.sh upgrade` for the highest release
   only, never a backport, and only once `create` has stored the `GCP_*`
   repository variables. It holds no key: Workload Identity Federation
   exchanges GitHub's OIDC token for a deploy service account's short-lived
   credentials, and accepts only this repository's `release.yml` on a `v*`
   tag. That account can snapshot the disk, set the VM's metadata and SSH in
   as root through IAP, which is what `upgrade` does.

Runs for one tag share a concurrency group, and every checkout sets
`persist-credentials: false`.

| Artefact                                                      | Targets                                                                        |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| `llm-tracker-agent-darwin-{arm64,amd64}`                    | plus a `.sha256` for each                                                      |
| `llm-tracker-server-{linux-amd64,linux-arm64,darwin-arm64}` | no checksum yet                                                                |
| `ghcr.io/<owner>/llm-tracker-server`                        | `linux/amd64`, `linux/arm64`; `:vX.Y.Z`, and `:latest` for the highest release |

Rules, each load-bearing:

**One tag ships both halves.** The server returns its own version on every
ingest, which is how an agent learns it is stale without reaching GitHub, and
what the dashboard's upgrade target is. Release them separately and the
server's version stops meaning anything.

**The tag must be exactly `vX.Y.Z`.** Anything `schema.ReleaseVersion` cannot
parse — `v1.4`, `v1.4.0-rc1` — silently turns off the staleness check for the
whole fleet, and `agent upgrade` refuses it without `-force`. `on.push.tags` is
a glob matched before any expression runs, which is why `verify` checks the
shape.

**Never re-tag.** `install.sh` and `agent upgrade` verify against the published
`.sha256`, so moving a tag makes every agent refuse the download it now
disagrees with. Cut a patch instead. `verify` and `publish` refuse a published
tag, and `publish` refuses a draft another commit built or a tag that moved
during the run; turn on GitHub's immutable releases in the repository settings
to enforce the rule outright.

**The image cross-compiles, it does not emulate.** The Dockerfile pins both
build stages to `$BUILDPLATFORM` and passes `$TARGETARCH` to `go build`, so one
Linux runner produces both architectures. Dropping those turns a one-minute
build into ten under QEMU. It builds the server module alone, not the
workspace — `server/go.mod` replaces `schema` with `../schema` — so an
agent-only change does not rebuild the image.

**Nothing is manual afterwards.** `install.sh` and `agent upgrade` both read
the latest release through `gh`, and `deploy` upgrades the server. There is no
separate publish or deploy step.

## Deploying the server

One binary and one SQLite file. No external database, no Redis, nothing to
provision but a disk.

- **Migrations run themselves** — `goose.Up` on `db.Open`, from an embedded FS.
  Deploying is replacing the binary and restarting.
- **Stored costs follow the build.** Cost is resolved at ingest, so each new
  build, and a changed price table under the same build, reprices stored
  events once, in the background after it starts (`db.RepriceIfChanged`). Every write is conditional on the row still holding
  what it was priced from, which is what makes running beside ingest safe.
  Rolled-up days keep the prices they had: a rollup keeps sums, not the
  dimensions pricing needs.
- **Forward only in practice.** `00013`, `00016` and `00020` delete data
  their Down cannot restore, and `00016` installs triggers that keep retiring
  re-keyed rows as agents upgrade, so never plan a rollback that crosses one;
  roll forward with a new migration. And never edit one that has shipped.
- **One writer, ever.** SQLite on a volume means exactly one instance. Two
  gives you two divergent databases and no error.
- **It needs a persistent filesystem and a long-lived process** — the daily
  prune is a goroutine — so serverless hosts are out.
- **Back up the `.db`** — and its `-wal`, or use `sqlite3 .backup`, which is
  consistent without stopping the server.

`deploy/gcp/deploy.sh` sets up and upgrades the deployment on one Compute
Engine VM; the README's On Google Cloud section says what it creates. Its
`startup.sh` runs on every boot, and must never let a container start before
the data disk is mounted: `/mnt/disks` is tmpfs there, and a server started on
the empty directory would take uploads into memory and lose them, while the
agents mark them sent.

GitHub auth is not optional, locally included: the server does not start
without all of `LLM_TRACKER_GITHUB_{CLIENT_ID,CLIENT_SECRET,ORG}`,
`LLM_TRACKER_BASE_URL` and a `LLM_TRACKER_SESSION_KEY` of at least 32
bytes, and it names every one that is missing. Development uses an OAuth app of
its own. Keep it to one way of running: no unauthenticated mode, no shared
token, no overrides.

Retention is off by default. Pruning rolls each day up before deleting its
events, and a rolled-up day never returns to per-event detail: turning pruning
off only lets agents deliver what it refused. The README's Retention section
has the rules.

## Style

**Go.** Standard library first; the only dependencies are a pure-Go SQLite
driver, goose and uuid. `net/http` routing, no framework. Comments explain
_why_, especially where a subtlety cost a bug — do not delete those.

**TypeScript.** Imports use the `@/` alias, never relative paths. Tests live in
`dashboard/tests/`. Derive state rather than syncing it in an effect; React's
lint rules reject synchronous `setState` in an effect body and they are right.

**SQL.** Migrations are append-only under `server/internal/db/migrations/`.
Never edit one that has shipped. Dimensions that reach SQL go through an
allow-list, never string interpolation from a request.

**Charts.** Read the repository's data-visualisation conventions before adding
one: sequential encoding is a single hue, categorical slots are assigned in
fixed order and never cycled, and no chart uses two y-axes.

## Testing

- Go: table-driven, named for the behaviour they protect.
- Go and the dashboard both run under `America/Santiago` **and** UTC. That
  zone's DST transition is at midnight, which is the case that breaks naive
  date arithmetic; a US-only test passes in every timezone and proves nothing.
  Go runs with `-count=1`, because its test cache ignores `TZ`.
- The price generator has its own tests (`schema/scripts/test_gen_prices.py`),
  run by `make test` and CI.
- The server↔dashboard contract is pinned by fixtures in
  `dashboard/tests/fixtures/api`, written from the live handlers by
  `TestDashboardContract` and type-checked against `api.ts` by `tsc -b`
  (`make lint`). After changing a response on purpose, run
  `go test ./server/internal/api -run DashboardContract -update` and fix
  `api.ts` until `tsc -b` passes.
- A test that asserts an invariant above is worth more than one that asserts a
  shape. Prefer the former.

## Security

- Everything but `/healthz`, the OAuth flow, ingest and enrolment needs a
  GitHub session from a member of the org, the page and its scripts included.
- Ingest takes only tokens enrolment issued. `POST /v1/enroll` trades a GitHub
  token for one, after the org check the dashboard makes; the server keeps only
  a SHA-256 of it, and `-revoke <login>` withdraws a login's tokens.
- Ingest bounds every list in a batch, clips every string, and rejects an
  event whose counters, timestamp or native cost are implausible. A list added
  to `schema.Batch` must be shadowed in `ingestBody`; a test fails until it is.
- Never log or transmit an OAuth token. `identity` reads them only to name the
  active account. The one exception is `enroll`, which sends the GitHub CLI's
  token to the tracker, once: only over https (loopback excepted), never on
  through a redirect, and the server spends it on the membership check alone.
- Secrets belong in `.env`, which is gitignored. `.env.example` is the
  committed template: add the key there too, with no value, or nobody else
  learns it exists.

## Do not

- Hand-edit `schema/prices.json`, or any file under `server/internal/web/dist/`.
- Add a field to `schema.Event` that could hold message text.
- Use `resync` where `rewind` would do — it deletes rows before rebuilding and
  permanently loses anything past a source's retention window.
- Commit `.env`, `*.db`, or anything under `bin/`.

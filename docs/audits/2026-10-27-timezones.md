# Timezone audit — 27 October 2026

Scope: timezone and timestamp handling across schema, collector adapters and
archive/sync, server queries/API/retention, dashboard, launchd, and tests.
This is a timezone audit, not a general security or correctness certification.

## Decision

Detect the viewer's browser timezone automatically for hourly charts and absolute
time displays. Keep date filters, daily totals, exports grouped by day, and stored
rollups explicitly UTC. Never re-label a UTC daily sum as a local calendar day.

## Findings and changes

| Area | Finding | Resolution |
| --- | --- | --- |
| Dashboard hourly charts | Labels and bucket alignment were hard-coded to UTC. | Use the browser's IANA zone, send it with read requests, align buckets to local hours, and format local dates/times with numeric offsets. UTC instant keys keep repeated DST hours distinct. |
| Heatmap | Used the server's zone and a single midpoint offset across an entire range, shifting events on one side of a DST transition. | Use the requested viewer zone and actual timezone transition intervals. Its 24-column grid combines repeated wall-clock hours and says so. Legacy callers without a zone still use the server zone. |
| Comparison tooltip | Displayed raw RFC3339 UTC timestamps. | Localize instant ranges; label date-only comparisons as UTC days. Relative ages are elapsed durations and need no timezone conversion. |
| Collector probe | Printed a literal `Z` even when a parsed timestamp carried a non-UTC offset. | Format in the CLI host's local zone with an actual RFC3339 offset. Collected timestamps and counts are unchanged. |
| API | No viewer timezone parameter. | Accept and validate an IANA zone on read requests. Reject invalid zones and bound timezone-aware ranges to the dashboard's existing 3650-day maximum. SQL contains only numeric literals derived from timezone data, never a user-supplied zone name. Bundle Go timezone data for hosts without zoneinfo. |
| Tests | UTC/Santiago runs protected calendar arithmetic, but did not cover viewer/server mismatch, repeated hours, or fractional offsets. | Add Dubai, Kolkata, Kathmandu, New York DST, Santiago midnight DST, and Lord Howe half-hour DST cases, with token-total conservation checks. |

## Correct UTC behavior retained

- `schema.Event.TS` is an instant. Adapters accept RFC3339 offsets or epoch times;
  the archive and sync use instants. No account attribution, proxy match, event
  ID, upload payload, collector version, or stored token count needs changing.
- `server/internal/db/ingest.go` derives the UTC `day`; `event_day` triggers and
  `daily_rollup` preserve UTC-day sums. After pruning, there is insufficient
  information to divide those sums at another timezone's midnight.
- Retention cutoff, retention-floor comparisons in the collector, and historical
  date boundaries must continue agreeing on UTC to avoid dropped/duplicate rows.
- Daily series, CSV day columns, history bounds, date inputs, and 7d/30d/90d/All
  continue using UTC days. The 24h filter remains exactly 86400 elapsed seconds.
- launchd installation and scheduling do not choose a dashboard display zone.
  Viewer localization does not require changing the Mac's timezone or job env.

## Operational behavior and limits

The browser/OS timezone setting is the source, not IP geolocation or an account
profile. The dashboard rechecks it as its clock updates; a changed zone changes
the request key and reloads the data. Shared relative links follow the recipient's
zone. If browser detection is unavailable, UTC is the explicit fallback.

The heatmap has one cell per wall-clock hour, so repeated DST hours are combined;
hourly time-series charts retain separate instants and display offsets. Missing
hours in a series are gap-filled using the same local-hour alignment. Partial
first/last hours stay partial rather than admitting events outside the range.

Browser Intl and Go use their own timezone databases. Keeping browsers and Go
builds current matters when governments change timezone rules. No attempt is
made to infer a viewer's location when their OS timezone is set incorrectly.

## Verification

`make lint` and `make test` pass, including Go and dashboard suites in UTC and
America/Santiago, the price-generator suite, and regenerated API contracts.
Timezone tests assert exact bucket keys and conservation of token totals across
hourly series, model series, heatmaps, and summary queries.

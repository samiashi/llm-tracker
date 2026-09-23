package db

import (
	"context"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// IngestResult reports what one upload changed. It is the wire reply itself,
// so the agent decodes the type the server fills; the handler adds the
// rejected count and the server version.
type IngestResult = schema.IngestAck

// longerReading is true when an incoming event saw more of the response than
// the stored row.
const longerReading = `excluded.total_tokens > event.total_tokens`

// newerReading is true when a newer collector read at least as much of the
// response. Its reading replaces an older collector's, which is how a
// corrected adapter reaches rows already uploaded. Old agents send 0, so they
// never override a newer reading.
const newerReading = `(excluded.collector > event.collector
		AND excluded.total_tokens >= event.total_tokens)`

// nativeCost is true when the incoming reading carries the harness's own cost
// and saw at least as much as the stored row.
const nativeCost = `(excluded.cost_source = 'native'
		AND excluded.total_tokens >= event.total_tokens)`

// upsertEvent stores one reading of a response, merging it into any earlier
// reading of the same one. Each column moves only on the condition that makes
// the incoming value better information, and every SET needs an arm in the
// WHERE, or it can never fire.
//
// An id in pruned_event is already counted inside a rollup, so it is neither
// inserted nor merged: it changes nothing, like an equal or poorer reading.
const upsertEvent = `
	INSERT INTO event (
		id, native_id, source, surface, ts, day, machine_id, account_ref,
		provider, model, endpoint,
		input_tokens, output_tokens, cache_read_tokens, cache_write_5m,
		cache_write_1h, reasoning_tokens, web_search_calls, web_fetch_calls,
		total_tokens, cost_basis, cost_usd, cost_source,
		session_id, project_path, git_branch, is_subagent, received_at,
		speed, effort, inference_geo, agent_version, collector
	) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
	  WHERE NOT EXISTS (SELECT 1 FROM pruned_event WHERE id = ?)
	ON CONFLICT(id) DO UPDATE SET
		-- A stored total never falls, and the split is always one reading's.
		input_tokens      = CASE WHEN ` + longerReading + ` OR ` + newerReading + ` THEN excluded.input_tokens      ELSE event.input_tokens      END,
		output_tokens     = CASE WHEN ` + longerReading + ` OR ` + newerReading + ` THEN excluded.output_tokens     ELSE event.output_tokens     END,
		cache_read_tokens = CASE WHEN ` + longerReading + ` OR ` + newerReading + ` THEN excluded.cache_read_tokens ELSE event.cache_read_tokens END,
		cache_write_5m    = CASE WHEN ` + longerReading + ` OR ` + newerReading + ` THEN excluded.cache_write_5m    ELSE event.cache_write_5m    END,
		cache_write_1h    = CASE WHEN ` + longerReading + ` OR ` + newerReading + ` THEN excluded.cache_write_1h    ELSE event.cache_write_1h    END,
		total_tokens      = MAX(excluded.total_tokens, event.total_tokens),

		-- Outside total_tokens, so no reading's total vouches for them. Within
		-- one response each only grows, and web search is billed per call.
		reasoning_tokens  = MAX(excluded.reasoning_tokens, event.reasoning_tokens),
		web_search_calls  = MAX(excluded.web_search_calls, event.web_search_calls),
		web_fetch_calls   = MAX(excluded.web_fetch_calls,  event.web_fetch_calls),

		-- What a newer collector read replaces what an older one did.
		native_id    = CASE WHEN ` + newerReading + ` THEN excluded.native_id    ELSE event.native_id    END,
		surface      = CASE WHEN ` + newerReading + ` THEN excluded.surface      ELSE event.surface      END,
		ts           = CASE WHEN ` + newerReading + ` THEN excluded.ts           ELSE event.ts           END,
		day          = CASE WHEN ` + newerReading + ` THEN excluded.day          ELSE event.day          END,
		provider     = CASE WHEN ` + newerReading + ` THEN excluded.provider     ELSE event.provider     END,
		model        = CASE WHEN ` + newerReading + ` THEN excluded.model        ELSE event.model        END,
		endpoint     = CASE WHEN ` + newerReading + ` THEN excluded.endpoint     ELSE event.endpoint     END,
		session_id   = CASE WHEN ` + newerReading + ` THEN excluded.session_id   ELSE event.session_id   END,
		project_path = CASE WHEN ` + newerReading + ` THEN excluded.project_path ELSE event.project_path END,
		git_branch   = CASE WHEN ` + newerReading + ` THEN excluded.git_branch   ELSE event.git_branch   END,
		is_subagent  = CASE WHEN ` + newerReading + ` THEN excluded.is_subagent  ELSE event.is_subagent  END,
		collector    = CASE WHEN ` + newerReading + ` THEN excluded.collector    ELSE event.collector    END,

		-- Also filled in where we had none, by any reading.
		effort        = CASE WHEN ` + newerReading + ` OR event.effort        = '' THEN excluded.effort        ELSE event.effort        END,
		speed         = CASE WHEN ` + newerReading + ` OR event.speed         = '' THEN excluded.speed         ELSE event.speed         END,
		inference_geo = CASE WHEN ` + newerReading + ` OR event.inference_geo = '' THEN excluded.inference_geo ELSE event.inference_geo END,
		agent_version = CASE WHEN ` + newerReading + ` OR event.agent_version = '' THEN excluded.agent_version ELSE event.agent_version END,

		-- Stamped from whoever is signed in when a file is read, so only ever
		-- filled in: a re-read after a login switch must not move history. A
		-- known basis is never replaced by 'unknown', which is the absence of one.
		account_ref = CASE WHEN event.account_ref = '' THEN excluded.account_ref ELSE event.account_ref END,
		cost_basis  = CASE WHEN excluded.cost_basis IN ` + knownBasis + `
		                    AND (` + newerReading + ` OR event.cost_basis NOT IN ` + knownBasis + `)
		                   THEN excluded.cost_basis ELSE event.cost_basis END,

		-- A native figure is taken only from a reading at least as long, and
		-- never gives way to a table price. Otherwise the reading's own price
		-- stands in until priceMerged prices the merged row; usually it already
		-- is that price, and no second write is needed.
		cost_usd    = CASE WHEN ` + nativeCost + ` THEN excluded.cost_usd
		                   WHEN event.cost_source = 'native' OR excluded.cost_source = 'native'
		                   THEN event.cost_usd ELSE excluded.cost_usd END,
		cost_source = CASE WHEN ` + nativeCost + ` THEN 'native'
		                   WHEN event.cost_source = 'native' OR excluded.cost_source = 'native'
		                   THEN event.cost_source ELSE excluded.cost_source END,

		received_at = excluded.received_at
	WHERE ` + longerReading + ` OR ` + newerReading + `
	   OR excluded.reasoning_tokens > event.reasoning_tokens
	   OR excluded.web_search_calls > event.web_search_calls
	   OR excluded.web_fetch_calls  > event.web_fetch_calls
	   OR (` + nativeCost + ` AND (event.cost_source != 'native' OR excluded.cost_usd != event.cost_usd))
	   OR (excluded.effort        != '' AND event.effort        = '')
	   OR (excluded.speed         != '' AND event.speed         = '')
	   OR (excluded.inference_geo != '' AND event.inference_geo = '')
	   OR (excluded.agent_version != '' AND event.agent_version = '')
	   OR (excluded.account_ref   != '' AND event.account_ref   = '')
	   OR (excluded.cost_basis IN ` + knownBasis + ` AND event.cost_basis NOT IN ` + knownBasis + `)`

// Ingest stores one agent batch.
func (d *DB) Ingest(ctx context.Context, b *schema.Batch) (*IngestResult, error) {
	now := time.Now().Unix()
	res := &IngestResult{EventsReceived: len(b.Events)}

	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if b.MachineID != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO machine (id, hostname, agent_version, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?)
			-- An agent that could not read its hostname sends none; that must
			-- not blank the name the agents table shows.
			ON CONFLICT(id) DO UPDATE SET
			  hostname = CASE WHEN excluded.hostname != '' THEN excluded.hostname
			                  ELSE machine.hostname END,
			  agent_version = excluded.agent_version, last_seen = excluded.last_seen`,
			b.MachineID, b.Hostname, b.AgentVersion, now, now); err != nil {
			return nil, err
		}
	}

	for _, a := range b.Accounts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account (ref, provider, email, plan_type, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?)
			-- Never blanked. The agent sends a bare ref whenever it cannot
			-- decode the login -- an expired token, an API key -- and a blanked
			-- email splits one colleague's history into two people.
			ON CONFLICT(ref) DO UPDATE SET
			  provider  = CASE WHEN excluded.provider != '' THEN excluded.provider
			                   ELSE account.provider END,
			  email     = CASE WHEN excluded.email != '' THEN excluded.email
			                   ELSE account.email END,
			  plan_type = CASE WHEN excluded.plan_type != '' THEN excluded.plan_type
			                   ELSE account.plan_type END,
			  last_seen = excluded.last_seen`,
			a.Ref, a.Provider, a.Email, a.PlanType, now, now); err != nil {
			return nil, err
		}
	}

	upsert, err := tx.PrepareContext(ctx, upsertEvent)
	if err != nil {
		return nil, err
	}
	defer upsert.Close()

	// A machine belongs to one person in practice, so an event whose harness
	// knows no account -- opencode authenticates per provider -- is credited
	// to the machine's first one, or its spend belongs to nobody.
	var machineAccount string
	if len(b.Accounts) > 0 {
		machineAccount = b.Accounts[0].Ref
	}

	floor, err := d.ingestFloorTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Reported on every ingest, and enforced exactly when a floor is: an
	// agent re-offers what it was refused only when told nothing is refused,
	// so any mismatch re-sends the refused backlog on every push.
	res.RetentionFloor = floor
	res.RetentionEnforced = floor != ""

	// merged collects the rows a reading was merged into; see priceMerged.
	// An upsert that updates leaves last_insert_rowid where it was, so a new
	// rowid means the reading was inserted as it arrived, already priced. The
	// first upsert has nothing to compare with and is always re-read.
	var merged []string
	lastRowid := int64(-1)
	for i := range b.Events {
		e := &b.Events[i]
		// The day is derived from the substituted timestamp, never the raw
		// one: a zero ts must not land on day 0001-01-01, outside every query.
		ts := e.TS
		if ts.IsZero() {
			ts = time.Unix(now, 0)
		}
		day := ts.UTC().Format("2006-01-02")
		if floor != "" && day < floor {
			res.EventsSkipped++
			continue
		}
		if e.AccountRef == "" {
			e.AccountRef = machineAccount
		}
		cost, source := d.priceEvent(e)
		if source == "unpriced" {
			res.Unpriced++
		}
		u := e.Usage
		r, err := upsert.ExecContext(ctx,
			e.ID, e.NativeID, string(e.Source), string(e.Surface), ts.Unix(), day,
			e.MachineID, e.AccountRef, e.Provider, e.Model, e.Endpoint,
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWrite5mTokens,
			u.CacheWrite1hTokens, u.ReasoningTokens, u.WebSearchCalls, u.WebFetchCalls,
			u.TotalTokens(), string(e.CostBasis), cost, source,
			e.SessionID, e.ProjectPath, e.GitBranch, boolInt(e.IsSubagent), now,
			e.Speed, e.Effort, e.InferenceGeo, e.AgentVersion, e.Collector, e.ID)
		if err != nil {
			return nil, err
		}
		rowid, _ := r.LastInsertId()
		inserted := lastRowid >= 0 && rowid != lastRowid
		lastRowid = rowid
		// Nothing changed: an equal or poorer reading, or one already counted
		// inside a rollup whatever machine, account or day it arrives with now.
		// Either is a successful duplicate, neither stored nor refused.
		if n, _ := r.RowsAffected(); n > 0 {
			res.EventsStored++
			if !inserted {
				merged = append(merged, e.ID)
			}
		}
	}
	if err := d.priceMerged(ctx, tx, merged); err != nil {
		return nil, err
	}

	for _, q := range b.Quota {
		r, err := tx.ExecContext(ctx, `
			INSERT INTO quota_sample (id, source, ts, machine_id, account_ref, plan_type,
			  limit_id, limit_name, window_minutes, used_percent, resets_at, is_overage)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			-- The agent re-sends an hour whenever its peak rises, so the peak
			-- is kept rather than the first sample: an hour that ended at 91%
			-- must not report the 8% it started at.
			ON CONFLICT(id) DO UPDATE SET
			  used_percent = MAX(excluded.used_percent, quota_sample.used_percent),
			  is_overage   = MAX(excluded.is_overage, quota_sample.is_overage),
			  plan_type    = CASE WHEN quota_sample.plan_type = ''
			                      THEN excluded.plan_type ELSE quota_sample.plan_type END,
			  resets_at    = MAX(excluded.resets_at, quota_sample.resets_at),
			  -- Filled in once and never blanked: an older collector re-sending
			  -- the hour must not strip the pool a newer one named.
			  limit_id     = CASE WHEN quota_sample.limit_id = ''
			                      THEN excluded.limit_id ELSE quota_sample.limit_id END,
			  limit_name   = CASE WHEN quota_sample.limit_name = ''
			                      THEN excluded.limit_name ELSE quota_sample.limit_name END
			-- Every SET above needs an arm here, or it can never fire.
			WHERE excluded.used_percent > quota_sample.used_percent
			   OR excluded.is_overage > quota_sample.is_overage
			   OR excluded.resets_at > quota_sample.resets_at
			   OR (excluded.plan_type != '' AND quota_sample.plan_type = '')
			   OR (excluded.limit_id != '' AND quota_sample.limit_id = '')
			   OR (excluded.limit_name != '' AND quota_sample.limit_name = '')`,
			q.ID, string(q.Source), q.TS.Unix(), q.MachineID, q.AccountRef, q.PlanType,
			q.LimitID, q.LimitName,
			q.WindowMinutes, q.UsedPercent, unixOrZero(q.ResetsAt), boolInt(q.IsOverage))
		if err != nil {
			return nil, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.QuotaStored++
		}
	}

	if b.UnknownComplete && b.MachineID != "" {
		// Replace rather than merge: a harness that gained an adapter must
		// stop being reported as a gap.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM unknown_source WHERE machine_id = ?`, b.MachineID); err != nil {
			return nil, err
		}
	}
	for _, us := range b.UnknownSource {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO unknown_source (machine_id, path, hint, size_bytes, first_seen, last_seen, status, note)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(machine_id, path) DO UPDATE SET size_bytes=excluded.size_bytes,
			  hint=excluded.hint, status=excluded.status, note=excluded.note,
			  last_seen=excluded.last_seen`,
			us.MachineID, us.Path, us.Hint, us.SizeBytes, now, now,
			string(us.Status), us.Note); err != nil {
			return nil, err
		}
		res.UnknownStored++
	}

	return res, tx.Commit()
}

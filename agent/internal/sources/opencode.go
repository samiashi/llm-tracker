package sources

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // opencode.db is SQLite; registered here, not inherited from store

	"github.com/samiashi/llm-tracker/schema"
)

func init() { Register(OpenCode{}) }

// OpenCode reads opencode.db, the SQLite database opencode keeps its history
// in; the JSON tree beside it is a legacy layout it no longer writes. Like
// Cline, opencode computes its own cost, which the server prefers over the
// price table.
//
// Its history does not outlive our archive (no KeepsHistory): deleting a
// session deletes its messages with it (message.session_id is ON DELETE
// CASCADE), and a message can be removed on its own.
type OpenCode struct{}

func (OpenCode) Name() schema.Source { return schema.SourceOpenCode }
func (OpenCode) Roots() []string     { return []string{".local/share/opencode"} }

func (OpenCode) dbPath(c *Ctx) string {
	return filepath.Join(c.Home, ".local", "share", "opencode", "opencode.db")
}

// Collect reads one event per assistant response, from the `message` table:
// each row carries its own time, model, provider and variant. The `session`
// table's running totals would stamp a whole conversation at the moment it
// was last touched, on whatever model it ended with.
func (a OpenCode) Collect(ctx context.Context, c *Ctx) (Result, error) {
	var res Result
	path := a.dbPath(c)

	if _, err := os.Stat(path); err != nil {
		return res, nil // opencode is not installed here
	}
	// Read-only: opencode owns this live database, and query_only makes an
	// accidental write fail loudly rather than corrupt its history.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(true)&_pragma=busy_timeout(5000)")
	if err != nil {
		return res, err
	}
	defer db.Close()

	// A message is rewritten as it streams, so the watermark is on
	// time_updated and a changed row is read again; the max-wins upsert turns
	// the repeat into an update. The key is the message table's own: another
	// table's watermark counts in a different time base.
	var since int64
	if v, err := c.Store.Meta(ctx, "opencode:message_watermark"); err == nil && v != "" {
		since, _ = strconv.ParseInt(v, 10, 64)
	}

	// Every field is named. `data` also holds a `summary` and, on a failed
	// call, the provider's entire error response body -- so it is never read
	// whole, and nothing here can carry a prompt or a completion.
	//
	// A subagent runs in a child session of the one that started it, and
	// that, not the agent's name, is what marks it: opencode's primary agents
	// include plan and the hidden compaction, title and summary, which all
	// run in the root session.
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.session_id, m.time_updated,
		       json_extract(m.data,'$.modelID'),
		       json_extract(m.data,'$.providerID'),
		       json_extract(m.data,'$.variant'),
		       COALESCE(s.parent_id, '') <> '',
		       json_extract(m.data,'$.cost'),
		       json_extract(m.data,'$.tokens.input'),
		       json_extract(m.data,'$.tokens.output'),
		       json_extract(m.data,'$.tokens.reasoning'),
		       json_extract(m.data,'$.tokens.cache.read'),
		       json_extract(m.data,'$.tokens.cache.write'),
		       json_extract(m.data,'$.time.created')
		FROM message m LEFT JOIN session s ON s.id = m.session_id
		WHERE m.time_updated > ? AND json_extract(m.data,'$.role') = 'assistant'
		ORDER BY m.time_updated`, since)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	newWatermark := since
	for rows.Next() {
		var (
			id, sessionID                string
			updated                      int64
			model, provider, variant     sql.NullString
			subagent                     bool
			cost                         sql.NullFloat64
			tin, tout, treason, tcr, tcw sql.NullInt64
			created                      sql.NullInt64
		)
		res.Scanned++
		if err := rows.Scan(&id, &sessionID, &updated, &model, &provider, &variant,
			&subagent, &cost, &tin, &tout, &treason, &tcr, &tcw, &created); err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		if updated > newWatermark {
			newWatermark = updated
		}

		usage := schema.Usage{
			// opencode reports input net of cache, unlike Codex, so nothing
			// is subtracted.
			InputTokens: tin.Int64,
			// Reasoning is folded into output: opencode counts it apart from
			// output (its own total adds the two), while schema.Usage treats
			// reasoning as a subset of output and leaves it out of the total.
			OutputTokens:       tout.Int64 + treason.Int64,
			CacheReadTokens:    tcr.Int64,
			CacheWrite5mTokens: tcw.Int64,
			ReasoningTokens:    treason.Int64,
		}
		if usage.TotalTokens() == 0 {
			continue
		}

		// A response that never completed has no creation time recorded; the
		// row's own update time is then the only thing that places it.
		at := created.Int64
		if at == 0 {
			at = updated
		}

		ev := schema.Event{
			V:         schema.Version,
			ID:        schema.MakeID(schema.SourceOpenCode, id),
			NativeID:  id,
			Source:    schema.SourceOpenCode,
			Surface:   schema.SurfaceCLI,
			TS:        time.UnixMilli(at).UTC(),
			MachineID: c.MachineID,
			Provider:  provider.String,
			Model:     model.String,
			// Endpoint carries providerID so pricing keys on the provider that
			// actually served the tokens rather than on a model name that
			// several providers resell.
			Endpoint:   provider.String,
			Effort:     variant.String,
			Usage:      usage,
			SessionID:  sessionID,
			IsSubagent: subagent,
			// opencode talks to provider APIs with real keys, so this is
			// metered spend rather than seat usage.
			CostBasis: schema.CostBilled,
		}
		// opencode's figure, zero included: that a free-tier model cost
		// nothing is a fact it knows and we do not. A missing figure is not a
		// zero, though -- carried as one, it would claim that spend was free --
		// so the price table prices those.
		if cost.Valid {
			v := cost.Float64
			ev.NativeCostUSD = &v
		}
		c.emit(ev)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}

	// Staged, so the watermark commits in the same transaction as the rows it
	// covers: an interrupted pass keeps both or neither.
	if newWatermark > since {
		c.StageMeta("opencode:message_watermark", strconv.FormatInt(newWatermark, 10))
	}
	stored, err := c.CommitPending(ctx)
	res.Stored = stored
	return res, err
}

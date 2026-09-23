package sources

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/schema"
)

// codexLine is one rollout record.
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Ordinal   int64           `json:"ordinal"`
	Payload   json.RawMessage `json:"payload"`
}

type codexTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// key spells out every figure, so two readings share a key only when all of
// them agree.
func (u codexTokenUsage) key() string {
	return fmt.Sprintf("%d,%d,%d,%d,%d,%d", u.InputTokens, u.CachedInputTokens,
		u.CacheWriteInputTokens, u.OutputTokens, u.ReasoningOutputTokens, u.TotalTokens)
}

// codexUsageRecord is the newer per-response form, keyed on its response_id.
type codexUsageRecord struct {
	SessionID  string          `json:"session_id"`
	ResponseID string          `json:"response_id"`
	Usage      codexTokenUsage `json:"usage"`
}

type codexTurnContext struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
	// Newer builds carry the same value under the collaboration settings.
	CollaborationMode *struct {
		Settings *struct {
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
}

func (t codexTurnContext) effort() string {
	if t.Effort != "" {
		return t.Effort
	}
	if c := t.CollaborationMode; c != nil && c.Settings != nil {
		return c.Settings.ReasoningEffort
	}
	return ""
}

type codexSessionMeta struct {
	ID            string `json:"id"`
	SessionID     string `json:"session_id"`
	Originator    string `json:"originator"`
	CLIVersion    string `json:"cli_version"`
	ModelProvider string `json:"model_provider"`
	ThreadSource  string `json:"thread_source"`
}

type codexTokenCount struct {
	Type string `json:"type"`
	Info *struct {
		LastTokenUsage  codexTokenUsage  `json:"last_token_usage"`
		TotalTokenUsage *codexTokenUsage `json:"total_token_usage"`
	} `json:"info"`
}

// fileCtx is the per-file state a usage line needs but does not carry. The
// header and turn context arrive once near the top of a rollout, and a later
// pass can resume long past them, so it is persisted with the cursor.
type fileCtx struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Originator string `json:"originator"`
	Version    string `json:"version"`
	Subagent   bool   `json:"subagent"`
	Effort     string `json:"effort"`
	SessionID  string `json:"session_id"`
	// HeaderSeen marks the rollout's own session_meta as read. A fork or
	// subagent rollout carries a copy of its parent's header after its own,
	// and that copy must not relabel it.
	HeaderSeen bool `json:"header_seen"`
	// HasUsageRecord marks a rollout that reports through token_usage_record.
	// From its first record on, token_count lines restate the same usage.
	HasUsageRecord bool `json:"has_usage_record"`
}

func init() { Register(Codex{}) }

// Codex reads Codex CLI and desktop rollouts.
//
// Its history does not outlive our archive (no KeepsHistory): Codex can
// permanently delete a session and its rollout (0.156's thread store has a
// delete_thread), and what is gone from disk cannot be re-read.
type Codex struct{}

func (Codex) Name() schema.Source { return schema.SourceCodex }

// Roots lists archived_sessions first. A fork opens with a copy of its
// parent's token_count lines stamped at fork time, and the copy stored first
// keeps its date, so an archived parent must be read before its live fork.
func (Codex) Roots() []string { return []string{".codex/archived_sessions", ".codex/sessions"} }

// MetaPrefixes names the per-file context key, which predates the
// "<source>:" convention and cannot be renamed without stranding what
// existing stores hold under it.
func (Codex) MetaPrefixes() []string { return []string{"codexctx:"} }

func codexSurface(originator string) schema.Surface {
	o := strings.ToLower(originator)
	switch {
	case strings.Contains(o, "desktop"):
		return schema.SurfaceDesktop
	case strings.Contains(o, "vscode"), strings.Contains(o, "ide"):
		return schema.SurfaceIDE
	case o == "":
		return schema.SurfaceUnknown
	default:
		return schema.SurfaceCLI
	}
}

// codexEndpoint maps the header's model_provider onto a pricing endpoint.
// OpenAI, the default, needs none. Codex's local providers map onto the
// local-runtime ids pricing charges nothing for -- "oss" is the older name of
// its Ollama provider -- and any other provider the user configured passes
// through, so pricing tries it before the bare model name.
func codexEndpoint(provider string) string {
	switch p := strings.ToLower(provider); p {
	case "", "openai":
		return ""
	case "oss":
		return "ollama"
	default:
		return p
	}
}

// codexTokenCountID keys a token_count response on its usage: the thread's
// running total and the response's own figures. Codex restates a response in
// a second token_count, and a fork or subagent rollout opens with a copy of
// its parent's lines; both carry the same pair, so they collapse onto one id
// wherever they are read. Unrelated responses -- on any machine, since ids
// carry none -- rarely share a pair, and merge when they do.
func codexTokenCountID(total *codexTokenUsage, last codexTokenUsage, path string, ordinal int64) string {
	// Without a running total, usage alone could merge two responses of one
	// thread, so the line's place in the rollout identifies it instead.
	k := fmt.Sprintf("at|%s|%d|%s", filepath.Base(path), ordinal, last.key())
	if total != nil {
		k = total.key() + "|" + last.key()
	}
	sum := sha256.Sum256([]byte(k))
	return "tc:" + hex.EncodeToString(sum[:16])
}

func (a Codex) Collect(ctx context.Context, c *Ctx) (Result, error) {
	ctxs := map[string]*fileCtx{}

	return walkJSONL(ctx, c, AbsRoots(a, c), hasExt(".jsonl"), func(path string, at int64, line []byte) {
		fc, ok := ctxs[path]
		if !ok {
			v := loadFileCtx(ctx, c, path)
			fc, ctxs[path] = &v, &v
		}
		// State held for a rollout read from its first byte was left by
		// content since replaced: it would mislabel the new content, and a
		// stale HasUsageRecord drops every token_count response in it.
		if at == 0 && *fc != (fileCtx{}) {
			*fc = fileCtx{}
			stageFileCtx(c, path, *fc)
		}

		var l codexLine
		if json.Unmarshal(line, &l) != nil {
			c.Unparsed()
			return
		}
		ts := parseTS(l.Timestamp)

		switch l.Type {
		case "session_meta":
			var m codexSessionMeta
			if fc.HeaderSeen || json.Unmarshal(l.Payload, &m) != nil {
				return
			}
			fc.HeaderSeen = true
			fc.Originator, fc.Version = m.Originator, m.CLIVersion
			fc.Subagent = m.ThreadSource == "subagent"
			fc.Provider = m.ModelProvider
			// The session a thread belongs to, which is also what its
			// token_usage_record lines carry: a subagent shares its root's.
			fc.SessionID = cmp.Or(m.SessionID, m.ID)
			stageFileCtx(c, path, *fc)

		case "turn_context":
			var t codexTurnContext
			if json.Unmarshal(l.Payload, &t) == nil {
				if t.Model != "" {
					fc.Model = t.Model
				}
				if e := t.effort(); e != "" {
					fc.Effort = e
				}
				stageFileCtx(c, path, *fc)
			}

		case "token_usage_record":
			var r codexUsageRecord
			if json.Unmarshal(l.Payload, &r) != nil || r.ResponseID == "" {
				return
			}
			if !fc.HasUsageRecord {
				fc.HasUsageRecord = true
				stageFileCtx(c, path, *fc)
			}
			acct := c.AccountRefAt("openai", ts)
			if ev, ok := codexEvent(c, fc, acct, r.ResponseID, cmp.Or(r.SessionID, fc.SessionID), r.Usage, ts); ok {
				c.emit(ev)
			}

		case "event_msg":
			var tc codexTokenCount
			if json.Unmarshal(l.Payload, &tc) != nil || tc.Type != "token_count" {
				return
			}
			// last_token_usage is this response's own usage, not a running
			// total. Token_count lines before a rollout's first usage record
			// are responses an older CLI reported only this way.
			if fc.HasUsageRecord || tc.Info == nil {
				return
			}
			u := tc.Info.LastTokenUsage
			if u.InputTokens+u.OutputTokens == 0 {
				return
			}
			// Attributed by the line's own time: auth.json holds one account
			// and switching overwrites it.
			acct := c.AccountRefAt("openai", ts)
			nid := codexTokenCountID(tc.Info.TotalTokenUsage, u, path, l.Ordinal)
			if ev, ok := codexEvent(c, fc, acct, nid, fc.SessionID, u, ts); ok {
				c.emit(ev)
			}
		}
	})
}

func codexEvent(c *Ctx, fc *fileCtx, acct, nativeID, sessionID string, u codexTokenUsage, ts time.Time) (schema.Event, bool) {
	usage := schema.Usage{
		// Codex counts cached tokens inside input_tokens; left there they
		// would be charged at both the full and the cache rate.
		InputTokens:        max(u.InputTokens-u.CachedInputTokens, 0),
		OutputTokens:       u.OutputTokens,
		CacheReadTokens:    u.CachedInputTokens,
		CacheWrite5mTokens: u.CacheWriteInputTokens,
		ReasoningTokens:    u.ReasoningOutputTokens,
	}
	if usage.TotalTokens() == 0 {
		return schema.Event{}, false
	}

	provider := fc.Provider
	if provider == "" {
		provider = inferProvider(fc.Model)
	}
	// Usage can precede the first turn_context. "unknown" keeps it visible
	// and unpriced; a blank model renders as a bar nobody can read.
	model := fc.Model
	if model == "" {
		model = "unknown"
	}
	basis := schema.CostRateCard
	if acct == "" {
		basis = schema.CostUnknown
	}

	return schema.Event{
		V:            schema.Version,
		ID:           schema.MakeID(schema.SourceCodex, nativeID),
		NativeID:     nativeID,
		Source:       schema.SourceCodex,
		Surface:      codexSurface(fc.Originator),
		TS:           ts,
		MachineID:    c.MachineID,
		AccountRef:   acct,
		Provider:     provider,
		Model:        model,
		Endpoint:     codexEndpoint(fc.Provider),
		Usage:        usage,
		SessionID:    sessionID,
		IsSubagent:   fc.Subagent,
		CostBasis:    basis,
		Effort:       fc.Effort,
		AgentVersion: fc.Version,
	}, true
}

func loadFileCtx(ctx context.Context, c *Ctx, path string) fileCtx {
	var fc fileCtx
	if v, err := c.Store.Meta(ctx, "codexctx:"+path); err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &fc)
	}
	return fc
}

// stageFileCtx queues the file's parser state to commit in the same
// transaction as its rows and cursor. Committed apart, an interrupted pass
// resumes mid-file without it: token_count restatements of usage records are
// counted again, and a fork's copied header relabels the fork.
func stageFileCtx(c *Ctx, path string, fc fileCtx) {
	if b, err := json.Marshal(fc); err == nil {
		c.StageMeta("codexctx:"+path, string(b))
	}
}
